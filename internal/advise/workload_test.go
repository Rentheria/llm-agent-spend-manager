package advise

import (
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/workload"
)

func planFor(report workload.Report, class string) workload.ClassPlan {
	for _, c := range report.Classes {
		if c.Class == class {
			return c
		}
	}
	return workload.ClassPlan{}
}

// onRoute re-attributes turns to another agent so a test can put the same shape
// through two routes.
func onRoute(records []aggregate.Record, agent string) []aggregate.Record {
	out := make([]aggregate.Record, 0, len(records))
	for _, r := range records {
		r.Agent = agent
		out = append(out, r)
	}
	return out
}

// A session that spawned subagents ran several independent contexts at once. Its
// main thread and its subagents can be different SHAPES with different levers,
// so classifying the session as one thing would hand at least one of them the
// wrong advice.
func TestAnalyze_ClassifiesEachContextThreadSeparately(t *testing.T) {
	records := growingSession("s-1", "main", "tarea larga", 300, 30_000, 800, 0, 2)
	for i := 0; i < 5; i++ {
		records = append(records, contextTurn("s-1", "sub", "tarea larga", 1+i*2, 20_000))
	}

	report := Analyze(records, "all", time.UTC)

	if got := planFor(report.Workloads, workload.ClassLongConversation).Streams; got != 1 {
		t.Errorf("long conversations = %d, want the main thread counted once", got)
	}
	if got := planFor(report.Workloads, workload.ClassMechanicalBurst).Streams; got != 1 {
		t.Errorf("mechanical bursts = %d, want the subagent thread counted once", got)
	}
}

func TestAnalyze_ComparesTheSameShapeAcrossTheRoutesThatRanIt(t *testing.T) {
	cc := growingSession("s-cc", "", "misma forma", 300, 30_000, 800, 0, 2)
	oc := onRoute(growingSession("s-oc", "", "misma forma", 300, 30_000, 800, 0, 2), aggregate.AgentOpenClaw)

	report := Analyze(append(cc, oc...), "all", time.UTC)

	plan := planFor(report.Workloads, workload.ClassLongConversation)
	if len(plan.Routes) != 2 {
		t.Fatalf("routes = %d, want both routes compared", len(plan.Routes))
	}
	if !plan.ByRoute.Known {
		t.Errorf("no counterfactual with two measured routes: %q", plan.ByRoute.Reason)
	}
}

// Cursor and Antigravity expose one record per conversation, not per turn. The
// plan has to say their figure is missing rather than quietly leaving them out
// of the comparison, which would read as "they weren't relevant".
func TestAnalyze_ReportsEstimatedRoutesAsMissingDataNotAsAClass(t *testing.T) {
	records := growingSession("s-cc", "", "medida", 300, 30_000, 800, 0, 2)
	records = append(records, aggregate.Record{
		Agent:      aggregate.AgentCursor,
		SessionID:  "conv-1",
		Timestamp:  time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC),
		Confidence: aggregate.ConfidenceActivity,
		TokensLow:  100_000,
		TokensHigh: 300_000,
		CostUSD:    2,
		CostKnown:  true,
	})

	report := Analyze(records, "all", time.UTC)

	if report.Workloads.Unclassified.Streams != 1 {
		t.Fatalf("unclassified = %d, want the Cursor conversation there", report.Workloads.Unclassified.Streams)
	}
	if got := report.Workloads.Unclassified.Reasons[0].Reason; got != workload.ReasonActivityTier {
		t.Errorf("reason = %q, want %q", got, workload.ReasonActivityTier)
	}
	var cursorReason string
	for _, m := range planFor(report.Workloads, workload.ClassLongConversation).Missing {
		if m.Route == aggregate.AgentCursor {
			cursorReason = m.Reason
		}
	}
	if cursorReason != workload.MissingActivityOnly {
		t.Errorf("cursor missing reason = %q, want %q", cursorReason, workload.MissingActivityOnly)
	}
}

// The whole fleet is accounted for: every route is either compared or explicitly
// missing. A plan that lists neither is a plan with a hole in it.
func TestAnalyze_AccountsForEveryFleetRouteInEveryShape(t *testing.T) {
	report := Analyze(growingSession("s-1", "", "una", 300, 30_000, 800, 0, 2), "all", time.UTC)

	for _, plan := range report.Workloads.Classes {
		named := len(plan.Routes) + len(plan.Missing)
		if named != len(fleetRoutes) {
			t.Errorf("shape %q accounts for %d routes, want %d", plan.Class, named, len(fleetRoutes))
		}
	}
}

func TestShapesOf_MeasuresTokensPerTurnOverTheStreamsOwnTurns(t *testing.T) {
	records := growingSession("s-1", "", "una", 4, 10_000, 0, 0, 1)

	shapes := shapesOf(session{agent: aggregate.AgentClaudeCode, turns: records})

	if len(shapes) != 1 {
		t.Fatalf("shapes = %d, want 1", len(shapes))
	}
	// Each turn carries 10,000 cache-read tokens plus the 100 of output the
	// builder writes, and tokens per turn counts everything the turn billed.
	if got := shapes[0].TokensPerTurn; got != 10_100 {
		t.Errorf("tokens per turn = %d, want 10100", got)
	}
	if shapes[0].Turns != 4 || !shapes[0].Measured {
		t.Errorf("shape = %+v, want 4 measured turns", shapes[0])
	}
}
