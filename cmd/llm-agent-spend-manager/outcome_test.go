package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/advise"
	"github.com/Rentheria/llm-agent-spend-manager/internal/outcome"
)

// aShiftDown is a level change with the numbers already worked out, so these tests
// are about what the report SAYS and not about the detection.
func aShiftDown() outcome.LevelShift {
	return outcome.LevelShift{
		Verdict:      outcome.VerdictShiftDown,
		ChangeDay:    "2026-07-14",
		DaysBefore:   6,
		DaysAfter:    8,
		MeanBefore:   0.64,
		MeanAfter:    0.31,
		PooledStdDev: 0.09,
		Delta:        -0.33,
		DeltaPct:     -51.6,
		ShiftStdDevs: -3.67,
		CusumPeak:    1.23,
		Series: []outcome.Point{
			{Day: "2026-07-12", Value: 0.64}, {Day: "2026-07-13", Value: 0.63},
			{Day: "2026-07-14", Value: 0.31}, {Day: "2026-07-15", Value: 0.30},
		},
	}
}

func aChange(ref, note string) outcome.Change {
	return outcome.Change{
		At:     time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC),
		Source: outcome.SourceLog, Ref: ref, Actor: "claude-code", Note: note,
	}
}

func TestWriteLevelShifts_SpellsOutTheArithmeticBehindTheVerdict(t *testing.T) {
	// A reader has to be able to redo the count, so the two means, their day counts
	// and the dispersion they were judged against all have to be on the page.
	var buf bytes.Buffer
	shifted := advise.Outcome{
		Series: advise.SeriesCostShare(advise.BucketCacheRead),
		Shift:  aShiftDown(),
	}

	writeLevelShifts(&buf, []advise.Outcome{shifted})

	out := buf.String()
	for _, want := range []string{"BAJÓ DE NIVEL", "2026-07-14", "64.0%", "31.0%", "9.0%", "CUSUM", "-3.7σ"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteLevelShifts_SaysItOnlyReportsOneShiftPerMetric(t *testing.T) {
	// The detection returns the single largest step in the series. A reader who
	// doesn't know that would read "sin cambio de nivel" as "nothing ever moved".
	var buf bytes.Buffer

	writeLevelShifts(&buf, []advise.Outcome{{Series: advise.SeriesCostPerTurn, Shift: aShiftDown()}})

	if !strings.Contains(buf.String(), "UN cambio de nivel por métrica") {
		t.Errorf("the report does not admit it only surfaces one shift per metric:\n%s", buf.String())
	}
}

func TestWriteCandidates_RefusesToPickAmongSeveralChanges(t *testing.T) {
	// Several changes in one window is the case the fleet actually produces. The
	// report lists them all and credits none, and says both things out loud.
	var buf bytes.Buffer
	attribution := outcome.Attribute(aShiftDown(), []outcome.Change{
		aChange("T70", "escalamiento"), aChange("T76", "tope de contexto"),
	}, time.UTC)

	writeCandidates(&buf, attribution)

	out := buf.String()
	for _, want := range []string{"T70", "T76", "NO son separables", "Coincidencia temporal no es causalidad"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteCandidates_SaysSoWhenNothingMarkedIsInTheWindow(t *testing.T) {
	// The level moved and nothing tracked happened just before it. Printing an empty
	// list would read like an oversight; saying the cause is not in the ledger is the
	// actual finding.
	var buf bytes.Buffer

	writeCandidates(&buf, outcome.Attribute(aShiftDown(), nil, time.UTC))

	out := buf.String()
	if !strings.Contains(out, "ninguno") || !strings.Contains(out, "no está en esta bitácora") {
		t.Errorf("output does not say the cause is absent from the ledger:\n%s", out)
	}
	if !strings.Contains(out, "Coincidencia temporal no es causalidad") {
		t.Errorf("the caveat is missing even here:\n%s", out)
	}
}

func TestWriteAdviceLoop_CallsAnInsufficientSampleWhatItIs(t *testing.T) {
	// "Muestra insuficiente" is a valid, expected result. It must read as one and not
	// as a broken report.
	var buf bytes.Buffer
	ledger := advise.OutcomeLedger{Outcomes: []advise.Outcome{{
		Series:      advise.SeriesCostShare(advise.BucketCacheRead),
		FindingID:   advise.FindingDominantBucket,
		FindingText: "El 64% del costo equivalente se va en un solo bucket: cache-read",
		Shift:       outcome.LevelShift{Verdict: outcome.VerdictInsufficientSample},
	}}}

	writeAdviceLoop(&buf, ledger)

	out := buf.String()
	if !strings.Contains(out, "muestra insuficiente") {
		t.Errorf("output does not name the verdict:\n%s", out)
	}
	if !strings.Contains(out, "No es un fallo") {
		t.Errorf("output does not say an insufficient sample is a result, not a failure:\n%s", out)
	}
}

func TestWriteAdviceLoop_AdmitsTheAdviceItCannotGrade(t *testing.T) {
	// A ledger that listed only the advice it could grade would read as a verdict on
	// the whole report.
	var buf bytes.Buffer
	ledger := advise.OutcomeLedger{Unmeasured: []advise.UnmeasuredAdvice{{
		FindingID:  advise.FindingCacheWasted,
		MetricName: advise.MetricWastedCacheShare,
		Reason:     advise.NoSeriesReason,
	}}}

	writeAdviceLoop(&buf, ledger)

	out := buf.String()
	for _, want := range []string{"SIN SERIE DIARIA", advise.FindingCacheWasted, "Por qué:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteMarkedChanges_ShowsWhatItCouldNotRead(t *testing.T) {
	// Without the unreadable count, "no marked change in that window" and "we could
	// not see that part of the log" look identical on the page.
	var buf bytes.Buffer
	changes := outcome.ChangeLedger{
		Changes:       []outcome.Change{aChange("T70", "algo")},
		Repos:         []string{"llm-agent-spend-manager"},
		Commits:       12,
		LogEntries:    783,
		LogUnreadable: 42,
		LogNotAChange: 336,
	}

	writeMarkedChanges(&buf, changes)

	out := buf.String()
	for _, want := range []string{"CAMBIOS MARCADOS", "42", "447", "12"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteLevelShifts_ShowsNoNumbersForASampleItCouldNotSplit(t *testing.T) {
	// An insufficient sample has no means. The zeroed struct fields must not reach the
	// page as $0.0000 — a figure nobody measured, which is exactly what this project
	// refuses to print.
	var buf bytes.Buffer
	tooSmall := advise.Outcome{
		Series: advise.SeriesCostPerTurn,
		Shift:  outcome.LevelShift{Verdict: outcome.VerdictInsufficientSample, Series: []outcome.Point{{Day: "2026-07-27"}}},
	}

	writeLevelShifts(&buf, []advise.Outcome{tooSmall})

	if strings.Contains(buf.String(), "$0.0000") {
		t.Errorf("printed a mean for a sample it could not split:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "muestra insuficiente") {
		t.Errorf("output does not name the verdict:\n%s", buf.String())
	}
}
