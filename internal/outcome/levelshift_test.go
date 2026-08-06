package outcome

import (
	"fmt"
	"math"
	"testing"
)

// seriesOf builds a series of consecutive days from bare values, which is all any
// of these tests care about: the detection reads the order and the numbers, never
// the calendar.
func seriesOf(values ...float64) []Point {
	points := make([]Point, 0, len(values))
	for i, v := range values {
		points = append(points, Point{Day: fmt.Sprintf("2026-07-%02d", i+1), Value: v})
	}
	return points
}

// repeat is the fixture shorthand for "this many days at this level".
func repeat(value float64, days int) []float64 {
	out := make([]float64, days)
	for i := range out {
		out[i] = value
	}
	return out
}

func TestDetectLevelShift_CanBeRedoneByHand(t *testing.T) {
	// The claim that makes this a measurement and not a heuristic: every figure in
	// the verdict is arithmetic anyone can check with a calculator.
	//
	// Five days at 0.8/0.6/0.8/0.6/0.7, then five at 0.3/0.1/0.3/0.1/0.2. Each side
	// averages 0.7 and 0.2. Each side's deviations from its own mean are ±0.1, ±0.1
	// and 0, so each sum of squares is 0.04 and each sample variance is 0.04/4 =
	// 0.01. The pooled standard deviation is therefore sqrt(0.01) = 0.1, and the two
	// means sit (0.2 - 0.7) / 0.1 = 5 pooled sigmas apart.
	series := seriesOf(0.8, 0.6, 0.8, 0.6, 0.7, 0.3, 0.1, 0.3, 0.1, 0.2)

	shift := DetectLevelShift(series)

	if shift.Verdict != VerdictShiftDown {
		t.Fatalf("verdict = %q, want %q", shift.Verdict, VerdictShiftDown)
	}
	if shift.ChangeDay != "2026-07-06" {
		t.Errorf("change day = %q, want the first day of the lower level (2026-07-06)", shift.ChangeDay)
	}
	if shift.DaysBefore != 5 || shift.DaysAfter != 5 {
		t.Errorf("split = %d/%d days, want 5/5", shift.DaysBefore, shift.DaysAfter)
	}
	assertClose(t, "meanBefore", shift.MeanBefore, 0.7)
	assertClose(t, "meanAfter", shift.MeanAfter, 0.2)
	assertClose(t, "pooledStdDev", shift.PooledStdDev, 0.1)
	assertClose(t, "delta", shift.Delta, -0.5)
	assertClose(t, "shiftStdDevs", shift.ShiftStdDevs, -5)
}

func TestDetectLevelShift_CallsAMoveInsideTheNoiseNoChange(t *testing.T) {
	// The means differ by a third (0.6 to 0.4), which looks like a lot until you
	// notice the metric swings between 0.1 and 0.9 every day. A verdict here would
	// be a statement about a Tuesday.
	series := seriesOf(0.9, 0.1, 0.8, 0.6, 0.1, 0.9, 0.2, 0.4)

	shift := DetectLevelShift(series)

	if shift.Verdict != VerdictNoShift {
		t.Errorf("verdict = %q, want %q — the gap between the means is smaller than the dispersion inside them "+
			"(Δ %.3f vs σ %.3f)", shift.Verdict, VerdictNoShift, shift.Delta, shift.PooledStdDev)
	}
}

func TestDetectLevelShift_SaysInsufficientSampleInsteadOfGuessing(t *testing.T) {
	// A clean, enormous step — but with three days on the low side there is no mean
	// worth comparing. "Insufficient sample" is the right answer, not a failure.
	series := seriesOf(0.9, 0.9, 0.9, 0.9, 0.1, 0.1, 0.1)

	shift := DetectLevelShift(series)

	if shift.Verdict != VerdictInsufficientSample {
		t.Errorf("verdict = %q, want %q with only %d days", shift.Verdict, VerdictInsufficientSample, len(series))
	}
	if shift.ChangeDay != "" {
		t.Errorf("change day = %q; a sample this small should not name a day at all", shift.ChangeDay)
	}
}

func TestDetectLevelShift_FindsTheDayTheStepHappened(t *testing.T) {
	// The step sits at day 11 of 20, far from either edge, so nothing but the CUSUM
	// walk can be putting it there.
	values := append(repeat(0.20, 10), repeat(0.50, 10)...)

	shift := DetectLevelShift(seriesOf(values...))

	if shift.ChangeDay != "2026-07-11" {
		t.Errorf("change day = %q, want 2026-07-11 (the first day at the new level)", shift.ChangeDay)
	}
	if shift.DaysBefore != 10 || shift.DaysAfter != 10 {
		t.Errorf("split = %d/%d days, want 10/10", shift.DaysBefore, shift.DaysAfter)
	}
	if shift.Verdict != VerdictShiftUp {
		t.Errorf("verdict = %q, want %q", shift.Verdict, VerdictShiftUp)
	}
}

func TestDetectLevelShift_HandlesACleanStepWithNoDispersion(t *testing.T) {
	// Two flat levels: every day on each side is identical, so the pooled standard
	// deviation is exactly zero. Dividing by it would produce an infinity that no
	// report can render, and calling it "no change" would be plainly wrong — the
	// verdict has to come out of comparing the gap against the dispersion as a
	// product, not a ratio.
	values := append(repeat(0.80, 5), repeat(0.20, 5)...)

	shift := DetectLevelShift(seriesOf(values...))

	if shift.PooledStdDev != 0 {
		t.Fatalf("fixture is not exercising the zero-dispersion case: σ = %v", shift.PooledStdDev)
	}
	if shift.Verdict != VerdictShiftDown {
		t.Errorf("verdict = %q, want %q", shift.Verdict, VerdictShiftDown)
	}
	if math.IsInf(shift.ShiftStdDevs, 0) || math.IsNaN(shift.ShiftStdDevs) {
		t.Errorf("shiftStdDevs = %v; a report cannot render that, and JSON cannot encode it", shift.ShiftStdDevs)
	}
}

func TestDetectLevelShift_IgnoresAFlatSeries(t *testing.T) {
	// Nothing moved at all. The peak of the CUSUM walk is zero everywhere, so the
	// day it names is arbitrary — what must not happen is a verdict.
	shift := DetectLevelShift(seriesOf(repeat(0.42, 12)...))

	if shift.Verdict != VerdictNoShift {
		t.Errorf("verdict = %q, want %q for a series that never moved", shift.Verdict, VerdictNoShift)
	}
	if shift.Delta != 0 {
		t.Errorf("delta = %v, want 0", shift.Delta)
	}
}

func TestDetectLevelShift_IsDeterministic(t *testing.T) {
	// Same series in, same verdict out, with nothing remembered between calls — the
	// same property the recurrence check in internal/advise rests on.
	series := seriesOf(0.7, 0.65, 0.72, 0.68, 0.3, 0.28, 0.31, 0.29, 0.30, 0.27)

	first, second := DetectLevelShift(series), DetectLevelShift(series)

	if first.Verdict != second.Verdict || first.ChangeDay != second.ChangeDay ||
		first.ShiftStdDevs != second.ShiftStdDevs {
		t.Errorf("two identical calls disagreed:\n%+v\n%+v", first, second)
	}
}

// assertClose compares floats at a tolerance far tighter than any figure the
// report prints, so it fails on a real arithmetic change and not on the last bit.
func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	const tolerance = 1e-9
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
