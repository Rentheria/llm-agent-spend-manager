package openclaw

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testdataAgentsDir points at a synthetic fixture tree mirroring the on-disk
// layout OpenClaw writes (~/.openclaw/agents/<agentId>/sessions/*.jsonl). The
// active session file contains, in order: a session line, a user turn, a
// claude-opus-4-8 turn, a delivery-mirror echo, and a gemini turn. Alongside it
// live a trajectory trace and a reset archive that must both be ignored.
const testdataAgentsDir = "testdata/agents"

func TestCollectUsage_ParsesAssistantTurnsFromRealLayout(t *testing.T) {
	entries, err := CollectUsage(testdataAgentsDir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}

	// Expected survivors: the claude turn and the gemini turn. Skipped: the
	// session line, the user turn, the delivery-mirror echo, every line of the
	// trajectory trace, and every line of the reset archive.
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (claude + gemini; delivery-mirror, trajectory and reset excluded)", len(entries))
	}

	claude := entries[0]
	if claude.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("SessionID = %q, want it derived from the file name", claude.SessionID)
	}
	if claude.Model != "claude-opus-4-8" {
		t.Errorf("Model = %q, want claude-opus-4-8", claude.Model)
	}
	if claude.InputTokens != 7597 || claude.OutputTokens != 447 {
		t.Errorf("tokens = (%d in, %d out), want (7597, 447)", claude.InputTokens, claude.OutputTokens)
	}
	// OpenClaw's short keys map onto the Claude Code bucket names: cacheWrite ->
	// cache creation, cacheRead -> cache read.
	if claude.CacheCreationInputTokens != 5093 || claude.CacheReadInputTokens != 18125 {
		t.Errorf("cache tokens = (%d write, %d read), want (5093, 18125)",
			claude.CacheCreationInputTokens, claude.CacheReadInputTokens)
	}
	wantTime := time.Date(2026, 1, 15, 18, 1, 48, 75_000_000, time.UTC)
	if !claude.Timestamp.Equal(wantTime) {
		t.Errorf("Timestamp = %v, want %v (the outer line timestamp)", claude.Timestamp, wantTime)
	}
}

func TestCollectUsage_SkipsDeliveryMirrorButKeepsTheGeminiFallback(t *testing.T) {
	entries, err := CollectUsage(testdataAgentsDir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}

	for _, e := range entries {
		if e.Model == deliveryMirrorModel {
			t.Fatalf("delivery-mirror turn leaked into entries: %+v", e)
		}
	}

	// The gemini turn is the agent falling back to the google-gemini-cli backend
	// after its Anthropic chain failed: a real conversation turn, and since A6 a
	// priced one (1200 in @ $2/MTok + 80 out @ $12/MTok = $0.00336).
	var gemini *Entry
	for i := range entries {
		if entries[i].Model == "gemini-3.1-pro-preview" {
			gemini = &entries[i]
		}
	}
	if gemini == nil {
		t.Fatal("gemini turn missing — a real turn must be kept whatever its model")
	}
	cost, known := EstimateCostUSD(*gemini)
	if !known {
		t.Fatal("known = false for gemini-3.1-pro-preview, want true since A6 added its list price")
	}
	if want := 0.00336; math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}

func TestCollectUsage_KeepsARealTurnWhoseModelHasNoPrice(t *testing.T) {
	// Collection and pricing are separate concerns: a model nobody has priced yet
	// still produced real tokens, so the turn must survive the scan and let
	// pricing report known=false. Dropping it here would hide the measurement gap
	// that advise's E-05 exists to surface.
	dir := t.TempDir()
	sessions := filepath.Join(dir, "main", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"message","timestamp":"2026-07-26T00:00:00Z","message":{"role":"assistant",` +
		`"model":"modelo-que-nadie-ha-tarifado","usage":{"input":100,"output":20,"cacheRead":0,"cacheWrite":0}}}` + "\n"
	path := filepath.Join(sessions, "33333333-3333-3333-3333-333333333333.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1: an unpriced turn is still a real turn", len(entries))
	}
	if _, known := EstimateCostUSD(entries[0]); known {
		t.Error("known = true for a model absent from the table, want false")
	}
}

func TestCollectUsage_MissingAgentsDirReturnsEmptyNotError(t *testing.T) {
	entries, err := CollectUsage(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("CollectUsage: %v, want nil error for a missing dir", err)
	}
	if entries != nil {
		t.Fatalf("entries = %v, want nil", entries)
	}
}

func TestCollectUsage_SkipsMalformedLinesWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "main", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "not valid json at all\n" +
		`{"type":"message","timestamp":"2026-07-26T00:00:00Z","message":{"role":"assistant","model":"claude-haiku-4-5","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessions, "s.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1 (malformed line skipped, not fatal)", len(entries))
	}
}

func TestIsActiveSessionFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"0e140d79.jsonl", true},
		{"0e140d79.trajectory.jsonl", false},
		{"2909ae6c.jsonl.reset.2026-07-23T22-10-36.673Z", false},
		{"29ec1705.jsonl.deleted.2026-07-26T04-14-00.174Z", false},
		{"0e140d79.trajectory-path.json", false},
	}
	for _, c := range cases {
		if got := isActiveSessionFile(c.name); got != c.want {
			t.Errorf("isActiveSessionFile(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAgentsDir(t *testing.T) {
	got := AgentsDir("/home/user")
	want := filepath.Join("/home/user", ".openclaw", "agents")
	if got != want {
		t.Errorf("AgentsDir = %q, want %q", got, want)
	}
}

// assistantTurn renders one usage-bearing line as OpenClaw writes it: the event
// id lives at the TOP level, not under message.
func assistantTurn(eventID string, input int) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"timestamp":"2026-08-04T23:55:06.000Z",`+
		`"message":{"role":"assistant","model":"claude-opus-4-8",`+
		`"usage":{"input":%d,"output":10,"cacheRead":0,"cacheWrite":0}}}`, eventID, input)
}

// writeSession drops one transcript into an agents tree and returns the tree.
func writeSession(t *testing.T, agentsDir, name string, turns ...string) string {
	t.Helper()
	sessions := filepath.Join(agentsDir, "main", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(turns, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(sessions, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return agentsDir
}

func TestCollectUsage_CountsARepeatedTurnOnceAndKeepsTheFullestTranscript(t *testing.T) {
	// The real shape of H-01: OpenClaw snapshots a live conversation as it grows,
	// so its files nest — the plain uuid holds the first turn, each later snapshot
	// holds everything before it plus what came after.
	dir := t.TempDir()
	writeSession(t, dir, "d49696e2-1e1b-49e2-9624-99d5c95426a3.jsonl",
		assistantTurn("turn-1", 100))
	writeSession(t, dir, "2026-07-27T23-51-17-489Z_6e13b7dc.jsonl",
		assistantTurn("turn-1", 100), assistantTurn("turn-2", 200))
	writeSession(t, dir, "2026-07-27T23-58-31-095Z_f0af5ff9.jsonl",
		assistantTurn("turn-1", 100), assistantTurn("turn-2", 200), assistantTurn("turn-3", 300))

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3: six records on disk are three real turns", len(entries))
	}
	if got := totalInput(entries); got != 600 {
		t.Errorf("input tokens = %d, want 600 (1200 would be the H-01 double count)", got)
	}
	// All three turns belong to the transcript that holds the whole thread —
	// otherwise the conversation reads as three sessions and its context curve
	// gets cut into pieces.
	const fullest = "2026-07-27T23-58-31-095Z_f0af5ff9"
	for _, e := range entries {
		if e.SessionID != fullest {
			t.Errorf("SessionID = %q, want %q (the transcript with the complete conversation)", e.SessionID, fullest)
		}
	}
}

func TestCollectUsage_RealConversationOutranksTheGatewayFallbackCopy(t *testing.T) {
	// Both transcripts hold exactly one turn, so size can't decide: the tie-break
	// has to, and the fallback file is a side-record of a call that already lives
	// in the conversation that produced it.
	dir := t.TempDir()
	writeSession(t, dir, "gateway-fallback-2670c9c4.jsonl", assistantTurn("turn-1", 100))
	writeSession(t, dir, "2026-08-04T23-55-09-957Z_2e145a76.jsonl", assistantTurn("turn-1", 100))

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if want := "2026-08-04T23-55-09-957Z_2e145a76"; entries[0].SessionID != want {
		t.Errorf("SessionID = %q, want %q (not the gateway-fallback side-record)", entries[0].SessionID, want)
	}
}

func TestCollectUsage_NewestSnapshotWinsWhenTheyAreTheSameSize(t *testing.T) {
	// Three snapshots taken seconds apart with no turn in between (real case:
	// 2026-07-26T23-41/42/43, 35 turns each). Nothing distinguishes them but the
	// timestamp in the name, which sorts chronologically.
	dir := t.TempDir()
	for _, name := range []string{
		"2026-07-26T23-41-40-703Z_cbeb3d1d.jsonl",
		"2026-07-26T23-43-16-870Z_ebb46f78.jsonl",
		"2026-07-26T23-42-27-945Z_a31bdd08.jsonl",
	} {
		writeSession(t, dir, name, assistantTurn("turn-1", 100))
	}

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if want := "2026-07-26T23-43-16-870Z_ebb46f78"; entries[0].SessionID != want {
		t.Errorf("SessionID = %q, want %q (the newest of three equal snapshots)", entries[0].SessionID, want)
	}
}

func TestCollectUsage_NeverDropsATurnThatCarriesNoEventID(t *testing.T) {
	// A transcript older than the id field can't be deduplicated. Counting such a
	// turn twice is a visible error; deleting it is an invisible one, so it stays.
	dir := t.TempDir()
	noID := `{"type":"message","timestamp":"2026-07-26T00:00:00Z","message":{"role":"assistant",` +
		`"model":"claude-opus-4-8","usage":{"input":100,"output":10,"cacheRead":0,"cacheWrite":0}}}`
	writeSession(t, dir, "aaaaaaaa-0000-0000-0000-000000000000.jsonl", noID)
	writeSession(t, dir, "bbbbbbbb-0000-0000-0000-000000000000.jsonl", noID)

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2: an un-identifiable turn is kept, not guessed away", len(entries))
	}
}

func TestCollectUsage_KeepsDistinctTurnsThatHappenToShareTokenCounts(t *testing.T) {
	// Deduplication keys on the id OpenClaw assigned, not on the token counts:
	// two different calls with identical usage are two calls.
	dir := t.TempDir()
	writeSession(t, dir, "cccccccc-0000-0000-0000-000000000000.jsonl",
		assistantTurn("turn-1", 100), assistantTurn("turn-2", 100))

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (same usage, different turns)", len(entries))
	}
}

func totalInput(entries []Entry) int {
	sum := 0
	for _, e := range entries {
		sum += e.InputTokens
	}
	return sum
}

func TestCollectUsage_CapsEntriesPerFile(t *testing.T) {
	old := maxEntriesPerFile
	maxEntriesPerFile = 2
	defer func() { maxEntriesPerFile = old }()

	dir := t.TempDir()
	sessDir := filepath.Join(dir, "agent1", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"message","timestamp":"2026-07-26T18:01:48.075Z","message":{"role":"assistant","model":"claude-opus-4-8","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0}}}` + "\n"
	content := ""
	for i := 0; i < 8; i++ {
		content += line
	}
	if err := os.WriteFile(filepath.Join(sessDir, "11111111-1111-1111-1111-111111111111.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (capped, not OOM)", len(entries))
	}
}
