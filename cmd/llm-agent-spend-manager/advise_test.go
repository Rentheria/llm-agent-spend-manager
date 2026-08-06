package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Rentheria/llm-agent-spend-manager/internal/advise"
)

func anEscalation() advise.Escalation {
	return advise.Escalation{
		FindingID:      advise.FindingCacheWasted,
		FindingTitle:   "9 sesiones escribieron caché y nunca la leyeron",
		Windows:        3,
		WindowDays:     3,
		MetricName:     advise.MetricWastedCacheShare,
		MetricDeltaPct: 0.4,
		Evidence:       "El consejo E-02 se emitió en 3 ventanas seguidas…",
		Mechanism:      "Fierro: quitar el caché en el lanzador de las tareas de un solo disparo.",
	}
}

func TestWriteEscalations_NamesTheMechanismInsteadOfRepeatingTheTip(t *testing.T) {
	var buf bytes.Buffer

	writeEscalations(&buf, []advise.Escalation{anEscalation()})

	out := buf.String()
	for _, want := range []string{"BRECHA DE ARQUITECTURA", advise.FindingCacheWasted, "Qué falta:", "Fierro:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteEscalations_StaysSilentWithNothingToEscalate(t *testing.T) {
	var buf bytes.Buffer

	writeEscalations(&buf, nil)

	if buf.Len() != 0 {
		t.Errorf("printed a section with no escalations: %q", buf.String())
	}
}

func TestCmdAdvise_RefusesBothOutputFlagsBeforeReadingAnything(t *testing.T) {
	// The check has to run before the command touches disk: an unattended caller
	// that asked for the wrong payload should learn it immediately, not after a
	// full collection pass.
	var buf bytes.Buffer

	if code := cmdAdvise([]string{"--json", "--alerts"}, &buf); code != 2 {
		t.Errorf("expected usage exit code 2, got %d", code)
	}
	if !strings.Contains(buf.String(), "mutually exclusive") {
		t.Errorf("the refusal must say why:\n%s", buf.String())
	}
}

func TestEncodeJSON_IndentsSoAHumanCanReadWhatTheNotifierGot(t *testing.T) {
	var buf bytes.Buffer

	if code := encodeJSON(&buf, advise.Alerts(advise.Report{Window: "week"})); code != 0 {
		t.Fatalf("expected success, got %d", code)
	}
	if !strings.Contains(buf.String(), "\n  \"window\": \"week\"") {
		t.Errorf("expected indented JSON:\n%s", buf.String())
	}
}

func TestMetricWording_KeepsTheQualifierTheFindingAttached(t *testing.T) {
	// E-01 names the bucket it's talking about, and that qualifier is what makes
	// two windows comparable — it has to survive into what the reader sees.
	wording := metricWording(advise.MetricDominantBucketShare + ":" + advise.BucketCacheRead)

	if !strings.Contains(wording, advise.BucketCacheRead) {
		t.Errorf("wording = %q, want it to name the bucket %q", wording, advise.BucketCacheRead)
	}
}
