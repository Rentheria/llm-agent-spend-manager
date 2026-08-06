package aggregate

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/antigravity"
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/openclaw"
	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
	_ "modernc.org/sqlite"
)

func TestFromClaudeCode_NormalizesAndPrices(t *testing.T) {
	ts := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	entries := []claudecode.Entry{{
		SessionID:                "s1",
		Timestamp:                ts,
		Model:                    "claude-opus-4-8",
		InputTokens:              1000,
		OutputTokens:             200,
		CacheCreationInputTokens: 50,
		CacheReadInputTokens:     4000,
	}}

	recs := FromClaudeCode(entries)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Agent != AgentClaudeCode {
		t.Errorf("agent = %q, want %q", r.Agent, AgentClaudeCode)
	}
	if r.CacheWrite != 50 || r.CacheRead != 4000 {
		t.Errorf("cache buckets mismapped: write=%d read=%d", r.CacheWrite, r.CacheRead)
	}
	wantCost, wantKnown := pricing.EstimateUSD("claude-opus-4-8", 1000, 200, 50, 4000)
	if !r.CostKnown || !wantKnown {
		t.Fatalf("expected known cost")
	}
	if r.CostUSD != wantCost {
		t.Errorf("cost = %v, want %v", r.CostUSD, wantCost)
	}
	if got := r.TotalTokens(); got != 5250 {
		t.Errorf("total tokens = %d, want 5250", got)
	}
}

func TestFromOpenClaw_MapsCacheWriteFromCacheCreation(t *testing.T) {
	entries := []openclaw.Entry{{
		SessionID:                "u1",
		Timestamp:                time.Now(),
		Model:                    "claude-sonnet-5",
		InputTokens:              10,
		OutputTokens:             20,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
	}}
	recs := FromOpenClaw(entries)
	if len(recs) != 1 || recs[0].Agent != AgentOpenClaw {
		t.Fatalf("bad conversion: %+v", recs)
	}
	if recs[0].CacheWrite != 30 || recs[0].CacheRead != 40 {
		t.Errorf("cache buckets mismapped: %+v", recs[0])
	}
}

func TestFromOpenClaw_TagsModeInteractive(t *testing.T) {
	recs := FromOpenClaw([]openclaw.Entry{{Model: "claude-sonnet-5", InputTokens: 1}})
	if recs[0].Mode != ModeInteractive {
		t.Errorf("Mode = %q, want %q", recs[0].Mode, ModeInteractive)
	}
}

func TestFromOpenClawCron_TagsModeCron(t *testing.T) {
	recs := FromOpenClawCron([]openclaw.Entry{{
		SessionID:            "agent:main:cron:job:run:1",
		Model:                "claude-opus-4-8",
		CacheReadInputTokens: 1000,
	}})
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].Mode != ModeCron {
		t.Errorf("Mode = %q, want %q", recs[0].Mode, ModeCron)
	}
	if recs[0].Agent != AgentOpenClaw {
		t.Errorf("Agent = %q, want %q (cron runs still belong to OpenClaw)", recs[0].Agent, AgentOpenClaw)
	}
}

func TestByMode_SplitsInteractiveAndCron(t *testing.T) {
	recs := []Record{
		{Mode: ModeInteractive, Input: 100, CostUSD: 1.0, CostKnown: true},
		{Mode: ModeInteractive, Input: 50, CostUSD: 0.5, CostKnown: true},
		{Mode: ModeCron, CacheRead: 1000, CostUSD: 0.2, CostKnown: true},
	}
	byMode := ByMode(recs)
	if len(byMode) != 2 {
		t.Fatalf("want 2 modes, got %d", len(byMode))
	}
	// Sorted by mode key: "cron" before "interactive".
	if byMode[0].Mode != ModeCron || byMode[0].Totals.Turns != 1 {
		t.Errorf("first mode = %+v, want cron with 1 turn", byMode[0])
	}
	if byMode[1].Mode != ModeInteractive || byMode[1].Totals.Turns != 2 {
		t.Errorf("second mode = %+v, want interactive with 2 turns", byMode[1])
	}
	if byMode[1].Totals.CostUSD != 1.5 {
		t.Errorf("interactive cost = %v, want 1.5", byMode[1].Totals.CostUSD)
	}
}

func TestFromAntigravity_ActivityTierTokensOnly(t *testing.T) {
	recs := FromAntigravity([]antigravity.Entry{{
		ConversationID: "conv-1", Steps: 10, TokensLow: 8000, TokensHigh: 60000,
	}})
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Agent != AgentAntigravity || r.Mode != ModeEditor {
		t.Errorf("agent/mode = %q/%q, want %q/editor", r.Agent, r.Mode, AgentAntigravity)
	}
	if r.Confidence != ConfidenceActivity {
		t.Errorf("Confidence = %q, want activity", r.Confidence)
	}
	if r.CostKnown {
		t.Error("CostKnown = true, want false (Antigravity model unknown → tokens only)")
	}
	if r.TokensLow != 8000 || r.TokensHigh != 60000 {
		t.Errorf("range = (%d, %d), want (8000, 60000)", r.TokensLow, r.TokensHigh)
	}
	// PointTokens is the midpoint for activity records.
	if got := r.PointTokens(); got != 34000 {
		t.Errorf("PointTokens = %d, want 34000 (midpoint)", got)
	}
}

func TestFromClaudeCode_UnknownModelHasNoCost(t *testing.T) {
	recs := FromClaudeCode([]claudecode.Entry{{Model: "gpt-imaginary", InputTokens: 5, OutputTokens: 5}})
	if recs[0].CostKnown {
		t.Errorf("unknown model should not be priced")
	}
	if recs[0].CostUSD != 0 {
		t.Errorf("unknown model cost = %v, want 0", recs[0].CostUSD)
	}
}

func TestStartOfWindow(t *testing.T) {
	// A Wednesday at 15:30 local.
	now := time.Date(2026, 7, 22, 15, 30, 0, 0, time.UTC)

	today := StartOfWindow(WindowToday, now)
	if !today.Equal(time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("today start = %v", today)
	}

	week := StartOfWindow(WindowWeek, now)
	if week.Weekday() != time.Monday {
		t.Errorf("week start weekday = %v, want Monday", week.Weekday())
	}
	if week.After(now) || now.Sub(week) >= 7*24*time.Hour {
		t.Errorf("week start %v not within the 7 days before %v", week, now)
	}
	if h, m, s := week.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("week start not at midnight: %v", week)
	}
	// 2026-07-22 is a Wednesday, so Monday is the 20th.
	if !week.Equal(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("week start = %v, want 2026-07-20", week)
	}

	if !StartOfWindow(WindowAll, now).IsZero() {
		t.Errorf("all window should have zero start")
	}
}

func TestFilterWindow(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	recs := []Record{
		{Agent: "a", Timestamp: now.Add(-1 * time.Hour)},       // today
		{Agent: "b", Timestamp: now.Add(-25 * time.Hour)},      // yesterday
		{Agent: "c", Timestamp: now.Add(-5 * 24 * time.Hour)},  // last week-ish
		{Agent: "d", Timestamp: now.Add(-40 * 24 * time.Hour)}, // long ago
	}

	if got := len(FilterWindow(recs, WindowToday, now)); got != 1 {
		t.Errorf("today count = %d, want 1", got)
	}
	// Monday 2026-07-20: today(-1h), yesterday(21st) both in week; 5 days ago = 17th (prev week) out.
	if got := len(FilterWindow(recs, WindowWeek, now)); got != 2 {
		t.Errorf("week count = %d, want 2", got)
	}
	if got := len(FilterWindow(recs, WindowAll, now)); got != 4 {
		t.Errorf("all count = %d, want 4", got)
	}
}

func TestByAgent(t *testing.T) {
	recs := []Record{
		{Agent: AgentOpenClaw, Input: 100, CostUSD: 0.5, CostKnown: true},
		{Agent: AgentClaudeCode, Input: 10, CostUSD: 0.1, CostKnown: true},
		{Agent: AgentClaudeCode, Output: 20, CostUSD: 0.2, CostKnown: true},
		{Agent: AgentClaudeCode, Input: 5, CostKnown: false}, // unpriced
	}
	got := ByAgent(recs)
	if len(got) != 2 {
		t.Fatalf("want 2 agents, got %d", len(got))
	}
	// Sorted by name: "Claude Code" < "OpenClaw".
	if got[0].Agent != AgentClaudeCode {
		t.Errorf("first agent = %q, want %q", got[0].Agent, AgentClaudeCode)
	}
	k := got[0].Totals
	if k.Turns != 3 || k.UnpricedTurns != 1 {
		t.Errorf("claude code turns=%d unpriced=%d, want 3/1", k.Turns, k.UnpricedTurns)
	}
	if !approxEqual(k.CostUSD, 0.3) {
		t.Errorf("claude code cost = %v, want ~0.3", k.CostUSD)
	}
	if k.TotalTokens != 35 {
		t.Errorf("claude code tokens = %d, want 35", k.TotalTokens)
	}
}

func TestByAgentDay(t *testing.T) {
	loc := time.UTC
	recs := []Record{
		{Agent: AgentClaudeCode, Timestamp: time.Date(2026, 7, 26, 9, 0, 0, 0, loc), Input: 1, CostUSD: 0.1, CostKnown: true},
		{Agent: AgentClaudeCode, Timestamp: time.Date(2026, 7, 26, 23, 0, 0, 0, loc), Input: 1, CostUSD: 0.1, CostKnown: true},
		{Agent: AgentClaudeCode, Timestamp: time.Date(2026, 7, 25, 9, 0, 0, 0, loc), Input: 1, CostUSD: 0.1, CostKnown: true},
	}
	got := ByAgentDay(recs, loc)
	if len(got) != 2 {
		t.Fatalf("want 2 agent-days, got %d", len(got))
	}
	// Newest day first.
	if got[0].Day != "2026-07-26" {
		t.Errorf("first day = %q, want 2026-07-26", got[0].Day)
	}
	if got[0].Totals.Turns != 2 {
		t.Errorf("2026-07-26 turns = %d, want 2", got[0].Totals.Turns)
	}
}

func TestGrand(t *testing.T) {
	recs := []Record{
		{Input: 100, CostUSD: 0.5, CostKnown: true},
		{Output: 50, CostUSD: 0.25, CostKnown: true},
		{Input: 1, CostKnown: false},
	}
	g := Grand(recs)
	if g.Turns != 3 || g.UnpricedTurns != 1 {
		t.Errorf("turns=%d unpriced=%d, want 3/1", g.Turns, g.UnpricedTurns)
	}
	if !approxEqual(g.CostUSD, 0.75) {
		t.Errorf("grand cost = %v, want ~0.75", g.CostUSD)
	}
}

func approxEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	return d < eps && d > -eps
}

func TestCollect_FixtureTree(t *testing.T) {
	home := t.TempDir()

	// Claude Code fixture: ~/.claude/projects/proj/session.jsonl
	ccDir := filepath.Join(home, ".claude", "projects", "proj")
	mustMkdir(t, ccDir)
	ccLine := `{"type":"assistant","sessionId":"k1","timestamp":"2026-07-26T10:00:00Z","message":{"model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	mustWrite(t, filepath.Join(ccDir, "session.jsonl"), ccLine)

	// OpenClaw fixture: ~/.openclaw/agents/main/sessions/<uuid>.jsonl
	ocDir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	mustMkdir(t, ocDir)
	ocLine := `{"type":"message","timestamp":"2026-07-26T11:00:00Z","message":{"role":"assistant","model":"claude-sonnet-5","usage":{"input":200,"output":80,"cacheRead":10,"cacheWrite":5}}}` + "\n"
	mustWrite(t, filepath.Join(ocDir, "abc.jsonl"), ocLine)

	recs, err := Collect(home)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records (1 per agent), got %d: %+v", len(recs), recs)
	}
	byAgent := ByAgent(recs)
	if len(byAgent) != 2 {
		t.Fatalf("want 2 agents, got %d", len(byAgent))
	}
}

func TestCollect_IncludesCronUsage(t *testing.T) {
	home := t.TempDir()

	// Interactive OpenClaw session.
	ocDir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	mustMkdir(t, ocDir)
	ocLine := `{"type":"message","timestamp":"2026-07-26T11:00:00Z","message":{"role":"assistant","model":"claude-sonnet-5","usage":{"input":200,"output":80,"cacheRead":10,"cacheWrite":5}}}` + "\n"
	mustWrite(t, filepath.Join(ocDir, "abc.jsonl"), ocLine)

	// Cron usage in the state DB — a separate source from the JSONL.
	stateDir := filepath.Join(home, ".openclaw", "state")
	mustMkdir(t, stateDir)
	writeCronDB(t, filepath.Join(stateDir, "openclaw.sqlite"))

	recs, err := Collect(home)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records (1 interactive + 1 cron), got %d: %+v", len(recs), recs)
	}

	byMode := ByMode(recs)
	if len(byMode) != 2 {
		t.Fatalf("want 2 modes, got %d: %+v", len(byMode), byMode)
	}
	// The cron record's total_tokens (booked as cache-read) must show up as OpenClaw
	// spend that the JSONL scan alone would have missed.
	var cron *ModeTotals
	for i := range byMode {
		if byMode[i].Mode == ModeCron {
			cron = &byMode[i]
		}
	}
	if cron == nil {
		t.Fatal("cron mode missing — Collect did not read the state DB")
	}
	if cron.Totals.TotalTokens != 65943 {
		t.Errorf("cron total tokens = %d, want 65943", cron.Totals.TotalTokens)
	}
}

// writeCronDB creates a minimal cron_run_logs DB at path with one model run.
func writeCronDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open cron db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cron_run_logs (
		ts INTEGER NOT NULL, model TEXT, total_tokens INTEGER, session_key TEXT
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO cron_run_logs (ts, model, total_tokens, session_key) VALUES (?, ?, ?, ?)`,
		1_782_695_950_889, "claude-opus-4-8", 65943, "agent:main:cron:job:run:1",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func TestCollect_MissingHomeIsNotError(t *testing.T) {
	recs, err := Collect(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("missing dirs should not error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("want 0 records, got %d", len(recs))
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFromClaudeCode_AtribuyeAOpenClawLasSesionesDeSuWorkspace(t *testing.T) {
	// OpenClaw maneja el MISMO CLI a través de su gateway, así que sus turnos
	// quedan escritos en los transcripts de Claude Code igual que los de una
	// sesion interactiva. Sin esta regla, el costo de OpenClaw se le cobra a
	// Claude Code.
	ts := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	base := claudecode.Entry{Timestamp: ts, Model: "claude-opus-5", InputTokens: 100, OutputTokens: 50}

	deOpenClaw := base
	deOpenClaw.SessionID = "s-oc"
	deOpenClaw.CWD = "/home/user/.openclaw/workspace"

	deClaudeCode := base
	deClaudeCode.SessionID = "s-cc"
	deClaudeCode.CWD = "/home/user/Develop/algun-repo"

	recs := FromClaudeCode([]claudecode.Entry{deOpenClaw, deClaudeCode})
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].Agent != AgentOpenClaw {
		t.Errorf("sesión con cwd del workspace de OpenClaw = %q, want %q", recs[0].Agent, AgentOpenClaw)
	}
	if recs[1].Agent != AgentClaudeCode {
		t.Errorf("sesión de un repo cualquiera = %q, want %q", recs[1].Agent, AgentClaudeCode)
	}
}

func TestFromClaudeCode_SinCWDSeQuedaEnClaudeCode(t *testing.T) {
	// Un transcript viejo o parcial puede no traer cwd. Ante la duda se queda con
	// el default en vez de inventar una atribución.
	recs := FromClaudeCode([]claudecode.Entry{{SessionID: "s", Model: "claude-opus-5", InputTokens: 10}})
	if len(recs) != 1 || recs[0].Agent != AgentClaudeCode {
		t.Errorf("sin cwd debería quedarse en %q, got %+v", AgentClaudeCode, recs)
	}
}

func TestDedupeAgainst_QuitaLaMismaLlamadaContadaDosVeces(t *testing.T) {
	// OpenClaw registra algunos de sus turnos Y ademas maneja el CLI de Claude,
	// cuyos transcripts ya leyo el adaptador claudecode: la misma llamada cae en las dos
	// fuentes. Medido 2026-07-27: 193 turnos en esa situacion.
	ts := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	delCLI := Record{Agent: AgentOpenClaw, Timestamp: ts, Input: 100, Output: 50, CacheRead: 9000}

	mismaLlamada := delCLI
	mismaLlamada.SessionID = "otra-fuente" // misma llamada, otro archivo
	otraDistinta := Record{Agent: AgentOpenClaw, Timestamp: ts, Input: 7, Output: 3, CacheRead: 11}

	out := dedupeAgainst([]Record{mismaLlamada, otraDistinta}, []Record{delCLI})

	if len(out) != 1 {
		t.Fatalf("want 1 record tras deduplicar, got %d", len(out))
	}
	if out[0].Input != 7 {
		t.Errorf("se quedo el registro equivocado: %+v", out[0])
	}
}

func TestDedupeAgainst_NoColapsaTurnosSinTokens(t *testing.T) {
	// Dos registros sin tokens tienen huella identica (todo ceros) sin ser la
	// misma llamada: deduplicarlos borraria datos buenos.
	vacio1 := Record{Agent: AgentOpenClaw, SessionID: "a"}
	vacio2 := Record{Agent: AgentOpenClaw, SessionID: "b"}

	out := dedupeAgainst([]Record{vacio1, vacio2}, []Record{vacio1})

	if len(out) != 2 {
		t.Errorf("want 2 (no se deduplican turnos sin tokens), got %d", len(out))
	}
}

func TestByModel_RollsUpHeaviestFirst(t *testing.T) {
	recs := []Record{
		{Model: "claude-sonnet-5", Output: 100, Confidence: ConfidenceMeasured},
		{Model: "claude-opus-5", Output: 300, Confidence: ConfidenceMeasured},
		{Model: "claude-opus-5", Output: 200, Confidence: ConfidenceMeasured},
	}

	got := ByModel(recs)
	if len(got) != 2 {
		t.Fatalf("models = %d, want 2", len(got))
	}
	if got[0].Model != "claude-opus-5" || got[0].Totals.TotalTokens != 500 || got[0].Totals.Turns != 2 {
		t.Errorf("first model = %+v, want opus-5 with 500 tokens over 2 turns", got[0])
	}
	if got[1].Model != "claude-sonnet-5" {
		t.Errorf("second model = %q, want the lighter one last", got[1].Model)
	}
}

// The workspace is what separates the Telegram conversation from the code work
// when asking where the quota went, so it must survive normalization.
func TestByWorkspace_SeparatesTheChatFromTheWork(t *testing.T) {
	recs := []Record{
		{Agent: AgentOpenClaw, Workspace: "/home/user/.openclaw/workspace", Output: 900, Confidence: ConfidenceMeasured},
		{Agent: AgentClaudeCode, Workspace: "/home/user/Develop/repo", Output: 100, Confidence: ConfidenceMeasured},
		// No workspace to attribute: skipped rather than bucketed under "".
		{Agent: AgentCursor, Output: 5000, Confidence: ConfidenceMeasured},
	}

	got := ByWorkspace(recs)
	if len(got) != 2 {
		t.Fatalf("workspaces = %d, want 2 (the one with no directory is skipped)", len(got))
	}
	if got[0].Workspace != "/home/user/.openclaw/workspace" || got[0].Agent != AgentOpenClaw {
		t.Errorf("first workspace = %+v, want the Telegram one", got[0])
	}
}

func TestFromClaudeCode_CarriesTheWorkingDirectory(t *testing.T) {
	recs := FromClaudeCode([]claudecode.Entry{{SessionID: "s1", CWD: "/home/user/Develop/repo", Timestamp: time.Now()}})
	if len(recs) != 1 || recs[0].Workspace != "/home/user/Develop/repo" {
		t.Errorf("workspace = %q, want the entry's cwd", recs[0].Workspace)
	}
}

func TestAdd_ReturnsANewValueInsteadOfSharingAnAccumulator(t *testing.T) {
	base := Totals{}
	one := Add(base, Record{Output: 10, Confidence: ConfidenceMeasured})
	two := Add(base, Record{Output: 20, Confidence: ConfidenceMeasured})

	if base.Turns != 0 {
		t.Error("Add mutated the value it was given")
	}
	if one.TotalTokens != 10 || two.TotalTokens != 20 {
		t.Errorf("Add shared state between buckets: %d / %d", one.TotalTokens, two.TotalTokens)
	}
}
