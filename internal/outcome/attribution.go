package outcome

import (
	"sort"
	"time"
)

// AttributionLagDays is how far BACK from the day the level moved a marked change
// may sit and still be a candidate for it. A change lands, and the days from then
// on carry the new level — but a change made late in the day mostly shows up in
// the next one, so one day of slack is not enough and three starts sweeping in
// unrelated work.
//
// It is counted in ACTIVE days of the series, not calendar days, so a quiet
// weekend between the change and the shift cannot hide the change. The calendar
// span that lag resolves to is reported in From/Through, because that is what a
// reader needs in order to check which changes were in scope.
const AttributionLagDays = 2

// TemporalCaveat goes on every attribution, in these words, whatever the verdict.
// It is not a disclaimer bolted on for form: it is the one thing this layer knows
// for certain about its own output. The measurement can prove that a metric's
// level moved and that a change was made just before it moved. It cannot rule out
// that the work simply changed shape that week, and nothing in the data can.
const TemporalCaveat = "Coincidencia temporal no es causalidad: lo medido es que el nivel de la métrica " +
	"cambió y que esos cambios se hicieron justo antes. Que uno haya causado al otro NO está demostrado " +
	"aquí, y con estos datos no se puede demostrar."

// InseparableNote is what the report says when more than one marked change lands
// in the same window. Naming the most plausible one would be inventing the very
// thing the data withholds, so all of them are listed and none is credited.
const InseparableNote = "Varios cambios cayeron en la misma ventana: NO son separables con estos datos. " +
	"Se listan todos y no se le atribuye el movimiento a ninguno."

// Attribution is what the ledger can and cannot say about WHY a level moved.
type Attribution struct {
	// From/Through is the calendar span that was searched, derived from the series
	// so a reader can check the scope instead of trusting it. Through is exclusive:
	// it is the start of the day after the shift's day.
	From    time.Time `json:"from"`
	Through time.Time `json:"through"`
	// Candidates are the marked changes inside that span, oldest first. Empty is a
	// real answer: the level moved and nothing we track was done just before it, so
	// whatever moved it is not in this ledger.
	Candidates []Change `json:"candidates"`
	// Separable is true only when exactly one marked change lands in the window.
	Separable bool   `json:"separable"`
	Caveat    string `json:"caveat"`
	// InseparableNote carries InseparableNote when Separable is false and there is
	// more than one candidate, and is empty otherwise.
	InseparableNote string `json:"inseparableNote,omitempty"`
}

// Attribute finds the marked changes that were in the window when a level shift
// happened. It attributes nothing on its own: it delimits the window, lists what
// was in it, and states plainly whether the data can tell those changes apart.
//
// A shift with no located day (too small a sample) gets an empty attribution with
// the caveat still attached — there is nothing to attribute, and saying so is the
// answer.
func Attribute(shift LevelShift, changes []Change, loc *time.Location) Attribution {
	attribution := Attribution{Caveat: TemporalCaveat}
	if shift.ChangeDay == "" {
		return attribution
	}
	from, through, ok := windowFor(shift, loc)
	if !ok {
		return attribution
	}
	attribution.From, attribution.Through = from, through

	for _, change := range changes {
		if change.At.Before(from) || !change.At.Before(through) {
			continue
		}
		attribution.Candidates = append(attribution.Candidates, change)
	}
	sort.Slice(attribution.Candidates, func(i, j int) bool {
		return attribution.Candidates[i].At.Before(attribution.Candidates[j].At)
	})

	attribution.Separable = len(attribution.Candidates) == 1
	if len(attribution.Candidates) > 1 {
		attribution.InseparableNote = InseparableNote
	}
	return attribution
}

// windowFor resolves the attribution lag into a calendar span: from the start of
// the active day AttributionLagDays before the shift, through the end of the
// shift's own day. Walking back over the SERIES rather than the calendar is what
// makes the span cover the inactive days in between.
func windowFor(shift LevelShift, loc *time.Location) (time.Time, time.Time, bool) {
	changeIndex := -1
	for i, point := range shift.Series {
		if point.Day == shift.ChangeDay {
			changeIndex = i
			break
		}
	}
	if changeIndex < 0 {
		return time.Time{}, time.Time{}, false
	}

	firstDay := shift.Series[max(0, changeIndex-AttributionLagDays)].Day
	from, err := time.ParseInLocation(dayLayout, firstDay, loc)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	changeDay, err := time.ParseInLocation(dayLayout, shift.ChangeDay, loc)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return from, changeDay.AddDate(0, 0, 1), true
}

// dayLayout is the day format the whole project buckets by.
const dayLayout = "2006-01-02"
