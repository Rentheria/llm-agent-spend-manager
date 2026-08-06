package advise

import (
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
)

// contextTurn builds one measured turn carrying a given context, priced the way
// the adapters price it. minute orders the turn inside its session.
func contextTurn(session, thread, label string, minute, context int) aggregate.Record {
	r := aggregate.Record{
		Agent:        aggregate.AgentClaudeCode,
		Mode:         aggregate.ModeInteractive,
		SessionID:    session,
		SessionLabel: label,
		ThreadID:     thread,
		Timestamp:    time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute),
		Model:        "claude-opus-5",
		Output:       100,
		CacheRead:    context,
		Confidence:   aggregate.ConfidenceMeasured,
	}
	r.CostUSD, r.CostKnown = pricing.EstimateUSD(r.Model, r.Input, r.Output, r.CacheWrite, r.CacheRead)
	total := r.TotalTokens()
	r.TokensLow, r.TokensHigh = total, total
	r.CostLowUSD, r.CostHighUSD = r.CostUSD, r.CostUSD
	return r
}

// growingSession builds one thread whose context climbs every turn — the shape
// that makes carrying it eventually cost more than restarting.
func growingSession(session, thread, label string, turns, baseline, growth, startMinute, minuteStep int) []aggregate.Record {
	out := make([]aggregate.Record, 0, turns)
	for i := 0; i < turns; i++ {
		out = append(out, contextTurn(session, thread, label, startMinute+i*minuteStep, baseline+growth*i))
	}
	return out
}

func riskFor(report Report, sessionID string) (ContextRisk, bool) {
	for _, r := range report.ContextRisks {
		if r.Curve.SessionID == sessionID {
			return r, true
		}
	}
	return ContextRisk{}, false
}

// The measurement has to follow each context stream separately. One session can
// run a main thread and several subagents at once; their turns interleave in the
// transcript but their contexts are independent, so measuring the session as one
// stream reads every hand-off as a reset and finds no curve at all.
func TestAnalyze_MeasuresContextPerThreadNotPerSession(t *testing.T) {
	const mainTurns = 300
	var records []aggregate.Record
	// Main thread on the even minutes, a subagent on the odd ones: interleaved in
	// time, exactly as they land in a real transcript.
	records = append(records, growingSession("s1", "", "tarea larga", mainTurns, 60_000, 2_000, 0, 2)...)
	for i := 0; i < mainTurns; i++ {
		records = append(records, contextTurn("s1", "agent-sub", "tarea larga", 1+i*2, 8_000))
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	risk, found := riskFor(report, "s1")
	if !found {
		t.Fatalf("no context curve for s1: the subagent's turns were mixed into the main thread, "+
			"so every hand-off looked like a reset. Got %+v", report.ContextRisks)
	}
	if risk.Curve.Turns != mainTurns {
		t.Errorf("curve covers %d turns, want %d (the main thread only)", risk.Curve.Turns, mainTurns)
	}
	if risk.Curve.Baseline != 60_000 {
		t.Errorf("Baseline = %d, want 60000; the subagent's 8k context leaked into the main thread's shape",
			risk.Curve.Baseline)
	}
}

func TestAnalyze_ReportsWhenASessionRanPastItsBreakEven(t *testing.T) {
	records := growingSession("s1", "", "sesión eterna", 400, 60_000, 2_000, 0, 1)

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	risk, found := riskFor(report, "s1")
	if !found {
		t.Fatalf("expected a context curve for s1; got %+v", report.ContextRisks)
	}
	if risk.Curve.NoReturnTurn <= 0 || risk.Curve.NoReturnTurn >= 400 {
		t.Errorf("NoReturnTurn = %d, want a turn inside the session", risk.Curve.NoReturnTurn)
	}
	wantPast := 400 - risk.Curve.NoReturnTurn
	if risk.Curve.TurnsPastNoReturn != wantPast {
		t.Errorf("TurnsPastNoReturn = %d, want %d", risk.Curve.TurnsPastNoReturn, wantPast)
	}
	if risk.Curve.SavingsUSD <= 0 {
		t.Errorf("SavingsUSD = %v, want > 0", risk.Curve.SavingsUSD)
	}
}

// The finding has to name the session and the turn. "Make sessions shorter" is
// the advice that already failed for nine days; "this one crossed the line at
// turn N and ran M turns past it" is the thing a person can act on.
func TestAnalyze_FindingNamesTheWorstSessionAndItsBreakEven(t *testing.T) {
	records := growingSession("s1", "", "la sesión cara de Telegram", 400, 60_000, 2_000, 0, 1)

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	f, ok := findingByID(report, FindingContextNoReturn)
	if !ok {
		t.Fatalf("expected finding %s; got %+v", FindingContextNoReturn, report.Findings)
	}
	if !strings.Contains(f.Evidence, "la sesión cara de Telegram") {
		t.Errorf("evidence does not name the task: %s", f.Evidence)
	}
	if f.SavingsUSD <= 0 {
		t.Errorf("SavingsUSD = %v, want the measured overrun", f.SavingsUSD)
	}
	if f.MetricName != MetricPastNoReturnCostShare {
		t.Errorf("MetricName = %q, want %q", f.MetricName, MetricPastNoReturnCostShare)
	}
	if f.Metric <= 0 || f.Metric > 1 {
		t.Errorf("Metric = %v, want a share in (0,1]: the recurrence check compares shares, never totals", f.Metric)
	}
}

// A savings figure that ignores what cutting costs would be a number this tool
// invented. The caveat travels with the figure, not in a doc nobody opens.
func TestAnalyze_FindingSaysTheSavingsIgnoreTheCostOfLosingContext(t *testing.T) {
	records := growingSession("s1", "", "sesión eterna", 400, 60_000, 2_000, 0, 1)

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	f, ok := findingByID(report, FindingContextNoReturn)
	if !ok {
		t.Fatalf("expected finding %s", FindingContextNoReturn)
	}
	if !strings.Contains(f.Evidence, "NO está medido") {
		t.Errorf("evidence presents the savings without saying what it leaves out: %s", f.Evidence)
	}
}

func TestAnalyze_StaysQuietWhenNoSessionPassesItsBreakEven(t *testing.T) {
	// Twelve short sessions that never grow enough to be worth cutting.
	var records []aggregate.Record
	for s := 0; s < 12; s++ {
		records = append(records, growingSession(
			string(rune('a'+s)), "", "tarea corta", 5, 10_000, 100, s*100, 1)...)
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if len(report.ContextRisks) != 0 {
		t.Errorf("ContextRisks = %+v, want none: no session ran past its break-even", report.ContextRisks)
	}
	if _, ok := findingByID(report, FindingContextNoReturn); ok {
		t.Error("emitted the context finding with nothing to report; the report must stay quiet")
	}
}

func TestAnalyze_RanksTheCostliestOverrunFirst(t *testing.T) {
	var records []aggregate.Record
	records = append(records, growingSession("small", "", "tarea mediana", 120, 60_000, 2_000, 0, 1)...)
	records = append(records, growingSession("big", "", "tarea eterna", 600, 60_000, 2_000, 1000, 1)...)

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if len(report.ContextRisks) < 2 {
		t.Fatalf("expected both sessions to be flagged; got %+v", report.ContextRisks)
	}
	if report.ContextRisks[0].Curve.SessionID != "big" {
		t.Errorf("first risk = %q, want %q: the list ranks by what cutting would save",
			report.ContextRisks[0].Curve.SessionID, "big")
	}
}
