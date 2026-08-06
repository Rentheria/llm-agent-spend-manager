package quota

import (
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

func TestCycleFill_CrossesTheBoundsSoTheWorstCaseIsTheHighEnd(t *testing.T) {
	c := Cycle{
		Capacity: Capacity{Known: true, Limit: RangeMeasure(100, 200)},
		Used:     RangeMeasure(40, 60),
	}

	fill, ok := c.Fill()
	if !ok {
		t.Fatal("a known capacity must yield a fill")
	}
	// Worst case is the most consumed against the smallest ceiling: 60/100.
	if fill.High != 0.60 {
		t.Errorf("fill.High = %v, want 0.60", fill.High)
	}
	// Best case is the least consumed against the largest: 40/200.
	if fill.Low != 0.20 {
		t.Errorf("fill.Low = %v, want 0.20", fill.Low)
	}
}

func TestCycleRemaining_NeverGoesNegative(t *testing.T) {
	c := Cycle{
		Capacity: Capacity{Known: true, Limit: ExactMeasure(100)},
		Used:     ExactMeasure(140),
	}

	remaining, ok := c.Remaining()
	if !ok {
		t.Fatal("a known capacity must yield a remaining")
	}
	if remaining.Point != 0 {
		t.Errorf("remaining = %v, want 0: an overrun is empty, not negative", remaining.Point)
	}
}

func TestProject_UsesThePessimisticEndOfBothRanges(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	c := Cycle{
		Reset:    now.Add(4 * time.Hour),
		Capacity: Capacity{Known: true, Limit: RangeMeasure(100, 300)},
		Used:     ExactMeasure(50),
	}

	// Smallest ceiling (100) minus what is used (50) leaves 50, which at 50/h is
	// one hour — well before the reset.
	got := Project(c, Burn{PerHour: 50}, now)
	if !got.Known || !got.Exhausts {
		t.Fatalf("forecast = %+v, want a known exhaustion", got)
	}
	if got.TimeLeft != time.Hour {
		t.Errorf("time left = %s, want 1h", got.TimeLeft)
	}
}

// Not exhausting is the healthy case and deserves to be said in those words
// rather than reported as an unknown.
func TestProject_ReportsSurvivalToTheResetAsAKnownAnswer(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	c := Cycle{
		Reset:    now.Add(time.Hour),
		Capacity: Capacity{Known: true, Limit: ExactMeasure(1000)},
		Used:     ExactMeasure(10),
	}

	got := Project(c, Burn{PerHour: 10}, now)
	if !got.Known || got.Exhausts {
		t.Fatalf("forecast = %+v, want known and not exhausting", got)
	}
	if got.TimeLeft != time.Hour {
		t.Errorf("time left = %s, want the wait until the reset", got.TimeLeft)
	}
}

func TestProject_IsUnknownWithoutACeilingOrWithoutARate(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	noCeiling := Cycle{Reset: now.Add(time.Hour), Used: ExactMeasure(10)}
	if got := Project(noCeiling, Burn{PerHour: 10}, now); got.Known {
		t.Error("forecast a ceiling that is not known")
	}
	withCeiling := Cycle{Reset: now.Add(time.Hour), Capacity: Capacity{Known: true, Limit: ExactMeasure(100)}}
	if got := Project(withCeiling, Burn{}, now); got.Known {
		t.Error("forecast from a burn rate of zero")
	}
}

func TestUsed_KeepsActivityTierDataAsARange(t *testing.T) {
	records := []aggregate.Record{
		{Confidence: aggregate.ConfidenceMeasured, Output: 100},
		{Confidence: aggregate.ConfidenceActivity, TokensLow: 50, TokensHigh: 150},
	}

	got := Used(records, UnitTokens)
	if got.Exact {
		t.Error("a total containing an estimate was marked exact")
	}
	if got.Low != 150 || got.High != 250 {
		t.Errorf("used = %+v, want 150–250", got)
	}
}

func TestUnmeasured_CountsTurnsWhoseConsumptionCannotBeRead(t *testing.T) {
	records := []aggregate.Record{
		{Confidence: aggregate.ConfidenceActivity, CostKnown: true, CostLowUSD: 1, CostHighUSD: 3},
		{Confidence: aggregate.ConfidenceActivity, CostKnown: false},
		{Confidence: aggregate.ConfidenceActivity, CostKnown: false},
	}

	if got := Unmeasured(records, UnitUSD); got != 2 {
		t.Errorf("unmeasured = %d, want 2", got)
	}
	if got := Used(records, UnitUSD); got.Point != 2 {
		t.Errorf("used = %v, want 2 (only the priced turn contributes)", got.Point)
	}
}

func TestBurnRate_MeasuresOnlyTheTrailingLookback(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		{Confidence: aggregate.ConfidenceMeasured, Timestamp: now.Add(-10 * time.Minute), Output: 1000},
		// Two hours ago: outside the lookback, and the reason a whole-window
		// average would still be reporting the morning's calm.
		{Confidence: aggregate.ConfidenceMeasured, Timestamp: now.Add(-2 * time.Hour), Output: 999_000},
	}

	got := BurnRate(records, UnitTokens, 30*time.Minute, now)
	if got.Turns != 1 {
		t.Fatalf("turns = %d, want 1", got.Turns)
	}
	if got.PerHour != 2000 {
		t.Errorf("rate = %v/h, want 2000 (1000 tokens over half an hour)", got.PerHour)
	}
}
