package advise

import (
	"strings"
	"time"
)

// Alert kinds. They are the two things on the report that ask for an action:
// advice that still stands as advice, and advice that stopped being advice (see
// escalation.go). Everything else in the report is context for reading those.
const (
	AlertKindFinding    = "finding"
	AlertKindEscalation = "escalation"
)

// fingerprintSeparator joins the fields of a fingerprint. A pipe rather than a
// hash: the fingerprint is meant to be read in a log and understood without
// running anything, so "why did this fire again" is answerable by eye.
const fingerprintSeparator = "|"

// Alert is one item of the report reduced to what a notifier outside this binary
// needs: what fired, the one number it exists to move, what to do about it, and
// a fingerprint that answers "is this the same item I already delivered?".
//
// It carries no cost figure on purpose. A notifier that leads with dollars
// invites the reader to rank alerts by money, and SavingsUSD is an upper bound
// rather than a promise (docs/automejora.md §4) — a number that reads as a
// promise the moment it lands in a chat message with no report around it.
type Alert struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
	// Impact is the report's own severity class for a finding, and empty for an
	// escalation: Escalation carries no impact, and inventing one here would be
	// this file deciding something the engine deliberately does not.
	Impact     string `json:"impact,omitempty"`
	MetricName string `json:"metricName"`
	// Metric is the finding's metric over the report's window. For an escalation
	// it is the NEWEST replay window instead (the last block of active days the
	// recurrence check compared), because that is the number its verdict was read
	// off. The two never appear for the same id in one feed — an escalated
	// finding is no longer emitted as a finding — so there is nothing to compare
	// across windows by accident.
	Metric float64 `json:"metric"`
	// Action is the recommendation for a finding and the mechanism for an
	// escalation. Both come verbatim from the report; nothing is rephrased here.
	Action      string `json:"action"`
	Evidence    string `json:"evidence"`
	Fingerprint string `json:"fingerprint"`
}

// AlertFeed is the whole machine-facing payload: the actionable items of one
// report, in the order the report already put them in.
//
// It exists because the full JSON report is the wrong thing to hand a notifier
// that runs unattended every night — it carries the per-agent tables, the daily
// series and the workload plan, all of which change on every single run, so
// "did anything change?" is unanswerable from it without reimplementing half of
// this package on the other side.
type AlertFeed struct {
	Window      string    `json:"window"`
	GeneratedAt time.Time `json:"generatedAt"`
	Alerts      []Alert   `json:"alerts"`
	Disclaimer  string    `json:"disclaimer"`
}

// Alerts reduces a report to its actionable items.
//
// It is a pure projection of the report: no state, no file, no memory of what a
// previous run emitted — the same rule the recurrence check follows
// (escalation.go). Which items were already delivered is the NOTIFIER's
// business, and deliberately not this binary's: keeping a seen-set here would
// make the report a function of its own execution history, and "this tool
// measures, it does not learn" is exactly the property that lets a
// reader re-run the report and get the same answer.
//
// Call it after WithBootFiles, or E-08 will be missing from the feed.
func Alerts(report Report) AlertFeed {
	feed := AlertFeed{
		Window:      report.Window,
		GeneratedAt: report.GeneratedAt,
		Disclaimer:  report.Disclaimer,
		// Non-nil so an empty feed encodes as [] and not as null: a notifier
		// reading null has to guess whether it means "nothing fired" or "the
		// field is missing", and those deserve different behaviour.
		Alerts: make([]Alert, 0, len(report.Escalations)+len(report.Findings)),
	}
	// Escalations first, and not because they are "more urgent" — because they
	// are the stronger claim. A finding says "try this"; an escalation says "this
	// was already tried and the number didn't move". Nothing is re-sorted beyond
	// that: the report already ordered its findings by impact, and re-deciding
	// the order here would be a second copy of that policy.
	for _, e := range report.Escalations {
		feed.Alerts = append(feed.Alerts, alertFromEscalation(e))
	}
	for _, f := range report.Findings {
		feed.Alerts = append(feed.Alerts, alertFromFinding(f))
	}
	return feed
}

func alertFromFinding(f Finding) Alert {
	return Alert{
		Kind:        AlertKindFinding,
		ID:          f.ID,
		Title:       f.Title,
		Impact:      f.Impact,
		MetricName:  f.MetricName,
		Metric:      f.Metric,
		Action:      f.Recommendation,
		Evidence:    f.Evidence,
		Fingerprint: findingFingerprint(f),
	}
}

func alertFromEscalation(e Escalation) Alert {
	return Alert{
		Kind:        AlertKindEscalation,
		ID:          e.FindingID,
		Title:       e.FindingTitle,
		MetricName:  e.MetricName,
		Metric:      newestMetric(e),
		Action:      e.Mechanism,
		Evidence:    e.Evidence,
		Fingerprint: escalationFingerprint(e),
	}
}

// newestMetric reads the last window of the series the escalation carries. An
// Escalation is only built from a full History (see escalationFor), so the empty
// case cannot happen through Analyze; it is handled anyway rather than indexed
// blindly, and it returns a zero that no caller can mistake for a measurement
// because the escalation's own Evidence prints the whole series.
func newestMetric(e Escalation) float64 {
	if len(e.History) == 0 {
		return 0
	}
	return e.History[len(e.History)-1].Metric
}

// findingFingerprint identifies a finding as an ALERT: the same fingerprint means
// "you have already been told this", a different one means the report is saying
// something new.
//
// The impact class is part of it, and that is the whole design. Re-alerting on
// every run is noise; never re-alerting hides a problem that got worse. The
// middle ground needs a rule for "worse enough to say it again", and inventing a
// threshold for that would be inventing policy. So it reuses the one the report
// already declares: impactOf quantizes the cost touched into high/medium/low at
// impactHighShare and impactMediumShare, thresholds that are written down with
// their reasoning in advise.go. A finding re-alerts exactly when the report
// itself changes its mind about how bad it is — no new constant, and the reason
// for the re-alert is legible in the fingerprint that changed.
//
// The metric name is in there too because a finding that changed what it
// measures is different advice, not the same advice repeated — the same identity
// rule escalationFor applies.
func findingFingerprint(f Finding) string {
	return strings.Join([]string{AlertKindFinding, f.ID, f.MetricName, f.Impact}, fingerprintSeparator)
}

// escalationFingerprint identifies an escalation. It has no impact component,
// unlike a finding: an escalation is terminal. It does not get worse on its way
// to being fixed, it just stays until a mechanism lands, and a severity that
// wobbled window over window would re-alert about the same unfixed gap for a
// reason that isn't news.
//
// The kind is part of the string, so a finding and the escalation of that same
// finding can never collide: the day E-07 escalates, the fingerprint changes,
// and the notifier delivers it — which is correct, because it is now a different
// statement about the same id.
func escalationFingerprint(e Escalation) string {
	return strings.Join([]string{AlertKindEscalation, e.FindingID, e.MetricName}, fingerprintSeparator)
}
