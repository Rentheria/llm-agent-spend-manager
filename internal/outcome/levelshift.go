// Package outcome closes the loop the advice layer leaves open.
//
// internal/advise can already say "this is not improving" (see its recurrence
// check: a tip that fired three windows running without moving its own metric).
// What it cannot say is "this improved AFTER that change, and that other change
// did nothing" — because it never knew which changes were made. This package
// supplies the missing half:
//
//  1. Marked changes — the commits of the fleet's repositories and the entries of
//     the fleet's own log. Real events with real timestamps, never inferred.
//  2. Level-change detection — whether a metric's mean moved by more than the
//     day-to-day dispersion around it, and on which day (CUSUM).
//  3. Honest attribution — which marked changes were in the window when the level
//     moved, whether they can be told apart, and the standing reminder that a
//     coincidence in time is not a cause.
//
// Everything here is arithmetic over measured data: two means, two standard
// deviations, a cumulative sum. No model is fitted and no number is invented, and
// every intermediate figure is carried in the result so the whole verdict can be
// redone by hand off the same series. That is the point — this layer's product is
// a dataset a human can audit, and it is the prerequisite for anything
// statistical that might come later, not a substitute for it.
package outcome

import "math"

// Point is one active day of a metric. Days with nothing to measure are absent
// from a series rather than present with a zero: a share of no cost is not zero,
// it is unmeasurable, and a fabricated zero is exactly the kind of number this
// project refuses to produce.
type Point struct {
	Day   string  `json:"day"`
	Value float64 `json:"value"`
}

// Verdicts a level-change test can reach. "Insufficient sample" is a result, not
// a failure: with a handful of active days on either side of a candidate day, the
// two means being different says nothing, and saying nothing is correct.
const (
	VerdictShiftDown          = "shift-down"
	VerdictShiftUp            = "shift-up"
	VerdictNoShift            = "no-shift"
	VerdictInsufficientSample = "insufficient-sample"
)

const (
	// minSideDays is how many active days each side of a candidate change day needs
	// before its mean is worth comparing. Below four, the "mean" is two or three
	// days of work and one heavy day sets it, so a difference between the two sides
	// would be a statement about noise.
	minSideDays = 4

	// minShiftStdDevs is how far apart the two means must be, measured in pooled
	// standard deviations, before the move counts as a change of level rather than
	// day-to-day variation. One sigma is deliberately modest: this test exists to
	// shortlist candidates for a human to read, not to publish a significance
	// claim, and being too strict here hides real steps behind normal variance.
	minShiftStdDevs = 1.0
)

// LevelShift is the whole calculation, not just its verdict. Every intermediate
// number the verdict rests on is a field, and Series carries the days it was
// computed from, so anyone can redo the arithmetic with a calculator and get the
// same answer. A verdict nobody can check is an opinion.
type LevelShift struct {
	Verdict string `json:"verdict"`
	// ChangeDay is the first day of the AFTER side: the day the level is measured
	// to have moved. Empty when the sample was too small to look for one.
	ChangeDay  string `json:"changeDay,omitempty"`
	DaysBefore int    `json:"daysBefore"`
	DaysAfter  int    `json:"daysAfter"`

	MeanBefore   float64 `json:"meanBefore"`
	MeanAfter    float64 `json:"meanAfter"`
	StdDevBefore float64 `json:"stdDevBefore"`
	StdDevAfter  float64 `json:"stdDevAfter"`
	// PooledStdDev is the dispersion the difference of means is judged against:
	// the two sides' sample variances combined, weighted by their degrees of
	// freedom. It answers "is this bigger than how much this metric normally
	// wobbles day to day".
	PooledStdDev float64 `json:"pooledStdDev"`

	Delta    float64 `json:"delta"`
	DeltaPct float64 `json:"deltaPct"`
	// ShiftStdDevs is Delta over PooledStdDev, the figure the verdict reads. It is
	// 0 when the dispersion is 0 (a perfectly clean step has no within-side
	// variance at all); the verdict handles that case without it — see
	// DetectLevelShift.
	ShiftStdDevs float64 `json:"shiftStdDevs"`
	// CusumPeak is the largest absolute cumulative deviation from the series mean,
	// in the metric's own units. It is the quantity ChangeDay maximizes, kept so
	// the choice of day is as checkable as the means are.
	CusumPeak float64 `json:"cusumPeak"`

	Series []Point `json:"series"`
}

// DetectLevelShift answers "did this metric change level, and when" over a series
// of active days, oldest first.
//
// Two steps, both hand-checkable. WHERE: a CUSUM walk locates the day the series
// splits into two regimes. HOW MUCH: the means of the two sides are compared
// against the dispersion inside them. A move smaller than that dispersion is not
// a change of level, it is a Tuesday.
func DetectLevelShift(series []Point) LevelShift {
	shift := LevelShift{Verdict: VerdictInsufficientSample, Series: series}
	if len(series) < 2*minSideDays {
		return shift
	}

	split, peak := cusumChangePoint(series)
	before, after := valuesOf(series[:split]), valuesOf(series[split:])
	shift.ChangeDay = series[split].Day
	shift.CusumPeak = peak
	shift.DaysBefore, shift.DaysAfter = len(before), len(after)
	shift.MeanBefore, shift.StdDevBefore = mean(before), stdDev(before)
	shift.MeanAfter, shift.StdDevAfter = mean(after), stdDev(after)
	shift.PooledStdDev = pooledStdDev(before, after)
	shift.Delta = shift.MeanAfter - shift.MeanBefore
	if shift.MeanBefore > 0 {
		shift.DeltaPct = shift.Delta / shift.MeanBefore * 100
	}
	if shift.PooledStdDev > 0 {
		shift.ShiftStdDevs = shift.Delta / shift.PooledStdDev
	}

	// Written as a product instead of a division so the zero-dispersion case needs
	// no special rule: a metric that never wobbled and then sat at a different
	// value is a clean step, and any non-zero Delta is the right answer there.
	if math.Abs(shift.Delta) <= minShiftStdDevs*shift.PooledStdDev {
		shift.Verdict = VerdictNoShift
		return shift
	}
	if shift.Delta < 0 {
		shift.Verdict = VerdictShiftDown
	} else {
		shift.Verdict = VerdictShiftUp
	}
	return shift
}

// cusumChangePoint locates the day a series most likely changed level, the
// textbook CUSUM way: walk it accumulating each day's deviation from the series
// mean, and take the day right after the accumulation is farthest from zero. A
// run of days above the mean pushes the sum up and a run below pulls it down, so
// its extreme is where the two regimes meet. It returns that day's index and the
// peak itself.
//
// Only splits that leave minSideDays on both sides are candidates: a change day
// with three days after it is one this test cannot evaluate, so offering it would
// only produce a verdict of "insufficient sample" about the wrong day.
func cusumChangePoint(series []Point) (int, float64) {
	seriesMean := mean(valuesOf(series))
	best, peak := minSideDays, 0.0
	var running float64
	for i := 0; i < len(series)-1; i++ {
		running += series[i].Value - seriesMean
		split := i + 1
		if split < minSideDays || len(series)-split < minSideDays {
			continue
		}
		if math.Abs(running) > peak {
			best, peak = split, math.Abs(running)
		}
	}
	return best, peak
}

func valuesOf(series []Point) []float64 {
	out := make([]float64, 0, len(series))
	for _, p := range series {
		out = append(out, p.Value)
	}
	return out
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// stdDev is the sample standard deviation (dividing by n-1): the series is a
// sample of the fleet's days, not the population of every day it could have had.
func stdDev(values []float64) float64 {
	return math.Sqrt(variance(values))
}

func variance(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	m := mean(values)
	var sumSquares float64
	for _, v := range values {
		sumSquares += (v - m) * (v - m)
	}
	return sumSquares / float64(len(values)-1)
}

// pooledStdDev combines the two sides' variances weighted by their degrees of
// freedom — the standard two-sample pooled estimate. It is the right scale for
// "did the level move" because it measures variation WITHIN each regime, not the
// variation the step itself creates.
func pooledStdDev(before, after []float64) float64 {
	degreesOfFreedom := len(before) + len(after) - 2
	if degreesOfFreedom < 1 {
		return 0
	}
	weighted := float64(len(before)-1)*variance(before) + float64(len(after)-1)*variance(after)
	return math.Sqrt(weighted / float64(degreesOfFreedom))
}
