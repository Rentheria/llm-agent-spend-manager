package advise

import (
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
)

// measuredTurn builds a token-accurate record the same way the real adapters do
// (aggregate.FromClaudeCode): priced model, CostUSD computed from the token buckets by
// the pricing table. Setting CostUSD by hand here would let the fixture drift
// from what production data actually looks like.
func measuredTurn(session string, day int, tokens [4]int) aggregate.Record {
	r := aggregate.Record{
		Agent:      aggregate.AgentClaudeCode,
		Mode:       aggregate.ModeInteractive,
		SessionID:  session,
		Timestamp:  time.Date(2026, 7, day, 10, 0, 0, 0, time.UTC),
		Model:      "claude-opus-5",
		Input:      tokens[0],
		Output:     tokens[1],
		CacheWrite: tokens[2],
		CacheRead:  tokens[3],
		Confidence: aggregate.ConfidenceMeasured,
	}
	r.CostUSD, r.CostKnown = pricing.EstimateUSD(r.Model, r.Input, r.Output, r.CacheWrite, r.CacheRead)
	total := r.TotalTokens()
	r.TokensLow, r.TokensHigh = total, total
	r.CostLowUSD, r.CostHighUSD = r.CostUSD, r.CostUSD
	return r
}

func findingByID(report Report, id string) (Finding, bool) {
	for _, f := range report.Findings {
		if f.ID == id {
			return f, true
		}
	}
	return Finding{}, false
}

func TestAnalyze_AttributesCostToTheDominantBucket(t *testing.T) {
	// A turn that reads a huge cached context and writes a tiny answer: the same
	// shape as this machine's real data, where cache-read carries the bill.
	records := []aggregate.Record{measuredTurn("s1", 20, [4]int{100, 100, 0, 5_000_000})}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if len(report.Fleet.Buckets) == 0 {
		t.Fatal("fleet buckets empty; expected a per-bucket cost split")
	}
	top := report.Fleet.Buckets[0]
	if top.Bucket != BucketCacheRead {
		t.Errorf("dominant bucket = %q, want %q", top.Bucket, BucketCacheRead)
	}
	if top.Tokens != 5_000_000 {
		t.Errorf("dominant bucket tokens = %d, want 5000000", top.Tokens)
	}
	f, ok := findingByID(report, FindingDominantBucket)
	if !ok {
		t.Fatalf("expected finding %s; got %+v", FindingDominantBucket, report.Findings)
	}
	if f.Impact != ImpactHigh {
		t.Errorf("impact = %q, want %q (the bucket carries nearly all the cost)", f.Impact, ImpactHigh)
	}
}

func TestAnalyze_DoesNotFlagBucketsWhenNoneDominates(t *testing.T) {
	// Output at $25/M vs cache-read at $0.50/M: these token counts put the two
	// buckets at roughly half the cost each, so no single lever dominates.
	records := []aggregate.Record{measuredTurn("s1", 20, [4]int{0, 20_000, 0, 1_000_000})}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if _, ok := findingByID(report, FindingDominantBucket); ok {
		t.Errorf("did not expect %s: buckets are balanced (%+v)", FindingDominantBucket, report.Fleet.Buckets)
	}
}

func TestAnalyze_FlagsCacheWrittenButNeverRead(t *testing.T) {
	// Two one-shot sessions that wrote cache nobody read back, plus a healthy
	// session that does reuse it.
	records := []aggregate.Record{
		measuredTurn("wasted-1", 20, [4]int{100, 50, 200_000, 0}),
		measuredTurn("wasted-2", 20, [4]int{100, 50, 200_000, 0}),
		measuredTurn("healthy", 20, [4]int{100, 50, 1000, 500_000}),
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	f, ok := findingByID(report, FindingCacheWasted)
	if !ok {
		t.Fatalf("expected finding %s; got %+v", FindingCacheWasted, report.Findings)
	}
	// 400k cache-write tokens on opus-5 at $6.25/M = $2.50 provably wasted.
	if want := 2.50; f.SavingsUSD < want*0.99 || f.SavingsUSD > want*1.01 {
		t.Errorf("savings = %.4f, want ~%.2f (the write surcharge that bought nothing)", f.SavingsUSD, want)
	}
}

func TestAnalyze_KeepsQuietWhenCacheIsReused(t *testing.T) {
	records := []aggregate.Record{
		measuredTurn("s1", 20, [4]int{100, 50, 1000, 500_000}),
		measuredTurn("s1", 20, [4]int{100, 50, 0, 500_000}),
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if _, ok := findingByID(report, FindingCacheWasted); ok {
		t.Error("did not expect a wasted-cache finding: every session reused its cache")
	}
	if report.Fleet.CacheReuseRatio <= 1 {
		t.Errorf("cache reuse ratio = %.2f, want > 1 (reads exceed writes)", report.Fleet.CacheReuseRatio)
	}
}

func TestAnalyze_ReportsSessionsAndTurnsPerSession(t *testing.T) {
	records := []aggregate.Record{
		measuredTurn("s1", 20, [4]int{100, 50, 0, 1000}),
		measuredTurn("s1", 20, [4]int{100, 50, 0, 1000}),
		measuredTurn("s2", 20, [4]int{100, 50, 0, 1000}),
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if report.Fleet.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", report.Fleet.Sessions)
	}
	if got, want := report.Fleet.TurnsPerSession, 1.5; got != want {
		t.Errorf("turns per session = %.2f, want %.2f", got, want)
	}
}

func TestAnalyze_TrendNeedsTwoFullBlocksOfActiveDays(t *testing.T) {
	var records []aggregate.Record
	for day := 1; day <= 2*trendWindowDays-1; day++ {
		records = append(records, measuredTurn("s", day, [4]int{100, 50, 0, 1000}))
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if report.Trend.Direction != TrendInsufficient {
		t.Errorf("direction = %q, want %q with only %d active days",
			report.Trend.Direction, TrendInsufficient, 2*trendWindowDays-1)
	}
}

func TestAnalyze_TrendCallsItImprovingWhenCostPerTurnDrops(t *testing.T) {
	var records []aggregate.Record
	// First block: expensive turns. Second block: same work, a tenth of the
	// context — cost per turn falls, which is the whole point of the metric.
	for day := 1; day <= trendWindowDays; day++ {
		records = append(records, measuredTurn("old", day, [4]int{100, 50, 0, 1_000_000}))
	}
	for day := trendWindowDays + 1; day <= 2*trendWindowDays; day++ {
		records = append(records, measuredTurn("new", day, [4]int{100, 50, 0, 100_000}))
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if report.Trend.Direction != TrendImproving {
		t.Errorf("direction = %q, want %q (delta %.1f%%)",
			report.Trend.Direction, TrendImproving, report.Trend.DeltaPct)
	}
	if report.Trend.DeltaPct >= 0 {
		t.Errorf("deltaPct = %.1f, want negative (cheaper per turn)", report.Trend.DeltaPct)
	}
}

func TestAnalyze_TrendIgnoresMoreWorkAtTheSameEfficiency(t *testing.T) {
	var records []aggregate.Record
	for day := 1; day <= trendWindowDays; day++ {
		records = append(records, measuredTurn("old", day, [4]int{100, 50, 0, 500_000}))
	}
	// Same per-turn shape, three times the turns: the total cost triples but
	// efficiency didn't change, so this must NOT read as a regression.
	for day := trendWindowDays + 1; day <= 2*trendWindowDays; day++ {
		for i := 0; i < 3; i++ {
			records = append(records, measuredTurn("new", day, [4]int{100, 50, 0, 500_000}))
		}
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if report.Trend.Direction != TrendFlat {
		t.Errorf("direction = %q, want %q (delta %.1f%%): working more is not a regression",
			report.Trend.Direction, TrendFlat, report.Trend.DeltaPct)
	}
}

func TestAnalyze_FlagsUnpricedModels(t *testing.T) {
	unpriced := measuredTurn("s1", 20, [4]int{100, 50, 0, 1000})
	unpriced.Model = "some-model-not-in-the-table"
	unpriced.CostKnown = false
	unpriced.CostUSD = 0

	report := Analyze([]aggregate.Record{unpriced}, string(aggregate.WindowAll), time.UTC)

	f, ok := findingByID(report, FindingUnpricedModels)
	if !ok {
		t.Fatalf("expected finding %s; got %+v", FindingUnpricedModels, report.Findings)
	}
	if f.SavingsUSD != 0 {
		t.Errorf("savings = %.2f, want 0: a data-quality gap has no defensible savings figure", f.SavingsUSD)
	}
}

func TestAnalyze_MarksActivityTierAgentsAsNotMeasured(t *testing.T) {
	activity := aggregate.Record{
		Agent:       aggregate.AgentCursor,
		Mode:        aggregate.ModeEditor,
		Confidence:  aggregate.ConfidenceActivity,
		SessionID:   "conv-1",
		Timestamp:   time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		TokensLow:   1000,
		TokensHigh:  3000,
		CostLowUSD:  0.01,
		CostHighUSD: 0.10,
		CostUSD:     0.055,
		CostKnown:   true,
	}

	report := Analyze([]aggregate.Record{activity}, string(aggregate.WindowAll), time.UTC)

	if len(report.ByAgent) != 1 {
		t.Fatalf("want 1 agent, got %d", len(report.ByAgent))
	}
	if report.ByAgent[0].Measured {
		t.Error("activity-tier agent reported as measured; its ratios are not real")
	}
	if report.ByAgent[0].TokensPerTurn != 2000 {
		t.Errorf("tokens per turn = %d, want 2000 (range midpoint)", report.ByAgent[0].TokensPerTurn)
	}
}

func TestAnalyze_EmptyRecordsProduceNoFindings(t *testing.T) {
	report := Analyze(nil, string(aggregate.WindowToday), time.UTC)

	if len(report.Findings) != 0 {
		t.Errorf("want no findings on empty data, got %+v", report.Findings)
	}
	if report.Trend.Direction != TrendInsufficient {
		t.Errorf("direction = %q, want %q", report.Trend.Direction, TrendInsufficient)
	}
	if report.CostLabel != aggregate.CostLabel {
		t.Errorf("cost label = %q, want the mandatory wording %q", report.CostLabel, aggregate.CostLabel)
	}
}

func TestHumanInt_GroupsThousands(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := humanInt(in); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", in, got, want)
		}
	}
}
