// Package contextcurve measures what a conversation pays to carry its own
// context, and derives the turn at which carrying it stops paying for itself.
//
// It exists because the fleet's dominant cost bucket is cache-read (63.7% of the
// equivalent cost, measured 2026-07-27) and cache-read scales with
// (context size × turns): not with what an agent writes, but with how much
// history every single turn has to drag along. A per-session total cannot see
// that — two sessions with the same cost can have completely different shapes,
// and only one of them gets cheaper by being cut shorter. So the report needed a
// second measurement next to "what did it cost": "how much was it carrying, and
// for how long".
//
// The shape this finds in practice is a sawtooth: context climbs turn after turn
// until something rebuilds the prefix (a compaction, a restart), drops back to a
// small baseline, and climbs again. Two numbers describe it — where a reset lands
// (Baseline) and how fast the climb is (GrowthPerTurn) — and those two, priced
// with the shared table, are all the break-even needs.
//
// Everything here is derived from observed turns. When a stream is too short to
// measure a shape, or its model has no price, the result says so (Known=false,
// with the reason) instead of estimating: this tool does not invent numbers
// (docs/calibracion.md). The method is documented in docs/automejora.md §7.
package contextcurve

import (
	"math"
	"sort"

	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
)

// resetDropRatio is the fraction of the previous turn's context below which a
// turn is treated as a reset rather than a continuation: the prefix was rebuilt
// (a compaction, a /clear, a resumed session re-priming).
//
// Half is not a guess. Measured over the 12 largest real transcripts on this
// machine (2026-07-27): a running thread's context is almost perfectly monotonic
// — only 8 turn-to-turn decreases in ~12,000 main-thread turns. Six of those
// collapsed to ≤0.12 of the previous turn (real rebuilds) and two were mild noise
// at 0.75 and 0.96. Nothing at all landed between 0.12 and 0.75, so the cut sits
// in the middle of an empty gap and moving it anywhere inside that gap changes no
// classification.
const resetDropRatio = 0.5

// minRunTurns is the shortest growth run worth deriving a slope from: two points
// give a slope that a single noisy turn can set to anything.
const minRunTurns = 3

// minNoReturnTurn floors the break-even at two turns. A "cut every turn" answer
// is arithmetically possible when a prefix is tiny and growth is huge, but a run
// of one turn has nothing to cut — it IS a fresh start.
const minNoReturnTurn = 2

// Reasons a stream yields no break-even point. They are user-facing wording: the
// report prints the reason where the number would go, so a missing measurement
// reads as missing instead of as zero.
const (
	ReasonTooShort      = "la sesión es muy corta para medir cómo crece su contexto"
	ReasonNoGrowth      = "el contexto no crece turno a turno: no hay nada que cortar"
	ReasonUnpricedModel = "el modelo no está en la tabla de precios"
)

// Turn is one assistant turn of a SINGLE context stream, in chronological order.
// Turns from different streams must never be mixed: a session that spawned
// subagents runs several independent contexts at once, and interleaving them
// produces a curve that describes nothing (see aggregate.Record.ThreadID).
type Turn struct {
	Model string
	// Context is everything the turn carried in: input + cache-read + cache-write
	// tokens. That sum is the context-window occupancy the provider billed for.
	Context int
	// CacheRead and CacheWrite split the cached part of Context. Reading the
	// prefix back is what a long conversation pays on every turn; writing it is
	// what a fresh start pays once. The break-even sits between exactly those two.
	CacheRead  int
	CacheWrite int
}

// Run is one uninterrupted growth run — the turns between two context resets,
// i.e. one tooth of the sawtooth.
type Run struct {
	Turns        int `json:"turns"`
	StartContext int `json:"startContext"`
	PeakContext  int `json:"peakContext"`
}

// Analysis is one context stream's measured shape plus the break-even derived
// from it. Every field above Known is measured from the turns; the ones below it
// are derived, and are all zero when Known is false.
type Analysis struct {
	SessionID string `json:"sessionId"`
	ThreadID  string `json:"threadId,omitempty"`
	Turns     int    `json:"turns"`
	// Model is the one that carried the most context in this stream; its rates are
	// what the break-even is priced with.
	Model         string `json:"model"`
	PeakContext   int    `json:"peakContext"`
	MedianContext int    `json:"medianContext"`
	// Resets counts how many times the context collapsed and started over. Zero
	// means the stream grew once and stopped.
	Resets int `json:"resets"`
	// ContextCostUSD is what this stream actually paid to move its context around:
	// observed cache-read plus cache-write at list price. Measured, not modelled.
	ContextCostUSD float64 `json:"contextCostUsd"`

	// Baseline is the context a reset lands on and GrowthPerTurn is how fast the
	// climb from there is, both medians over the runs long enough to measure.
	Baseline      int     `json:"baselineContext"`
	GrowthPerTurn float64 `json:"growthPerTurn"`
	// NoReturnTurn answers "when should this have been cut": the turn at which the
	// surcharge already paid for dragging a grown context passes what re-priming a
	// fresh one costs. Past it, every further turn pays more to keep the history
	// than a fresh start would have cost.
	NoReturnTurn int `json:"noReturnTurn"`
	// TurnsPastNoReturn is how many turns ran beyond it, summed over every run.
	// This is the number that makes the finding actionable.
	TurnsPastNoReturn int `json:"turnsPastNoReturn"`
	// SavingsUSD is ContextCostUSD minus what the same turns would have cost if the
	// stream had been cut every NoReturnTurn turns. The observed side is exact; the
	// counterfactual is built from this stream's own Baseline and GrowthPerTurn,
	// nothing borrowed from elsewhere.
	SavingsUSD float64 `json:"savingsUsd"`

	// Known is false when no break-even could be derived; Reason says why.
	Known  bool   `json:"known"`
	Reason string `json:"reason,omitempty"`
}

// Analyze measures one context stream. turns must be in chronological order and
// belong to a single stream.
func Analyze(sessionID, threadID string, turns []Turn) Analysis {
	a := Analysis{
		SessionID:      sessionID,
		ThreadID:       threadID,
		Turns:          len(turns),
		Model:          dominantModel(turns),
		PeakContext:    peakContext(turns),
		MedianContext:  medianContext(turns),
		ContextCostUSD: observedContextUSD(turns),
	}
	if len(turns) == 0 {
		a.Reason = ReasonTooShort
		return a
	}

	runs := growthRuns(turns)
	a.Resets = len(runs) - 1

	readRate, writeRate, priced := pricing.ContextRates(a.Model)
	if !priced || readRate <= 0 {
		a.Reason = ReasonUnpricedModel
		return a
	}
	baseline, growth, measurable := shapeOf(runs)
	if !measurable {
		a.Reason = ReasonTooShort
		return a
	}
	a.Baseline, a.GrowthPerTurn = baseline, growth

	cut, ok := noReturnTurn(baseline, growth, readRate, writeRate)
	if !ok {
		a.Reason = ReasonNoGrowth
		return a
	}
	a.NoReturnTurn = cut
	a.TurnsPastNoReturn = turnsPast(runs, cut)
	a.SavingsUSD = savingsOf(a, cut, readRate, writeRate)
	a.Known = true
	return a
}

// savingsOf is what cutting every cut turns would have avoided: the observed
// context cost minus the counterfactual one. Floored at zero — a negative figure
// would mean the counterfactual model disagrees with what actually happened, and
// reporting that as a loss would be reading precision the model doesn't have.
func savingsOf(a Analysis, cut int, readRate, writeRate float64) float64 {
	saved := a.ContextCostUSD - counterfactualContextUSD(a.Turns, cut, a.Baseline, a.GrowthPerTurn, readRate, writeRate)
	if saved < 0 {
		return 0
	}
	return saved
}

// noReturnTurn solves for the first turn t at which the surcharge already paid
// for dragging a grown context passes the one-time cost of re-priming a fresh
// one:
//
//	readRate · growth · t(t−1)/2  >  baseline · writeRate
//
// The left side is the cumulative excess over a fresh start (turn i carries
// growth·(i−1) tokens more than a fresh start would, and pays cache-read on all
// of it); the right side is what that fresh start costs, once. Solved in closed
// form rather than by walking the turns, so the answer describes the session's
// shape instead of depending on how far it happened to run.
//
// ok is false when the context doesn't grow: then there is no surcharge to
// accumulate and cutting never pays for itself.
func noReturnTurn(baseline int, growth, readRate, writeRate float64) (int, bool) {
	if growth <= 0 {
		return 0, false
	}
	// t(t−1) > target, solved with the quadratic formula.
	target := 2 * float64(baseline) * writeRate / (readRate * growth)
	t := int(math.Ceil((1 + math.Sqrt(1+4*target)) / 2))
	if t < minNoReturnTurn {
		return minNoReturnTurn, true
	}
	return t, true
}

// counterfactualContextUSD is what turns turns would have cost in context if the
// stream had been cut every cutEvery turns: each run pays once to prime a fresh
// baseline prefix, then reads that prefix back plus whatever the run has grown
// since.
func counterfactualContextUSD(turns, cutEvery, baseline int, growth, readRate, writeRate float64) float64 {
	var total float64
	for remaining := turns; remaining > 0; remaining -= cutEvery {
		runTurns := min(cutEvery, remaining)
		total += float64(baseline) * writeRate
		for i := 1; i < runTurns; i++ {
			total += (float64(baseline) + growth*float64(i)) * readRate
		}
	}
	return total
}

// growthRuns splits a stream at every context reset. A reset is a turn whose
// context collapsed to a fraction of the previous turn's, which can only mean the
// prefix was rebuilt — a running thread's context never shrinks on its own.
func growthRuns(turns []Turn) []Run {
	if len(turns) == 0 {
		return nil
	}
	var runs []Run
	current := runFrom(turns[0])
	previous := turns[0].Context
	for _, t := range turns[1:] {
		if isReset(previous, t.Context) {
			runs = append(runs, current)
			current = runFrom(t)
		} else {
			current.Turns++
			current.PeakContext = max(current.PeakContext, t.Context)
		}
		previous = t.Context
	}
	return append(runs, current)
}

func runFrom(t Turn) Run {
	return Run{Turns: 1, StartContext: t.Context, PeakContext: t.Context}
}

func isReset(previous, current int) bool {
	return float64(current) < float64(previous)*resetDropRatio
}

// shapeOf derives the two numbers that describe the sawtooth: where a reset lands
// and how fast the context climbs from there. Both are medians over the runs long
// enough to measure, so one runaway run can't set either. ok is false when no run
// is long enough — then the stream has no measurable shape and says so.
func shapeOf(runs []Run) (baseline int, growth float64, ok bool) {
	starts := make([]int, 0, len(runs))
	slopes := make([]float64, 0, len(runs))
	for i, r := range runs {
		if r.Turns < minRunTurns {
			continue
		}
		// Only a run that a reset was observed landing on carries a real baseline.
		// The FIRST run began before the data did — before the report's window, or
		// before the transcript — so its first turn is wherever we started looking,
		// not where a restart lands. Counting it would report the window's edge as
		// if it were a measurement.
		if i > 0 {
			starts = append(starts, r.StartContext)
		}
		slopes = append(slopes, float64(r.PeakContext-r.StartContext)/float64(r.Turns-1))
	}
	if len(slopes) == 0 {
		return 0, 0, false
	}
	if len(starts) == 0 {
		// No reset was observed at all, so the first turn seen is the only baseline
		// available. It can only OVERstate what restarting costs, which pushes the
		// break-even later — the report errs toward saying "keep going", never
		// toward inventing an overrun.
		starts = append(starts, runs[0].StartContext)
	}
	return medianInt(starts), medianFloat(slopes), true
}

// turnsPast counts the turns that ran beyond the break-even, summed over every
// run: each run gets its own budget of cut turns before it starts overpaying.
func turnsPast(runs []Run, cut int) int {
	past := 0
	for _, r := range runs {
		if r.Turns > cut {
			past += r.Turns - cut
		}
	}
	return past
}

// observedContextUSD is what the stream really paid to move context: cache-read
// plus cache-write at each turn's own model price. Turns on an unpriced model
// contribute nothing rather than a guessed figure.
func observedContextUSD(turns []Turn) float64 {
	var total float64
	for _, t := range turns {
		readRate, writeRate, known := pricing.ContextRates(t.Model)
		if !known {
			continue
		}
		total += float64(t.CacheRead)*readRate + float64(t.CacheWrite)*writeRate
	}
	return total
}

// dominantModel is the model that carried the most context tokens in the stream.
// Picking by context (not by turn count) matches what the break-even is about: a
// handful of huge turns decide the bill, not a majority of tiny ones.
func dominantModel(turns []Turn) string {
	byModel := map[string]int{}
	for _, t := range turns {
		byModel[t.Model] += t.Context
	}
	best, bestContext := "", -1
	for model, context := range byModel {
		// Ties break on the name so the same stream always reports the same model.
		if context > bestContext || (context == bestContext && model < best) {
			best, bestContext = model, context
		}
	}
	return best
}

func peakContext(turns []Turn) int {
	peak := 0
	for _, t := range turns {
		peak = max(peak, t.Context)
	}
	return peak
}

func medianContext(turns []Turn) int {
	contexts := make([]int, 0, len(turns))
	for _, t := range turns {
		contexts = append(contexts, t.Context)
	}
	return medianInt(contexts)
}

func medianInt(values []int) int {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	return sorted[len(sorted)/2]
}

func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}
