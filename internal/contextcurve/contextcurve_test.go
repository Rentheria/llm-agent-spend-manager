package contextcurve

import (
	"math"
	"testing"

	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
)

// pricedModel is in the pricing table, so a curve built on it is measurable.
const pricedModel = "claude-opus-5"

// sawtoothStream builds a stream shaped like the real ones on this machine:
// context climbs by growth every turn until the run ends, then something rebuilds
// the prefix and the climb starts over from baseline. The first turn of each run
// writes the prefix instead of reading it, which is exactly what a restart pays.
func sawtoothStream(model string, baseline, growth, runs, turnsPerRun int) []Turn {
	out := make([]Turn, 0, runs*turnsPerRun)
	for run := 0; run < runs; run++ {
		for i := 0; i < turnsPerRun; i++ {
			context := baseline + growth*i
			turn := Turn{Model: model, Context: context, CacheRead: context}
			if i == 0 {
				turn.CacheRead, turn.CacheWrite = 0, context
			}
			out = append(out, turn)
		}
	}
	return out
}

func TestAnalyze_MeasuresTheShapeOfTheSawtooth(t *testing.T) {
	turns := sawtoothStream(pricedModel, 50_000, 1_000, 3, 200)

	a := Analyze("s1", "", turns)

	if !a.Known {
		t.Fatalf("Known = false (%s); a 600-turn sawtooth has a measurable shape", a.Reason)
	}
	if a.Baseline != 50_000 {
		t.Errorf("Baseline = %d, want 50000 (where every reset lands)", a.Baseline)
	}
	if a.GrowthPerTurn != 1_000 {
		t.Errorf("GrowthPerTurn = %v, want 1000", a.GrowthPerTurn)
	}
	if a.Resets != 2 {
		t.Errorf("Resets = %d, want 2 (three runs are separated by two resets)", a.Resets)
	}
	if a.PeakContext != 50_000+1_000*199 {
		t.Errorf("PeakContext = %d, want %d", a.PeakContext, 50_000+1_000*199)
	}
}

// The break-even is the whole point of the package, so it gets pinned to its
// definition rather than to a hard-coded turn number: at the no-return turn the
// surcharge already paid for dragging a grown context must have passed the
// one-time cost of re-priming a fresh one, and at the turn before it must not
// have. Any change to the formula breaks one of the two.
func TestAnalyze_NoReturnTurnIsWhereDraggingPassesRestarting(t *testing.T) {
	const baseline, growth = 50_000, 1_000
	readRate, writeRate, known := pricing.ContextRates(pricedModel)
	if !known {
		t.Fatalf("ContextRates(%q) unknown; the fixture model must be priced", pricedModel)
	}
	a := Analyze("s1", "", sawtoothStream(pricedModel, baseline, growth, 3, 200))
	if !a.Known {
		t.Fatalf("Known = false (%s)", a.Reason)
	}

	// Cumulative excess over a fresh start through turn n: turn i carries
	// growth·(i−1) tokens more than a fresh start would and pays cache-read on it.
	excessThrough := func(n int) float64 {
		return readRate * growth * float64(n*(n-1)) / 2
	}
	resetCost := float64(baseline) * writeRate

	if got := excessThrough(a.NoReturnTurn); got <= resetCost {
		t.Errorf("at the no-return turn %d the excess is $%.6f, still under the $%.6f a restart costs",
			a.NoReturnTurn, got, resetCost)
	}
	if got := excessThrough(a.NoReturnTurn - 1); got > resetCost {
		t.Errorf("one turn earlier (%d) the excess is already $%.6f > $%.6f: the break-even is late",
			a.NoReturnTurn-1, got, resetCost)
	}
}

// A pricier prefix takes longer to amortize, so the break-even has to move later.
// This is what separates a real derivation from a fixed threshold: a constant
// would answer the same for both.
func TestAnalyze_BreakEvenMovesWithTheCostOfRestarting(t *testing.T) {
	cheap := Analyze("s1", "", sawtoothStream(pricedModel, 20_000, 1_000, 3, 200))
	expensive := Analyze("s2", "", sawtoothStream(pricedModel, 200_000, 1_000, 3, 200))

	if !cheap.Known || !expensive.Known {
		t.Fatalf("both curves must be measurable; got %q / %q", cheap.Reason, expensive.Reason)
	}
	if expensive.NoReturnTurn <= cheap.NoReturnTurn {
		t.Errorf("no-return turn with a 200k prefix = %d, with a 20k prefix = %d; "+
			"a prefix that costs 10x more to rebuild must be worth carrying longer",
			expensive.NoReturnTurn, cheap.NoReturnTurn)
	}
}

// Faster growth means the context bloats sooner, so cutting has to come earlier.
func TestAnalyze_BreakEvenMovesWithHowFastContextGrows(t *testing.T) {
	slow := Analyze("s1", "", sawtoothStream(pricedModel, 50_000, 200, 3, 200))
	fast := Analyze("s2", "", sawtoothStream(pricedModel, 50_000, 5_000, 3, 200))

	if !slow.Known || !fast.Known {
		t.Fatalf("both curves must be measurable; got %q / %q", slow.Reason, fast.Reason)
	}
	if fast.NoReturnTurn >= slow.NoReturnTurn {
		t.Errorf("no-return turn growing 5000 tok/turn = %d, growing 200 tok/turn = %d; "+
			"a context that bloats 25x faster must be cut sooner", fast.NoReturnTurn, slow.NoReturnTurn)
	}
}

func TestAnalyze_CountsOnlyTheTurnsThatRanPastTheBreakEven(t *testing.T) {
	const runs, turnsPerRun = 3, 200
	a := Analyze("s1", "", sawtoothStream(pricedModel, 50_000, 1_000, runs, turnsPerRun))

	if !a.Known {
		t.Fatalf("Known = false (%s)", a.Reason)
	}
	want := runs * (turnsPerRun - a.NoReturnTurn)
	if a.TurnsPastNoReturn != want {
		t.Errorf("TurnsPastNoReturn = %d, want %d (each run gets its own budget of %d turns)",
			a.TurnsPastNoReturn, want, a.NoReturnTurn)
	}
}

func TestAnalyze_ReportsNoOverrunWhenTheStreamStopsInTime(t *testing.T) {
	// One run of 25 turns, under the ~36-turn break-even for this shape.
	a := Analyze("s1", "", sawtoothStream(pricedModel, 50_000, 1_000, 1, 25))

	if !a.Known {
		t.Fatalf("Known = false (%s)", a.Reason)
	}
	if a.TurnsPastNoReturn != 0 {
		t.Errorf("TurnsPastNoReturn = %d, want 0: no run reached the break-even at turn %d",
			a.TurnsPastNoReturn, a.NoReturnTurn)
	}
	if a.SavingsUSD != 0 {
		t.Errorf("SavingsUSD = %v, want 0: there is nothing to save on a session already cut short", a.SavingsUSD)
	}
}

func TestAnalyze_SavingsStayUnderWhatTheStreamActuallyPaid(t *testing.T) {
	a := Analyze("s1", "", sawtoothStream(pricedModel, 50_000, 1_000, 3, 500))

	if !a.Known {
		t.Fatalf("Known = false (%s)", a.Reason)
	}
	if a.SavingsUSD <= 0 {
		t.Fatalf("SavingsUSD = %v, want > 0: 500-turn runs blow well past the break-even", a.SavingsUSD)
	}
	if a.SavingsUSD >= a.ContextCostUSD {
		t.Errorf("SavingsUSD = $%.2f but the stream only spent $%.2f moving context; "+
			"cutting cannot save more than was paid", a.SavingsUSD, a.ContextCostUSD)
	}
}

func TestAnalyze_PricesTheContextItActuallyMoved(t *testing.T) {
	readRate, writeRate, _ := pricing.ContextRates(pricedModel)
	turns := []Turn{
		{Model: pricedModel, Context: 1000, CacheWrite: 1000},
		{Model: pricedModel, Context: 2000, CacheRead: 2000},
	}

	a := Analyze("s1", "", turns)

	want := 1000*writeRate + 2000*readRate
	if math.Abs(a.ContextCostUSD-want) > 1e-12 {
		t.Errorf("ContextCostUSD = %v, want %v (write of the prefix plus the read back)", a.ContextCostUSD, want)
	}
}

func TestAnalyze_SaysItCannotMeasureAStreamTooShortToHaveAShape(t *testing.T) {
	a := Analyze("s1", "", []Turn{{Model: pricedModel, Context: 1000, CacheRead: 1000}})

	if a.Known {
		t.Fatalf("Known = true on a one-turn stream; a slope needs more than one point")
	}
	if a.Reason != ReasonTooShort {
		t.Errorf("Reason = %q, want %q", a.Reason, ReasonTooShort)
	}
	if a.NoReturnTurn != 0 || a.SavingsUSD != 0 {
		t.Errorf("unknown curve leaked derived values: turn %d, savings %v", a.NoReturnTurn, a.SavingsUSD)
	}
}

func TestAnalyze_SaysItCannotMeasureAnUnpricedModel(t *testing.T) {
	a := Analyze("s1", "", sawtoothStream("some-model-nobody-priced", 50_000, 1_000, 3, 200))

	if a.Known {
		t.Fatal("Known = true for a model with no price; the break-even is a cost comparison")
	}
	if a.Reason != ReasonUnpricedModel {
		t.Errorf("Reason = %q, want %q", a.Reason, ReasonUnpricedModel)
	}
}

func TestAnalyze_SaysThereIsNothingToCutWhenContextDoesNotGrow(t *testing.T) {
	turns := sawtoothStream(pricedModel, 50_000, 0, 1, 200)

	a := Analyze("s1", "", turns)

	if a.Known {
		t.Fatal("Known = true on a flat context; with no growth there is no surcharge to escape")
	}
	if a.Reason != ReasonNoGrowth {
		t.Errorf("Reason = %q, want %q", a.Reason, ReasonNoGrowth)
	}
}

func TestAnalyze_PricesTheStreamWithTheModelThatCarriedTheMostContext(t *testing.T) {
	turns := sawtoothStream(pricedModel, 50_000, 1_000, 1, 200)
	// A handful of cheap-model turns must not decide the rates: they carry almost
	// none of the context.
	for i := 0; i < 50; i++ {
		turns = append(turns, Turn{Model: "claude-haiku-4-5", Context: 100, CacheRead: 100})
	}

	a := Analyze("s1", "", turns)

	if a.Model != pricedModel {
		t.Errorf("Model = %q, want %q (the one that carried the context, not the one with more turns)",
			a.Model, pricedModel)
	}
}

func TestAnalyze_TreatsAFreeLocalModelAsHavingNothingToSave(t *testing.T) {
	a := Analyze("s1", "", sawtoothStream("nemotron-3-super", 50_000, 1_000, 3, 200))

	if a.Known {
		t.Fatal("Known = true for a local model; carrying its context costs nothing, so nothing is saved by cutting")
	}
	if a.Reason != ReasonUnpricedModel {
		t.Errorf("Reason = %q, want %q", a.Reason, ReasonUnpricedModel)
	}
}

// A report window cuts sessions in the middle. The turns before the cut are gone,
// so the first run's opening context is just where the window started — not where
// a restart lands. Taking it as the baseline would report the window's edge as a
// measurement and, because it is much larger than a real baseline, would push the
// break-even far too late.
func TestAnalyze_TakesTheBaselineFromAnObservedResetNotFromWhereTheWindowStarted(t *testing.T) {
	// A stream caught mid-run at 400k, which then resets to its real 50k baseline.
	truncated := sawtoothStream(pricedModel, 400_000, 1_000, 1, 100)
	truncated = append(truncated, sawtoothStream(pricedModel, 50_000, 1_000, 2, 200)...)

	a := Analyze("s1", "", truncated)

	if !a.Known {
		t.Fatalf("Known = false (%s)", a.Reason)
	}
	if a.Baseline != 50_000 {
		t.Errorf("Baseline = %d, want 50000: the 400k opening was the window's edge, not a restart", a.Baseline)
	}
}

// With no reset anywhere in the stream there is nothing else to use, so the first
// turn seen becomes the baseline. That can only overstate what restarting costs,
// which moves the break-even later — the safe direction.
func TestAnalyze_FallsBackToTheFirstTurnWhenNoResetWasEverObserved(t *testing.T) {
	a := Analyze("s1", "", sawtoothStream(pricedModel, 50_000, 1_000, 1, 300))

	if !a.Known {
		t.Fatalf("Known = false (%s)", a.Reason)
	}
	if a.Resets != 0 {
		t.Fatalf("Resets = %d, want 0 for this fixture", a.Resets)
	}
	if a.Baseline != 50_000 {
		t.Errorf("Baseline = %d, want 50000 (the only baseline available)", a.Baseline)
	}
}
