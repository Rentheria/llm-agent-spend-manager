package advise

import (
	"encoding/json"
	"strings"
	"testing"
)

// reportWith builds a report carrying only what the alert feed reads, so these
// tests exercise the projection and not the engine that fills it.
func reportWith(findings []Finding, escalations []Escalation) Report {
	return Report{
		Window:     "all",
		Disclaimer: "costo equivalente",
		Findings:   findings, Escalations: escalations,
	}
}

func TestAlerts_CarriesTheRecommendationAsTheAction(t *testing.T) {
	feed := Alerts(reportWith([]Finding{{
		ID: FindingCacheWasted, Title: "Caché escrita que nadie lee",
		Impact: ImpactHigh, MetricName: MetricWastedCacheShare, Metric: 0.31,
		Recommendation: "quita el caché en el lanzador", Evidence: "31% del costo",
	}}, nil))

	if len(feed.Alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(feed.Alerts))
	}
	got := feed.Alerts[0]
	if got.Kind != AlertKindFinding || got.ID != FindingCacheWasted {
		t.Errorf("expected a finding alert for E-02, got %s/%s", got.Kind, got.ID)
	}
	if got.Action != "quita el caché en el lanzador" {
		t.Errorf("the action must be the recommendation verbatim, got %q", got.Action)
	}
	if got.Metric != 0.31 {
		t.Errorf("expected the finding's own metric, got %v", got.Metric)
	}
}

func TestAlerts_CarriesTheMechanismAsTheActionOfAnEscalation(t *testing.T) {
	feed := Alerts(reportWith(nil, []Escalation{{
		FindingID: FindingCronOverhead, FindingTitle: "El cron pesa",
		MetricName: MetricCronCostShare, Mechanism: "baja el intervalo del cron",
		History: []MetricPoint{
			{Days: []string{"2026-07-01"}, Metric: 0.20},
			{Days: []string{"2026-07-04"}, Metric: 0.22},
			{Days: []string{"2026-07-07"}, Metric: 0.24},
		},
	}}))

	got := feed.Alerts[0]
	if got.Action != "baja el intervalo del cron" {
		t.Errorf("an escalation's action is its mechanism, got %q", got.Action)
	}
	if got.Metric != 0.24 {
		t.Errorf("expected the newest replay window (0.24), got %v", got.Metric)
	}
	if got.Impact != "" {
		t.Errorf("an escalation has no impact class; got %q invented", got.Impact)
	}
}

func TestAlerts_PutsEscalationsBeforeFindings(t *testing.T) {
	feed := Alerts(reportWith(
		[]Finding{{ID: FindingDominantBucket, Impact: ImpactHigh, MetricName: MetricDominantBucketShare}},
		[]Escalation{{FindingID: FindingCronOverhead, MetricName: MetricCronCostShare}},
	))

	if len(feed.Alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(feed.Alerts))
	}
	if feed.Alerts[0].Kind != AlertKindEscalation {
		t.Errorf("an escalation is the stronger claim and goes first, got %s", feed.Alerts[0].Kind)
	}
	if feed.Alerts[1].Kind != AlertKindFinding {
		t.Errorf("expected the finding second, got %s", feed.Alerts[1].Kind)
	}
}

func TestAlerts_KeepsTheOrderTheReportAlreadyDecided(t *testing.T) {
	// The report sorts its findings by impact; the feed must not re-decide it.
	feed := Alerts(reportWith([]Finding{
		{ID: FindingDominantBucket, Impact: ImpactHigh, MetricName: MetricDominantBucketShare},
		{ID: FindingModelMix, Impact: ImpactMedium, MetricName: MetricSmallTurnCostShare},
		{ID: FindingSessionChurn, Impact: ImpactLow, MetricName: MetricShortSessionShare},
	}, nil))

	want := []string{FindingDominantBucket, FindingModelMix, FindingSessionChurn}
	for i, id := range want {
		if feed.Alerts[i].ID != id {
			t.Errorf("alert %d: expected %s, got %s", i, id, feed.Alerts[i].ID)
		}
	}
}

func TestAlerts_FingerprintIsStableWhileTheReportSaysTheSameThing(t *testing.T) {
	// Same finding, same metric name, same impact class, different exact metric:
	// the number moving inside its class is not news, and re-alerting on it every
	// night is what makes an automatic alert unbearable.
	first := Alerts(reportWith([]Finding{{
		ID: FindingContextNoReturn, Impact: ImpactHigh,
		MetricName: MetricPastNoReturnCostShare, Metric: 0.41,
	}}, nil))
	second := Alerts(reportWith([]Finding{{
		ID: FindingContextNoReturn, Impact: ImpactHigh,
		MetricName: MetricPastNoReturnCostShare, Metric: 0.44,
	}}, nil))

	if first.Alerts[0].Fingerprint != second.Alerts[0].Fingerprint {
		t.Errorf("fingerprint moved without the report changing its verdict: %q vs %q",
			first.Alerts[0].Fingerprint, second.Alerts[0].Fingerprint)
	}
}

func TestAlerts_FingerprintChangesWhenTheImpactClassChanges(t *testing.T) {
	medium := Alerts(reportWith([]Finding{{
		ID: FindingContextNoReturn, Impact: ImpactMedium, MetricName: MetricPastNoReturnCostShare,
	}}, nil))
	high := Alerts(reportWith([]Finding{{
		ID: FindingContextNoReturn, Impact: ImpactHigh, MetricName: MetricPastNoReturnCostShare,
	}}, nil))

	if medium.Alerts[0].Fingerprint == high.Alerts[0].Fingerprint {
		t.Error("a finding that crossed into a worse impact class must re-alert")
	}
}

func TestAlerts_FingerprintChangesWhenTheFindingChangesWhatItMeasures(t *testing.T) {
	// Same id measuring something else is different advice, not repeated advice —
	// the same identity rule escalationFor applies.
	byCacheRead := Alerts(reportWith([]Finding{{
		ID: FindingDominantBucket, Impact: ImpactHigh,
		MetricName: MetricDominantBucketShare + ":cache-read",
	}}, nil))
	byOutput := Alerts(reportWith([]Finding{{
		ID: FindingDominantBucket, Impact: ImpactHigh,
		MetricName: MetricDominantBucketShare + ":output",
	}}, nil))

	if byCacheRead.Alerts[0].Fingerprint == byOutput.Alerts[0].Fingerprint {
		t.Error("advice about a different bucket must not pass as the same advice")
	}
}

func TestAlerts_AnEscalationDoesNotPassAsTheFindingItReplaced(t *testing.T) {
	asFinding := Alerts(reportWith([]Finding{{
		ID: FindingCronOverhead, Impact: ImpactMedium, MetricName: MetricCronCostShare,
	}}, nil))
	asEscalation := Alerts(reportWith(nil, []Escalation{{
		FindingID: FindingCronOverhead, MetricName: MetricCronCostShare,
	}}))

	if asFinding.Alerts[0].Fingerprint == asEscalation.Alerts[0].Fingerprint {
		t.Error("the day a tip escalates it is a new statement and must be delivered")
	}
}

func TestAlerts_FingerprintIsReadableRatherThanHashed(t *testing.T) {
	feed := Alerts(reportWith([]Finding{{
		ID: FindingCacheWasted, Impact: ImpactHigh, MetricName: MetricWastedCacheShare,
	}}, nil))

	got := feed.Alerts[0].Fingerprint
	for _, part := range []string{AlertKindFinding, FindingCacheWasted, MetricWastedCacheShare, ImpactHigh} {
		if !strings.Contains(got, part) {
			t.Errorf("fingerprint %q should be readable and contain %q", got, part)
		}
	}
}

func TestAlerts_EmptyReportEncodesAnEmptyListNotNull(t *testing.T) {
	encoded, err := json.Marshal(Alerts(reportWith(nil, nil)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"alerts":[]`) {
		t.Errorf("a quiet run must say [] and not null, got %s", encoded)
	}
}

func TestAlerts_KeepsTheWindowAndDisclaimerOfTheReport(t *testing.T) {
	feed := Alerts(reportWith(nil, nil))
	if feed.Window != "all" {
		t.Errorf("expected the report's window, got %q", feed.Window)
	}
	if feed.Disclaimer == "" {
		t.Error("the feed must carry the same disclaimer the report does")
	}
}
