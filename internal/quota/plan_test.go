package quota

import (
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

func cursorTurn(ts time.Time, low, high float64, priced bool) aggregate.Record {
	return aggregate.Record{
		Agent:       aggregate.AgentCursor,
		Confidence:  aggregate.ConfidenceActivity,
		Timestamp:   ts,
		CostKnown:   priced,
		CostLowUSD:  low,
		CostHighUSD: high,
		CostUSD:     (low + high) / 2,
	}
}

// The quota belongs to the account, so every agent driving Claude on it drains
// the same window — the fact verified on 2026-07-27 when one exhaustion silenced
// both agents at once.
func TestAnthropicMax_CoversEveryAgentOnTheAccount(t *testing.T) {
	plan := AnthropicMax{Tier: "Max 5x"}
	covered := map[string]bool{
		aggregate.AgentClaudeCode:  true,
		aggregate.AgentOpenClaw:    true,
		aggregate.AgentCursor:      false,
		aggregate.AgentAntigravity: false,
	}
	for agent, want := range covered {
		if got := plan.Covers(aggregate.Record{Agent: agent}); got != want {
			t.Errorf("Covers(%s) = %v, want %v", agent, got, want)
		}
	}
}

func TestAnthropicMax_SessionCycleCarriesTheCalibratedCeiling(t *testing.T) {
	records, events := fleet([]int{1, 2, 3}, "claude-opus-5", []int{100_000, 200_000, 300_000})
	calibration := Calibrate(records, events, sessionWindowLength)
	plan := AnthropicMax{Tier: "Max 5x", Calibration: calibration}

	// now sits inside the last window, which opened 2026-06-03 08:00.
	now := time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)
	cycles := plan.Cycles(records, now)
	if len(cycles) != 2 {
		t.Fatalf("cycles = %d, want the session window plus the weekly cap", len(cycles))
	}

	session := cycles[0]
	if session.Name != CycleSessionWindow || session.Unit != UnitTokens {
		t.Fatalf("first cycle = %q/%q, want the 5h window in tokens", session.Name, session.Unit)
	}
	if session.Capacity.Source != SourceCalibrated {
		t.Errorf("session ceiling source = %q, want %q", session.Capacity.Source, SourceCalibrated)
	}
	if _, ok := session.Fill(); !ok {
		t.Error("a calibrated ceiling should yield a fill percentage")
	}
}

// Anthropic publishes no weekly allowance and it has never been hit here, so the
// weekly cycle reports real consumption against no ceiling — rather than a
// percentage of a number nobody knows.
func TestAnthropicMax_WeeklyCycleReportsConsumptionWithoutInventingACeiling(t *testing.T) {
	records, _ := fleet([]int{1}, "claude-opus-5", []int{100_000})
	plan := AnthropicMax{Tier: "Max 5x"}

	cycles := plan.Cycles(records, time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC))
	weekly := cycles[len(cycles)-1]
	if weekly.Name != CycleWeeklyCap {
		t.Fatalf("last cycle = %q, want %q", weekly.Name, CycleWeeklyCap)
	}
	if weekly.Capacity.Known {
		t.Error("the weekly cap was given a ceiling that has never been observed")
	}
	if _, ok := weekly.Fill(); ok {
		t.Error("a cycle with no known ceiling must not produce a percentage")
	}
	if weekly.Used.Point == 0 {
		t.Error("the weekly consumption is real and must still be reported")
	}
}

func TestAnthropicMax_NoSessionCycleOnceTheWindowRefilled(t *testing.T) {
	records, _ := fleet([]int{1}, "claude-opus-5", []int{100_000})
	plan := AnthropicMax{Tier: "Max 5x"}

	// Six hours after the window opened it has long since refilled.
	cycles := plan.Cycles(records, time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC))
	for _, c := range cycles {
		if c.Name == CycleSessionWindow {
			t.Error("reported a session window that already refilled")
		}
	}
}

// Cursor is the one plan whose dollars are money, so its percentage is exact.
func TestCursorPlan_MeasuresTheBillingMonthAgainstThePublishedAllowance(t *testing.T) {
	plan := CursorPlan{MonthlyUSD: 200, RenewalDay: 1}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		cursorTurn(time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC), 20, 60, true),
		// Previous billing month: outside this cycle.
		cursorTurn(time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC), 100, 100, true),
	}

	cycles := plan.Cycles(records, now)
	if len(cycles) != 1 {
		t.Fatalf("cycles = %d, want 1", len(cycles))
	}
	c := cycles[0]
	if c.Unit != UnitUSD || c.Capacity.Source != SourcePlan {
		t.Errorf("cycle = %q/%q, want a published USD allowance", c.Unit, c.Capacity.Source)
	}
	if c.Turns != 1 {
		t.Errorf("turns = %d, want 1: the June turn belongs to the previous cycle", c.Turns)
	}
	fill, ok := c.Fill()
	if !ok {
		t.Fatal("a published allowance must yield a fill percentage")
	}
	// Activity data is a range and stays one: 20/200 to 60/200.
	if fill.Exact {
		t.Error("an activity-tier consumption was collapsed to an exact percentage")
	}
	if fill.Low != 0.10 || fill.High != 0.30 {
		t.Errorf("fill = %+v, want 10%%–30%%", fill)
	}
}

// A plan whose turns carry no readable cost must say so; otherwise a $0.00 reads
// as "nobody is using this $200 plan".
func TestCursorPlan_CountsTheTurnsItCannotPrice(t *testing.T) {
	plan := CursorPlan{MonthlyUSD: 200, RenewalDay: 1}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		cursorTurn(time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC), 0, 0, false),
		cursorTurn(time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC), 0, 0, false),
	}

	c := plan.Cycles(records, now)[0]
	if c.Unmeasured != 2 || c.Turns != 2 {
		t.Errorf("unmeasured/turns = %d/%d, want 2/2", c.Unmeasured, c.Turns)
	}
}

func TestBillingMonth_RollsBackWhenTheRenewalDayHasNotArrived(t *testing.T) {
	start, reset := billingMonth(time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC), 15)
	if start.Month() != time.June || start.Day() != 15 {
		t.Errorf("start = %s, want 15 June", start)
	}
	if reset.Month() != time.July || reset.Day() != 15 {
		t.Errorf("reset = %s, want 15 July", reset)
	}
}

// Antigravity exposes nothing to meter, and is listed rather than omitted: an
// absent agent reads as an agent consuming nothing.
func TestUnmeteredAgents_NamesAntigravityWithItsReason(t *testing.T) {
	reason, ok := UnmeteredAgents[aggregate.AgentAntigravity]
	if !ok || reason == "" {
		t.Fatal("Antigravity must be listed as unmetered, with a reason")
	}
}
