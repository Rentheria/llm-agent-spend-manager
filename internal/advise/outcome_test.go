package advise

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/outcome"
)

// cacheReadDay is one day whose cost is almost entirely cache-read: a turn that
// drags a large cached context and writes a small answer. cacheRead sets how much
// of the day's cost lands in that bucket, which is what moves the tracked series
// without changing the shape of anything else.
func cacheReadDay(day, cacheRead int) aggregate.Record {
	return measuredTurn(fmt.Sprintf("read-%d", day), day, [4]int{1_000, 1_000, 0, cacheRead})
}

func outcomeFor(ledger OutcomeLedger, series string) (Outcome, bool) {
	for _, o := range ledger.Outcomes {
		if o.Series == series {
			return o, true
		}
	}
	return Outcome{}, false
}

// buildLedger runs the whole chain the CLI runs: analyze, then grade.
func buildLedger(records []aggregate.Record, changes []outcome.Change) OutcomeLedger {
	report := Analyze(records, string(aggregate.WindowAll), time.UTC)
	return BuildOutcomeLedger(records, report, changes, time.UTC)
}

func TestBuildOutcomeLedger_DetectsTheDayACostShareChangedLevel(t *testing.T) {
	// Twelve active days: six where cache-read is a sliver of the bill and six where
	// it carries almost all of it. That is a change of level, and the ledger has to
	// put it on the first day of the second block.
	//
	// Six and six, not four and four, so the step sits well inside the range the
	// change-point search can reach. A step parked at the earliest evaluable day
	// would be found by a broken locator that always returns that day.
	var records []aggregate.Record
	for day := 10; day < 16; day++ {
		records = append(records, cacheReadDay(day, 1_000))
	}
	for day := 16; day < 22; day++ {
		records = append(records, cacheReadDay(day, 10_000_000))
	}

	ledger := buildLedger(records, nil)

	got, ok := outcomeFor(ledger, SeriesCostShare(BucketCacheRead))
	if !ok {
		t.Fatalf("no outcome for the cache-read series; got %+v", ledger.Outcomes)
	}
	if got.Shift.Verdict != outcome.VerdictShiftUp {
		t.Errorf("verdict = %q, want %q (cache-read went from a sliver to nearly all of the bill)",
			got.Shift.Verdict, outcome.VerdictShiftUp)
	}
	if got.Shift.ChangeDay != "2026-07-16" {
		t.Errorf("change day = %q, want 2026-07-16 (the first day of the second block)", got.Shift.ChangeDay)
	}
	if got.Shift.DaysBefore != 6 || got.Shift.DaysAfter != 6 {
		t.Errorf("split = %d/%d days, want 6/6", got.Shift.DaysBefore, got.Shift.DaysAfter)
	}
}

func TestBuildOutcomeLedger_PinsE01ToTheSeriesThatMeasuresIt(t *testing.T) {
	// The whole point of the layer: the advice about the dominant bucket gets graded
	// against the daily series of that same bucket's cost share, not against a
	// stand-in.
	var records []aggregate.Record
	for day := 10; day < 20; day++ {
		records = append(records, cacheReadDay(day, 10_000_000))
	}

	ledger := buildLedger(records, nil)

	got, ok := outcomeFor(ledger, SeriesCostShare(BucketCacheRead))
	if !ok {
		t.Fatal("no outcome for the cache-read series")
	}
	if got.FindingID != FindingDominantBucket {
		t.Errorf("cache-read series is pinned to %q, want %s", got.FindingID, FindingDominantBucket)
	}
	for _, u := range ledger.Unmeasured {
		if u.FindingID == FindingDominantBucket {
			t.Errorf("%s was also reported as unmeasured: %+v", FindingDominantBucket, u)
		}
	}
}

func TestBuildOutcomeLedger_DailySeriesAgreesWithTheFindingsOwnMetric(t *testing.T) {
	// The guard against drift. The daily series and the finding's Metric are computed
	// in different places, and if they ever stop meaning the same thing, the ledger
	// would be grading advice against a number that isn't its own. Over a stretch of
	// identical days, every daily point must equal the finding's metric exactly.
	//
	// Eight active days on purpose: one short of the three full windows the
	// recurrence check needs, so E-01 is still a tip with a Metric on it instead of
	// having escalated (see escalation.go).
	const activeDays = 8
	var records []aggregate.Record
	for day := 10; day < 10+activeDays; day++ {
		records = append(records, cacheReadDay(day, 10_000_000))
	}
	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	series := dailySeriesOf(records, SeriesCostShare(BucketCacheRead), time.UTC)

	finding, ok := findingByID(report, FindingDominantBucket)
	if !ok {
		t.Fatalf("fixture does not fire %s, so there is nothing to compare: %+v", FindingDominantBucket, report.Findings)
	}
	if len(series) != activeDays {
		t.Fatalf("series has %d points, want one per active day", len(series))
	}
	for _, point := range series {
		if math.Abs(point.Value-finding.Metric) > 1e-12 {
			t.Errorf("day %s measures %v but %s reports %v for the same quantity — the two definitions have drifted",
				point.Day, point.Value, FindingDominantBucket, finding.Metric)
		}
	}
}

func TestBuildOutcomeLedger_LeavesUnmeasurableDaysOutOfTheSeries(t *testing.T) {
	// A day whose only activity is activity-tier (no token buckets, no priced cost)
	// has no share to measure. Putting a 0 there would read as "cache-read dropped to
	// nothing that day", which is a number nobody measured.
	records := []aggregate.Record{
		cacheReadDay(10, 10_000_000),
		{
			Agent: aggregate.AgentCursor, Mode: aggregate.ModeEditor, SessionID: "cursor-1",
			Timestamp: time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC),
			// Activity tier: an estimate range, never per-bucket tokens (see T10).
			Confidence: aggregate.ConfidenceActivity, TokensLow: 1_000, TokensHigh: 3_000,
		},
		cacheReadDay(12, 10_000_000),
	}

	series := dailySeriesOf(records, SeriesCostShare(BucketCacheRead), time.UTC)

	if len(series) != 2 {
		t.Fatalf("series = %+v, want only the two days with priced cost", series)
	}
	for _, point := range series {
		if point.Day == "2026-07-11" {
			t.Errorf("the activity-only day is in the series with value %v; it has nothing to measure", point.Value)
		}
	}
}

func TestBuildOutcomeLedger_ReportsSessionMetricsAsUnmeasurableInsteadOfFakingThem(t *testing.T) {
	// E-02 is a share of session-level cost. Cutting a session at midnight to get a
	// daily value would produce an artifact of the cut, so this layer refuses and says
	// which advice it is refusing to grade.
	var records []aggregate.Record
	for day := 10; day < 20; day++ {
		records = append(records, measuredTurn(fmt.Sprintf("waste-%d", day), day, [4]int{0, 100, 200_000, 0}))
	}
	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	ledger := BuildOutcomeLedger(records, report, nil, time.UTC)

	var found bool
	for _, u := range ledger.Unmeasured {
		if u.FindingID == FindingCacheWasted {
			found = true
			if u.Reason == "" {
				t.Error("unmeasured advice carries no reason; a silent omission reads as a pass")
			}
		}
	}
	if !found {
		t.Errorf("%s is neither graded nor listed as unmeasurable: %+v", FindingCacheWasted, ledger)
	}
}

func TestBuildOutcomeLedger_CarriesTheChangesThatWereInTheWindow(t *testing.T) {
	// End to end: the level moves, and the change made the day before it shows up as
	// the candidate — with the caveat attached and no causal claim made.
	var records []aggregate.Record
	for day := 10; day < 14; day++ {
		records = append(records, cacheReadDay(day, 10_000_000))
	}
	for day := 14; day < 18; day++ {
		records = append(records, cacheReadDay(day, 1_000))
	}
	changes := []outcome.Change{{
		At:     time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC),
		Source: outcome.SourceLog, Ref: "T70", Actor: "claude-code", Note: "tope de contexto cableado",
	}}

	ledger := BuildOutcomeLedger(records, Analyze(records, string(aggregate.WindowAll), time.UTC), changes, time.UTC)

	got, _ := outcomeFor(ledger, SeriesCostShare(BucketCacheRead))
	if got.Shift.Verdict != outcome.VerdictShiftDown {
		t.Fatalf("verdict = %q, want %q", got.Shift.Verdict, outcome.VerdictShiftDown)
	}
	if len(got.Attribution.Candidates) != 1 || got.Attribution.Candidates[0].Ref != "T70" {
		t.Errorf("candidates = %+v, want the T70 entry", got.Attribution.Candidates)
	}
	if !got.Attribution.Separable {
		t.Error("a single change in the window should be reported as separable")
	}
	if got.Attribution.Caveat != outcome.TemporalCaveat {
		t.Error("the temporal caveat did not survive into the ledger")
	}
}

func TestBuildOutcomeLedger_IsDeterministic(t *testing.T) {
	// Same records and same changes in, same ledger out — nothing remembered between
	// runs, the same property escalation.go rests on.
	var records []aggregate.Record
	for day := 10; day < 20; day++ {
		records = append(records, cacheReadDay(day, 10_000_000*day))
	}

	first, second := buildLedger(records, nil), buildLedger(records, nil)

	if len(first.Outcomes) != len(second.Outcomes) {
		t.Fatalf("outcome count changed between identical runs: %d then %d", len(first.Outcomes), len(second.Outcomes))
	}
	for i := range first.Outcomes {
		if first.Outcomes[i].Shift.Verdict != second.Outcomes[i].Shift.Verdict ||
			first.Outcomes[i].Shift.ChangeDay != second.Outcomes[i].Shift.ChangeDay {
			t.Errorf("outcome %s differs between identical runs: %+v vs %+v",
				first.Outcomes[i].Series, first.Outcomes[i].Shift, second.Outcomes[i].Shift)
		}
	}
}
