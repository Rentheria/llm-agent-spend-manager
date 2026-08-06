package quota

import (
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// window builds one drained-then-refused quota window: turns of the given model
// totalling roughly the given tokens, plus the refusal that closed it.
func window(day int, model string, tokens int, turnCount int) ([]aggregate.Record, claudecode.LimitEvent) {
	start := time.Date(2026, 6, day, 8, 0, 0, 0, time.UTC)
	records := make([]aggregate.Record, 0, turnCount)
	for i := 0; i < turnCount; i++ {
		records = append(records, aggregate.Record{
			Agent:      aggregate.AgentClaudeCode,
			Confidence: aggregate.ConfidenceMeasured,
			Timestamp:  start.Add(time.Duration(i) * time.Minute),
			Model:      model,
			Output:     tokens / turnCount,
		})
	}
	refusedAt := start.Add(time.Duration(turnCount) * time.Minute)
	return records, claudecode.LimitEvent{Timestamp: refusedAt, ResetAt: start.Add(claudecode.WindowLength)}
}

func fleet(days []int, model string, tokens []int) ([]aggregate.Record, []claudecode.LimitEvent) {
	var records []aggregate.Record
	var events []claudecode.LimitEvent
	for i, day := range days {
		r, e := window(day, model, tokens[i], 10)
		records = append(records, r...)
		events = append(events, e)
	}
	return records, events
}

func TestCalibrate_RefusesToGuessBelowThreeObservations(t *testing.T) {
	records, events := fleet([]int{1, 2}, "claude-opus-5", []int{100_000, 120_000})

	got := Calibrate(records, events, claudecode.WindowLength)
	if got.Capacity.Known {
		t.Error("reported a calibrated ceiling from 2 observations")
	}
	if !strings.Contains(got.Note, "hacen falta") {
		t.Errorf("the note must say what is missing, got: %q", got.Note)
	}
}

func TestCalibrate_SummarizesTheObservedCeilingWithItsSpread(t *testing.T) {
	records, events := fleet([]int{1, 2, 3}, "claude-opus-5", []int{100_000, 200_000, 300_000})

	got := Calibrate(records, events, claudecode.WindowLength)
	if !got.Capacity.Known {
		t.Fatal("3 observations should produce a calibrated ceiling")
	}
	if got.Capacity.Source != SourceCalibrated {
		t.Errorf("source = %q, want %q", got.Capacity.Source, SourceCalibrated)
	}
	if got.Capacity.Limit.Point != 200_000 || got.Capacity.Limit.Low != 100_000 || got.Capacity.Limit.High != 300_000 {
		t.Errorf("limit = %+v, want median 200k over 100k–300k", got.Capacity.Limit)
	}
	if got.Capacity.Limit.Exact {
		t.Error("a calibrated ceiling must never be marked exact")
	}
	if got.Capacity.Dispersion <= maxTrustedDispersion {
		t.Fatalf("dispersion = %v, expected this spread to exceed the trust threshold", got.Capacity.Dispersion)
	}
	if !strings.Contains(got.Note, "orientativo") {
		t.Errorf("a ceiling this scattered must be labeled orientative, got: %q", got.Note)
	}
}

// The account fails once and every agent on it writes its own refusal within
// seconds. Counting those separately would treat one wall as several.
func TestCalibrate_CollapsesSimultaneousRefusalsIntoOneObservation(t *testing.T) {
	records, events := fleet([]int{1, 2, 3}, "claude-opus-5", []int{100_000, 200_000, 300_000})
	echo := events[0]
	echo.Timestamp = echo.Timestamp.Add(4 * time.Second)
	events = append(events, echo)

	got := Calibrate(records, events, claudecode.WindowLength)
	if len(got.Observations) != 3 {
		t.Errorf("observations = %d, want 3: the echoed refusal is the same wall", len(got.Observations))
	}
}

func TestCalibrate_WithdrawsAModelWeightThatHasNothingToCompareAgainst(t *testing.T) {
	// Three windows dominated by one model, tight enough to pass the dispersion
	// gate — the shape that used to yield a meaningless "×1.00".
	records, events := fleet([]int{1, 2, 3}, "claude-opus-4-8", []int{200_000, 210_000, 205_000})

	got := Calibrate(records, events, claudecode.WindowLength)
	if len(got.ModelWeights) != 1 {
		t.Fatalf("model weights = %d, want 1", len(got.ModelWeights))
	}
	w := got.ModelWeights[0]
	if w.Derivable {
		t.Errorf("a lone model was reported as derivable at ×%.2f; a weight is a ratio", w.Weight)
	}
	if !strings.Contains(w.Reason, "comparar") {
		t.Errorf("reason must explain the missing comparison, got: %q", w.Reason)
	}
}

func TestCalibrate_ReportsWhyAMixedWindowSaysNothingAboutAnyModel(t *testing.T) {
	records, events := fleet([]int{1, 2, 3}, "claude-opus-5", []int{100_000, 200_000, 300_000})
	// Half the last window is a different model: no model dominates it.
	for i := range records[len(records)-5:] {
		records[len(records)-5+i].Model = "claude-sonnet-5"
	}

	got := Calibrate(records, events, claudecode.WindowLength)
	for _, w := range got.ModelWeights {
		if w.Derivable {
			t.Errorf("%s was weighed from windows it did not dominate", w.Model)
		}
		if w.Reason == "" {
			t.Errorf("%s is non-derivable with no reason given", w.Model)
		}
	}
}

func TestObservation_ElapsedIsHowLongTheWindowActuallySurvived(t *testing.T) {
	records, events := fleet([]int{1, 2, 3}, "claude-opus-5", []int{100_000, 200_000, 300_000})

	got := Calibrate(records, events, claudecode.WindowLength)
	for _, o := range got.Observations {
		if o.Elapsed != 10*time.Minute {
			t.Errorf("elapsed = %s, want 10m", o.Elapsed)
		}
		if o.Elapsed >= claudecode.WindowLength {
			t.Error("a window cannot survive longer than its own length")
		}
	}
}
