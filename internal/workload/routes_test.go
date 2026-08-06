package workload

import (
	"strings"
	"testing"
)

const (
	routeClaudeCode = "Claude Code"
	routeOpenClaw   = "OpenClaw"
	routeCursor     = "Cursor"
)

var testRoutes = []string{routeClaudeCode, routeOpenClaw, routeCursor}

// aBurst is a mechanical-burst stream on a given route — few turns on a small
// context — so route-plan tests vary only what they're about: who ran it, on
// what, and for how much. Turns stay inside fewTurnsCeiling by construction:
// past it the stream is a different shape and would land in a different plan.
func aBurst(route, model string, turns int, costUSD float64) Shape {
	return Shape{
		SessionID:       "s-burst-" + route + model,
		Agent:           route,
		Model:           model,
		Measured:        true,
		Turns:           turns,
		TokensPerTurn:   20_000,
		ContextTokens:   20_000 * turns,
		CacheReadTokens: 10_000 * turns,
		CostUSD:         costUSD,
	}
}

// aLongConversation is the accumulating shape, which is the one that can carry
// enough turns for the volume-sensitive parts of the plan (the observation cap
// only bites when one option ran far more turns than another).
func aLongConversation(route, model string, turns int, costUSD float64) Shape {
	return Shape{
		SessionID:       "s-long-" + route + model,
		Agent:           route,
		Model:           model,
		Measured:        true,
		Turns:           turns,
		TokensPerTurn:   80_000,
		ContextTokens:   100_000 * turns,
		CacheReadTokens: 95_000 * turns,
		GrowthPerTurn:   900,
		NoReturnTurn:    30,
		CurveKnown:      true,
		CostUSD:         costUSD,
	}
}

func planFor(report Report, class string) ClassPlan {
	for _, c := range report.Classes {
		if c.Class == class {
			return c
		}
	}
	return ClassPlan{}
}

func TestAnalyze_ComparesWhatTheSameShapeCostThroughEachRoute(t *testing.T) {
	shapes := []Shape{
		aLongConversation(routeClaudeCode, "claude-opus-5", 100, 20),
		aLongConversation(routeOpenClaw, "claude-sonnet-5", 100, 5),
	}

	plan := planFor(Analyze(shapes, testRoutes), ClassLongConversation)

	if len(plan.Routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(plan.Routes))
	}
	if !plan.ByRoute.Known || plan.ByRoute.Cheapest != routeOpenClaw {
		t.Errorf("cheapest route = %q (known=%v), want %q", plan.ByRoute.Cheapest, plan.ByRoute.Known, routeOpenClaw)
	}
	// 100 turns at $0.20 vs the same 100 at $0.05: $15 of measured difference.
	if got := plan.ByRoute.SavingsUSD; got < 14.99 || got > 15.01 {
		t.Errorf("savings = %v, want 15", got)
	}
}

// The claim can only cover as many turns as the cheap option was actually seen
// carrying. Beyond that it stops being a measurement and becomes a forecast,
// which is the one thing this project does not do.
func TestAnalyze_CapsTheClaimAtTheTurnsTheCheapOptionWasObservedCarrying(t *testing.T) {
	shapes := []Shape{
		aLongConversation(routeClaudeCode, "claude-opus-5", 10_000, 2000), // $0.20/turn over 10k turns
		aLongConversation(routeOpenClaw, "claude-sonnet-5", 40, 2),        // $0.05/turn, only 40 turns of evidence
	}

	cf := planFor(Analyze(shapes, testRoutes), ClassLongConversation).ByRoute

	if cf.TurnsElsewhere != 10_000 {
		t.Fatalf("turns elsewhere = %d, want 10000", cf.TurnsElsewhere)
	}
	if cf.MovableTurns != 40 {
		t.Errorf("movable turns = %d, want 40 (all the evidence the cheap route has)", cf.MovableTurns)
	}
	if !cf.Capped() {
		t.Error("Capped() = false, want true so the report can say the claim was limited by observation")
	}
	// 40 turns × ($0.20 − $0.05), not 10,000 × the gap.
	if got := cf.SavingsUSD; got < 5.99 || got > 6.01 {
		t.Errorf("savings = %v, want 6 — the uncapped figure would be 1500", got)
	}
}

// A route that ran nothing of this shape is missing data, and the report says so
// with a reason. Leaving it out silently would read as "it wasn't relevant".
func TestAnalyze_NamesTheRoutesWithNoFigureAndWhy(t *testing.T) {
	shapes := []Shape{
		aBurst(routeClaudeCode, "claude-opus-5", 10, 20),
		aLongConversation(routeOpenClaw, "claude-opus-5", 60, 9), // OpenClaw ran, but not this shape
	}

	missing := planFor(Analyze(shapes, testRoutes), ClassMechanicalBurst).Missing

	reasons := map[string]string{}
	for _, m := range missing {
		reasons[m.Route] = m.Reason
	}
	if reasons[routeOpenClaw] != MissingNoShapeInClass {
		t.Errorf("openclaw reason = %q, want %q", reasons[routeOpenClaw], MissingNoShapeInClass)
	}
	if reasons[routeCursor] != MissingNoActivity {
		t.Errorf("cursor reason = %q, want %q", reasons[routeCursor], MissingNoActivity)
	}
}

// Cursor and Antigravity are activity estimates. Standing their figure next to a
// measured one as if both meant the same thing is exactly the error the
// confidence tiers exist to prevent, so the plan says why they're out.
func TestAnalyze_KeepsEstimatedRoutesOutOfTheComparisonAndSaysSo(t *testing.T) {
	estimated := aBurst(routeCursor, "", 5, 100)
	estimated.Measured = false
	shapes := []Shape{aBurst(routeClaudeCode, "claude-opus-5", 10, 20), estimated}

	report := Analyze(shapes, testRoutes)

	plan := planFor(report, ClassMechanicalBurst)
	for _, r := range plan.Routes {
		if r.Route == routeCursor {
			t.Fatalf("estimated route %q entered the measured comparison", routeCursor)
		}
	}
	var reason string
	for _, m := range plan.Missing {
		if m.Route == routeCursor {
			reason = m.Reason
		}
	}
	if reason != MissingActivityOnly {
		t.Errorf("cursor reason = %q, want %q", reason, MissingActivityOnly)
	}
	if report.Unclassified.Streams != 1 {
		t.Errorf("unclassified streams = %d, want the estimated one counted there", report.Unclassified.Streams)
	}
}

// A free local model costs $0 because it runs on owned hardware, not because it
// is more efficient. Letting it win the comparison would produce a "save 100%"
// figure that means nothing.
func TestAnalyze_LeavesZeroCostOptionsOutOfTheComparisonInsteadOfLettingThemWin(t *testing.T) {
	shapes := []Shape{
		aBurst(routeClaudeCode, "claude-opus-5", 10, 2),
		aBurst(routeClaudeCode, "nemotron-3-super", 10, 0),
		aBurst(routeOpenClaw, "claude-sonnet-5", 10, 0.5),
	}

	cf := planFor(Analyze(shapes, testRoutes), ClassMechanicalBurst).ByModel

	if cf.Cheapest == "nemotron-3-super" {
		t.Fatal("a zero-cost local model won the counterfactual")
	}
	if cf.Cheapest != "claude-sonnet-5" {
		t.Errorf("cheapest model = %q, want claude-sonnet-5", cf.Cheapest)
	}
	if len(cf.Excluded) != 1 || cf.Excluded[0] != "nemotron-3-super" {
		t.Errorf("excluded = %v, want the local model named", cf.Excluded)
	}
}

func TestAnalyze_SaysThereIsNoCounterfactualWhenOnlyOneRouteRanTheShape(t *testing.T) {
	shapes := []Shape{aBurst(routeClaudeCode, "claude-opus-5", 10, 20)}

	cf := planFor(Analyze(shapes, testRoutes), ClassMechanicalBurst).ByRoute

	if cf.Known {
		t.Fatal("claimed a comparison with a single observed route")
	}
	if !strings.Contains(cf.Reason, "no se interpola") {
		t.Errorf("reason = %q, want it to say the missing data is not interpolated", cf.Reason)
	}
}

func TestAnalyze_ReportsUnclassifiedLoadWithTheReasonItStayedThere(t *testing.T) {
	between := aShape()
	between.Turns = 12
	between.TokensPerTurn = 90_000
	between.CostUSD = 3

	report := Analyze([]Shape{between, aBurst(routeClaudeCode, "claude-opus-5", 10, 20)}, testRoutes)

	if report.Classified != 1 {
		t.Errorf("classified = %d, want 1", report.Classified)
	}
	if report.Unclassified.Streams != 1 || report.Unclassified.CostUSD != 3 {
		t.Errorf("unclassified = %+v, want 1 stream carrying $3", report.Unclassified)
	}
	if len(report.Unclassified.Reasons) != 1 || report.Unclassified.Reasons[0].Reason != ReasonBetweenShapes {
		t.Errorf("reasons = %+v, want the between-shapes reason", report.Unclassified.Reasons)
	}
}

// Every shape is listed even when nothing matched it: "this shape didn't appear"
// is information, and a table that only shows what fired can't say it.
func TestAnalyze_ListsEveryShapeEvenTheOnesNothingMatched(t *testing.T) {
	report := Analyze([]Shape{aBurst(routeClaudeCode, "claude-opus-5", 10, 20)}, testRoutes)

	if len(report.Classes) != len(classOrder) {
		t.Fatalf("classes = %d, want %d", len(report.Classes), len(classOrder))
	}
	oneShot := planFor(report, ClassOneShot)
	if oneShot.Streams != 0 || oneShot.Lever == "" {
		t.Errorf("empty class = %+v, want it listed with its lever and zero streams", oneShot)
	}
}

func TestAnalyze_SharesAddUpToTheWholeWindow(t *testing.T) {
	unclassified := aShape()
	unclassified.Turns = 12
	unclassified.TokensPerTurn = 90_000
	unclassified.CostUSD = 25
	shapes := []Shape{aBurst(routeClaudeCode, "claude-opus-5", 10, 75), unclassified}

	report := Analyze(shapes, testRoutes)

	total := report.Unclassified.CostShare
	for _, c := range report.Classes {
		total += c.CostShare
	}
	if total < 0.999 || total > 1.001 {
		t.Errorf("shares sum to %v, want 1", total)
	}
}
