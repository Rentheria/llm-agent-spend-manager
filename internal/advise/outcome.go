package advise

import (
	"sort"
	"strings"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/outcome"
)

// Series ids the outcome ledger follows day by day. They are SERIES, not finding
// metrics: a finding metric is the one number a tip exists to move, measured over
// the report's window; a series is that same kind of quantity measured every
// active day so a change of level can be located in time.
const (
	SeriesCostPerTurn     = "cost-per-turn"
	seriesCostSharePrefix = "cost-share:"
)

// SeriesCostShare names the series that follows one billable bucket's share of the
// priced equivalent cost.
func SeriesCostShare(bucket string) string { return seriesCostSharePrefix + bucket }

// trackedSeries is what the outcome ledger measures day by day.
//
// All five are pure per-TURN sums over measured, priced records. That is a
// deliberate limit, not an oversight: a metric defined over sessions (how many
// died in two turns, how much context a conversation dragged) has no honest daily
// value, because a session that runs past midnight would have to be cut in half to
// get one, and half a session looks short. The daily number would be an artifact
// of the cut rather than a measurement, so those metrics get no series and the
// report says so instead.
var trackedSeries = []string{
	SeriesCostPerTurn,
	SeriesCostShare(BucketCacheRead),
	SeriesCostShare(BucketOutput),
	SeriesCostShare(BucketCacheWrite),
	SeriesCostShare(BucketInput),
}

// NoSeriesReason is what the report prints for a piece of advice whose metric has
// no daily series. It is the same rule the rest of the project follows: a missing
// measurement reads as missing, never as a zero or as "no change".
const NoSeriesReason = "esta métrica se define por sesión, y una sesión partida a medianoche no da un valor " +
	"diario honesto — sin serie diaria no hay nivel que comparar"

// Outcome closes the loop for one tracked series: what was measured day by day,
// whether the level moved, and which real changes were in the window when it did.
type Outcome struct {
	Series string `json:"series"`
	// FindingID is the advice this series measures the effect of, when the report
	// carries one for it, and empty when the series stands on its own (still worth
	// following: a level change nobody advised is exactly what a reader wants to
	// know about). Escalated says that advice had already stopped being a tip
	// because it kept firing without moving — see escalation.go.
	FindingID   string              `json:"findingId,omitempty"`
	FindingText string              `json:"findingText,omitempty"`
	Escalated   bool                `json:"escalated"`
	Shift       outcome.LevelShift  `json:"shift"`
	Attribution outcome.Attribution `json:"attribution"`
}

// UnmeasuredAdvice is a piece of advice the outcome layer cannot follow, and why.
// It is reported rather than omitted: a ledger that silently listed only the
// advice it could grade would read as if it had graded everything.
type UnmeasuredAdvice struct {
	FindingID  string `json:"findingId"`
	MetricName string `json:"metricName"`
	Escalated  bool   `json:"escalated"`
	Reason     string `json:"reason"`
}

// OutcomeLedger is the whole outcome payload: one entry per tracked series, plus
// the advice that has no series to be graded against.
type OutcomeLedger struct {
	Outcomes   []Outcome          `json:"outcomes"`
	Unmeasured []UnmeasuredAdvice `json:"unmeasured"`
}

// BuildOutcomeLedger measures every tracked series over the records, tests each
// one for a change of level, and looks up which marked changes were in the window
// when it moved.
//
// It is a pure function of (records, report, changes): same inputs, same ledger,
// nothing remembered between runs — the same rule the recurrence check follows
// (see escalation.go). The changes are a parameter because they are I/O from
// outside the usage data (git and the fleet log), and this package reads no files.
func BuildOutcomeLedger(records []aggregate.Record, report Report,
	changes []outcome.Change, loc *time.Location) OutcomeLedger {
	advice := adviceBySeries(report)
	ledger := OutcomeLedger{
		Outcomes:   make([]Outcome, 0, len(trackedSeries)),
		Unmeasured: unmeasuredAdvice(report),
	}
	for _, series := range trackedSeries {
		shift := outcome.DetectLevelShift(dailySeriesOf(records, series, loc))
		entry := Outcome{
			Series:      series,
			Shift:       shift,
			Attribution: outcome.Attribute(shift, changes, loc),
		}
		if graded, hasAdvice := advice[series]; hasAdvice {
			entry.FindingID, entry.FindingText, entry.Escalated = graded.id, graded.text, graded.escalated
		}
		ledger.Outcomes = append(ledger.Outcomes, entry)
	}
	return ledger
}

// dailySeriesOf measures one series for every active day, oldest first. Days with
// nothing priced to measure are absent from the series rather than present as a
// zero — the same rule the trend and the recurrence check use, so a quiet weekend
// cannot read as a change of level.
func dailySeriesOf(records []aggregate.Record, series string, loc *time.Location) []outcome.Point {
	byDay := recordsByDay(records, loc)
	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)

	points := make([]outcome.Point, 0, len(days))
	for _, day := range days {
		if value, measured := dayValue(series, byDay[day]); measured {
			points = append(points, outcome.Point{Day: day, Value: value})
		}
	}
	return points
}

// dayValue computes one series over one day's records, reusing the same bucket
// arithmetic every other figure in the report comes from (see sumBuckets and
// bucketList) so a daily value and a window value can never disagree about what
// they mean.
//
// It returns false when the day carries no priced measured cost at all: a share of
// no cost is not zero, it is unmeasurable.
func dayValue(series string, records []aggregate.Record) (float64, bool) {
	tokens, costs := sumBuckets(records)
	total := costs.Total()
	if total <= 0 {
		return 0, false
	}
	if series == SeriesCostPerTurn {
		turns := pricedTurns(records)
		if turns == 0 {
			return 0, false
		}
		return total / float64(turns), true
	}

	bucket, isCostShare := strings.CutPrefix(series, seriesCostSharePrefix)
	if !isCostShare {
		return 0, false
	}
	for _, b := range bucketList(tokens, costs) {
		if b.Bucket == bucket {
			return b.Share, true
		}
	}
	return 0, false
}

// pricedTurns counts the turns whose cost is inside the sums sumBuckets returns,
// so cost per turn is divided by the turns that actually produced that cost and
// not by activity-tier records that contributed none.
func pricedTurns(records []aggregate.Record) int {
	n := 0
	for _, r := range records {
		if r.Confidence == aggregate.ConfidenceMeasured && r.CostKnown {
			n++
		}
	}
	return n
}

// gradedAdvice is one finding pinned to the series that measures its effect.
type gradedAdvice struct {
	id        string
	text      string
	escalated bool
}

// adviceBySeries indexes the report's advice by the series that measures the same
// quantity its own metric does.
func adviceBySeries(report Report) map[string]gradedAdvice {
	out := map[string]gradedAdvice{}
	for _, f := range report.Findings {
		if series := seriesForMetric(f.MetricName); series != "" {
			out[series] = gradedAdvice{id: f.ID, text: f.Title}
		}
	}
	for _, e := range report.Escalations {
		if series := seriesForMetric(e.MetricName); series != "" {
			out[series] = gradedAdvice{id: e.FindingID, text: e.FindingTitle, escalated: true}
		}
	}
	return out
}

// seriesForMetric maps a finding's metric to the tracked series that measures the
// same thing day by day, or "" when none does.
//
// Only E-01 has one today, and it is an exact match rather than an approximation:
// its metric IS a billable bucket's share of the priced equivalent cost, computed
// by the same sumBuckets/bucketList pair the daily series uses. Everything else on
// the report measures a share of SESSIONS or of session-level cost, and see
// trackedSeries for why those get no daily series.
func seriesForMetric(metricName string) string {
	name, bucket, hasBucket := strings.Cut(metricName, ":")
	if name == MetricDominantBucketShare && hasBucket {
		return SeriesCostShare(bucket)
	}
	return ""
}

// unmeasuredAdvice lists the report's advice that has no series behind it, so the
// ledger admits its own coverage instead of implying it graded everything.
func unmeasuredAdvice(report Report) []UnmeasuredAdvice {
	out := make([]UnmeasuredAdvice, 0, len(report.Findings)+len(report.Escalations))
	for _, f := range report.Findings {
		if seriesForMetric(f.MetricName) == "" {
			out = append(out, UnmeasuredAdvice{FindingID: f.ID, MetricName: f.MetricName, Reason: NoSeriesReason})
		}
	}
	for _, e := range report.Escalations {
		if seriesForMetric(e.MetricName) == "" {
			out = append(out, UnmeasuredAdvice{
				FindingID: e.FindingID, MetricName: e.MetricName, Escalated: true, Reason: NoSeriesReason,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FindingID < out[j].FindingID })
	return out
}
