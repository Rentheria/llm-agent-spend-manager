package advise

import (
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/contextcurve"
	"github.com/Rentheria/llm-agent-spend-manager/internal/workload"
)

// This file is the glue between the report and internal/workload: it turns the
// records the rest of advise already groups into the measured feature vector the
// classifier reads (Capa 2), and hands the result the universe of fleet routes
// it is allowed to report as missing (Capa 3). No rule and no threshold lives
// here — they live in internal/workload, in one place, with the observation each
// one came from written next to it.

// fleetRoutes is the universe of routes the plan compares against. It is passed
// in explicitly rather than read off the data on purpose: that is what lets the
// plan say "Cursor has no figure for this shape" instead of silently leaving
// Cursor out, which would read as "Cursor wasn't relevant".
var fleetRoutes = []string{
	aggregate.AgentClaudeCode,
	aggregate.AgentOpenClaw,
	aggregate.AgentCursor,
	aggregate.AgentAntigravity,
}

// workloadReport classifies every context stream in the window and builds the
// per-route savings plan for each shape.
func workloadReport(sessions []session) workload.Report {
	shapes := make([]workload.Shape, 0, len(sessions))
	for _, s := range sessions {
		shapes = append(shapes, shapesOf(s)...)
	}
	return workload.Analyze(shapes, fleetRoutes)
}

// shapesOf measures one session's context streams. The unit is (session,
// thread), not the session: a session that spawned subagents ran several
// independent contexts at once, and a shape computed over the interleaved turns
// would describe none of them (see aggregate.Record.ThreadID).
//
// The grouping is done here rather than reusing contextcap.go's streamsOf
// because a shape needs what that one drops on the floor — the per-turn cost,
// the agent and the model — and adding those to the curve's input would make the
// curve carry fields it has no use for.
func shapesOf(s session) []workload.Shape {
	type streamTurns struct {
		records []aggregate.Record
		curve   []contextcurve.Turn
	}
	byThread := map[string]*streamTurns{}
	order := make([]string, 0, 1)
	for _, r := range s.turns {
		if r.SessionID == "" {
			continue
		}
		st := byThread[r.ThreadID]
		if st == nil {
			st = &streamTurns{}
			byThread[r.ThreadID] = st
			order = append(order, r.ThreadID)
		}
		st.records = append(st.records, r)
		st.curve = append(st.curve, contextcurve.Turn{
			Model:      r.Model,
			Context:    r.Input + r.CacheRead + r.CacheWrite,
			CacheRead:  r.CacheRead,
			CacheWrite: r.CacheWrite,
		})
	}

	out := make([]workload.Shape, 0, len(order))
	for _, threadID := range order {
		st := byThread[threadID]
		out = append(out, shapeOf(s, threadID, st.records, st.curve))
	}
	return out
}

// shapeOf assembles one stream's feature vector. Activity-tier streams keep
// their turn and cost figures — they still belong to a route and still cost
// something — but Measured stays false, which is what stops the classifier from
// reading features they don't have.
func shapeOf(s session, threadID string, records []aggregate.Record, turns []contextcurve.Turn) workload.Shape {
	curve := contextcurve.Analyze(records[0].SessionID, threadID, turns)
	shape := workload.Shape{
		SessionID:     records[0].SessionID,
		ThreadID:      threadID,
		Agent:         s.agent,
		Label:         labelOf(s),
		Model:         curve.Model,
		Measured:      countMeasured(records) == len(records),
		Turns:         len(records),
		GrowthPerTurn: curve.GrowthPerTurn,
		NoReturnTurn:  curve.NoReturnTurn,
		CurveKnown:    curve.Known,
	}

	var tokens int
	for _, r := range records {
		tokens += r.PointTokens()
		shape.ContextTokens += r.Input + r.CacheRead + r.CacheWrite
		shape.CacheReadTokens += r.CacheRead
		shape.CacheWriteTokens += r.CacheWrite
		if r.CostKnown {
			shape.CostUSD += r.CostUSD
		}
	}
	if shape.Turns > 0 {
		shape.TokensPerTurn = tokens / shape.Turns
	}
	return shape
}
