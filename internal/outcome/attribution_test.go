package outcome

import (
	"strings"
	"testing"
	"time"
)

// changeOn builds a marked change at a given day and hour, in UTC, which is the
// zone these tests attribute in.
func changeOn(day string, hour int, ref string) Change {
	at, err := time.Parse(time.RFC3339, day+"T00:00:00Z")
	if err != nil {
		panic("bad fixture day: " + day)
	}
	return Change{At: at.Add(time.Duration(hour) * time.Hour), Source: SourceLog, Ref: ref}
}

// shiftAtDay10 is a level shift located on 2026-07-10 with ten consecutive active
// days around it, the shape most of these tests only need as scaffolding.
func shiftAtDay10() LevelShift {
	return LevelShift{
		Verdict:   VerdictShiftDown,
		ChangeDay: "2026-07-10",
		Series:    seriesOf(repeat(0.8, 9)...), // days 2026-07-01 … 2026-07-09
	}
}

// withDaysThrough extends a scaffold series so it actually contains ChangeDay.
func withDaysThrough(shift LevelShift, days int) LevelShift {
	shift.Series = seriesOf(repeat(0.5, days)...)
	return shift
}

func TestAttribute_NamesTheOnlyChangeInTheWindowWithoutCallingItTheCause(t *testing.T) {
	// One marked change in the window is the best case this layer ever gets, and even
	// then the answer is "único candidato", not "causa".
	shift := withDaysThrough(shiftAtDay10(), 12)
	changes := []Change{
		changeOn("2026-07-04", 9, "muy-viejo"),
		changeOn("2026-07-09", 22, "el-candidato"),
		changeOn("2026-07-11", 9, "posterior"),
	}

	attribution := Attribute(shift, changes, time.UTC)

	if len(attribution.Candidates) != 1 || attribution.Candidates[0].Ref != "el-candidato" {
		t.Fatalf("candidates = %+v, want only el-candidato", attribution.Candidates)
	}
	if !attribution.Separable {
		t.Error("one candidate should be separable")
	}
	if attribution.InseparableNote != "" {
		t.Errorf("inseparable note set with a single candidate: %q", attribution.InseparableNote)
	}
	if attribution.Caveat != TemporalCaveat {
		t.Errorf("caveat = %q, want the temporal caveat verbatim", attribution.Caveat)
	}
}

func TestAttribute_RefusesToSeparateSeveralChangesInTheSameWindow(t *testing.T) {
	// The case the fleet actually produces: a busy few days. Picking the one that
	// reads best would be inventing exactly what the data withholds.
	shift := withDaysThrough(shiftAtDay10(), 12)
	changes := []Change{
		changeOn("2026-07-09", 10, "uno"),
		changeOn("2026-07-10", 8, "dos"),
		changeOn("2026-07-10", 20, "tres"),
	}

	attribution := Attribute(shift, changes, time.UTC)

	if len(attribution.Candidates) != 3 {
		t.Fatalf("candidates = %+v, want all three", attribution.Candidates)
	}
	if attribution.Separable {
		t.Error("three changes in one window must not be reported as separable")
	}
	if attribution.InseparableNote != InseparableNote {
		t.Errorf("inseparable note = %q, want it stated in full", attribution.InseparableNote)
	}
}

func TestAttribute_SaysNothingWhenNoMarkedChangeIsInTheWindow(t *testing.T) {
	// The level moved and nothing we track happened just before. That is a real
	// finding — whatever moved it is not in this ledger — and must not be filled in
	// with the nearest change available.
	shift := withDaysThrough(shiftAtDay10(), 12)
	changes := []Change{changeOn("2026-07-01", 9, "lejano"), changeOn("2026-07-20", 9, "posterior")}

	attribution := Attribute(shift, changes, time.UTC)

	if len(attribution.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none", attribution.Candidates)
	}
	if attribution.Separable {
		t.Error("an empty window is not separable")
	}
	if attribution.Caveat == "" {
		t.Error("the caveat goes on every attribution, including the empty ones")
	}
}

func TestAttribute_WindowCoversTheCalendarGapBetweenActiveDays(t *testing.T) {
	// The lag is counted in ACTIVE days. Here the fleet was quiet for a week before
	// the shift, so walking two active days back lands nine calendar days earlier —
	// and a change made during that silence is still a candidate, because no measured
	// day sits between it and the shift.
	shift := LevelShift{
		Verdict:   VerdictShiftDown,
		ChangeDay: "2026-07-10",
		Series: []Point{
			{Day: "2026-07-01", Value: 0.8},
			{Day: "2026-07-02", Value: 0.8},
			{Day: "2026-07-10", Value: 0.2},
			{Day: "2026-07-11", Value: 0.2},
		},
	}
	changes := []Change{changeOn("2026-07-05", 12, "hecho-en-el-hueco")}

	attribution := Attribute(shift, changes, time.UTC)

	if len(attribution.Candidates) != 1 || attribution.Candidates[0].Ref != "hecho-en-el-hueco" {
		t.Errorf("candidates = %+v, want the change made during the quiet week", attribution.Candidates)
	}
	if got := attribution.From.Format(dayLayout); got != "2026-07-01" {
		t.Errorf("window starts %s, want 2026-07-01 (two active days back from the shift)", got)
	}
	if got := attribution.Through.Format(dayLayout); got != "2026-07-11" {
		t.Errorf("window ends (exclusive) %s, want the start of 2026-07-11", got)
	}
}

func TestAttribute_AttributesNothingWhenThereIsNoLocatedDay(t *testing.T) {
	// A sample too small to place a change point has nothing to attribute. The caveat
	// still goes out: it is true of this output too.
	attribution := Attribute(LevelShift{Verdict: VerdictInsufficientSample}, []Change{
		changeOn("2026-07-10", 9, "cualquiera"),
	}, time.UTC)

	if len(attribution.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none without a change day", attribution.Candidates)
	}
	if attribution.Caveat != TemporalCaveat {
		t.Error("the caveat is missing from an empty attribution")
	}
}

func TestTemporalCaveat_SaysCoincidenceIsNotCausality(t *testing.T) {
	// The wording is the deliverable, not decoration: this is the sentence that keeps
	// a reader from turning a coincidence in time into a conclusion. A refactor that
	// softens it changes what the tool claims.
	for _, phrase := range []string{"Coincidencia temporal no es causalidad", "NO está demostrado"} {
		if !strings.Contains(TemporalCaveat, phrase) {
			t.Errorf("the temporal caveat no longer contains %q: %q", phrase, TemporalCaveat)
		}
	}
}
