package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeTranscript(%s): %v", name, err)
	}
	return path
}

func TestCollectUsage_ParsesAssistantTurnsWithUsage(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "-home-user-Develop-project")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := `{"type":"user","message":{"role":"user"}}
{"type":"assistant","sessionId":"abc-123","timestamp":"2026-07-26T18:01:48.075Z","message":{"model":"claude-opus-4-8","usage":{"input_tokens":7597,"output_tokens":447,"cache_creation_input_tokens":5093,"cache_read_input_tokens":18125}}}
{"type":"assistant","sessionId":"abc-123","timestamp":"2026-07-26T18:05:00.000Z","message":{"model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`
	writeTranscript(t, projDir, "session1.jsonl", content)

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (the user-type line must be skipped)", len(entries))
	}

	first := entries[0]
	if first.SessionID != "abc-123" {
		t.Errorf("SessionID = %q, want abc-123", first.SessionID)
	}
	if first.Model != "claude-opus-4-8" {
		t.Errorf("Model = %q, want claude-opus-4-8", first.Model)
	}
	if first.InputTokens != 7597 || first.OutputTokens != 447 {
		t.Errorf("tokens = (%d in, %d out), want (7597, 447)", first.InputTokens, first.OutputTokens)
	}
	wantTime := time.Date(2026, 7, 26, 18, 1, 48, 75_000_000, time.UTC)
	if !first.Timestamp.Equal(wantTime) {
		t.Errorf("Timestamp = %v, want %v", first.Timestamp, wantTime)
	}
}

func TestCollectUsage_SkipsMalformedLinesWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := `not valid json at all
{"type":"assistant","sessionId":"ok","timestamp":"2026-07-26T00:00:00Z","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5}}}
`
	writeTranscript(t, projDir, "session.jsonl", content)

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1 (malformed line must be skipped, not fatal)", len(entries))
	}
}

func TestCollectUsage_MissingProjectsDirReturnsEmptyNotError(t *testing.T) {
	entries, err := CollectUsage(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("CollectUsage: %v, want nil error for a missing dir", err)
	}
	if entries != nil {
		t.Fatalf("entries = %v, want nil", entries)
	}
}

func TestCollectUsage_IgnoresNonJSONLFiles(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, projDir, "README.md", "not a transcript")

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0", len(entries))
	}
}

func TestProjectsDir(t *testing.T) {
	got := ProjectsDir("/home/user")
	want := filepath.Join("/home/user", ".claude", "projects")
	if got != want {
		t.Errorf("ProjectsDir = %q, want %q", got, want)
	}
}

func TestCollectUsage_CapsEntriesPerFile(t *testing.T) {
	old := maxEntriesPerFile
	maxEntriesPerFile = 3
	defer func() { maxEntriesPerFile = old }()

	dir := t.TempDir()
	projDir := filepath.Join(dir, "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"s","timestamp":"2026-07-26T18:01:48.075Z","message":{"model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	content := ""
	for i := 0; i < 10; i++ {
		content += line
	}
	writeTranscript(t, projDir, "huge.jsonl", content)

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3 (capped, not OOM)", len(entries))
	}
}

// Claude Code writes a subagent's turns under the SAME sessionId as the main
// thread, tagged with an agentId, and files them in a subdirectory. Each subagent
// carries its own independent context, so anything measuring how context grows
// needs to tell the streams apart (see internal/contextcurve).
func TestCollectUsage_MarksTheThreadWhenTheTurnCameFromASubagent(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "-home-user-Develop-project")
	subDir := filepath.Join(projDir, "abc-123", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTranscript(t, projDir, "abc-123.jsonl",
		`{"type":"assistant","sessionId":"abc-123","timestamp":"2026-07-26T18:01:48.075Z","message":{"model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":900000}}}`+"\n")
	writeTranscript(t, subDir, "agent-a008eea358b45ba7a.jsonl",
		`{"type":"assistant","sessionId":"abc-123","agentId":"agent-a008eea358b45ba7a","timestamp":"2026-07-26T18:02:00.000Z","message":{"model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":40000}}}`+"\n")

	entries, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}

	byThread := map[string]int{}
	for _, e := range entries {
		if e.SessionID != "abc-123" {
			t.Errorf("SessionID = %q, want abc-123 for both threads", e.SessionID)
		}
		byThread[e.ThreadID] = e.CacheReadInputTokens
	}
	if byThread[""] != 900000 {
		t.Errorf("main-thread cache read = %d, want 900000 (empty ThreadID means the main thread)", byThread[""])
	}
	if byThread["agent-a008eea358b45ba7a"] != 40000 {
		t.Errorf("subagent cache read = %d, want 40000; the agentId must reach ThreadID",
			byThread["agent-a008eea358b45ba7a"])
	}
}
