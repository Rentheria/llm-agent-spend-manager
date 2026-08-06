package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Rentheria/llm-agent-spend-manager/internal/workload"
)

func aWorkloadReport() workload.Report {
	return workload.Report{
		Streams:    3,
		Classified: 2,
		CostUSD:    100,
		Caveat:     workload.EquivalenceCaveat,
		Classes: []workload.ClassPlan{{
			Class:     workload.ClassLongConversation,
			Lever:     workload.LeverFor(workload.ClassLongConversation),
			Streams:   2,
			Turns:     1200,
			CostUSD:   90,
			CostShare: 0.9,
			Routes: []workload.RouteCost{
				{Route: "Claude Code", Measured: true, Streams: 1, Turns: 1000, CostUSD: 80,
					CostPerTurnUSD: 0.08, ByModel: []workload.ModelCost{{Model: "claude-opus-5", Turns: 1000, CostUSD: 80}}},
				{Route: "OpenClaw", Measured: true, Streams: 1, Turns: 200, CostUSD: 10,
					CostPerTurnUSD: 0.05, ByModel: []workload.ModelCost{{Model: "claude-sonnet-5", Turns: 200, CostUSD: 10}}},
			},
			Missing: []workload.MissingRoute{{Route: "Cursor", Reason: workload.MissingActivityOnly}},
			ByRoute: workload.Counterfactual{
				Dimension: workload.DimensionRoute, Cheapest: "OpenClaw",
				CheapestCostPerTurnUSD: 0.05, CheapestTurns: 200, TurnsElsewhere: 1000,
				MovableTurns: 200, SavingsUSD: 6, Known: true,
			},
			ByModel: workload.Counterfactual{
				Dimension: workload.DimensionModel,
				Known:     false, Reason: "solo se midió una opción de modelo…; no se interpola.",
			},
		}},
		Unclassified: workload.Unclassified{
			Streams: 1, Turns: 4, CostUSD: 10, CostShare: 0.1,
			Reasons: []workload.ReasonCount{{Reason: workload.ReasonActivityTier, Streams: 1, CostUSD: 10}},
		},
	}
}

func TestWriteWorkloads_ShowsEachShapeWithTheLeverThatAppliesToIt(t *testing.T) {
	var buf bytes.Buffer

	writeWorkloads(&buf, aWorkloadReport())

	out := buf.String()
	for _, want := range []string{"FORMA DE LA CARGA", "Conversación larga", "tope de contexto / corte por tarea",
		"PLAN DE AHORRO POR RUTA", "Palanca:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// The unclassified load is printed with the same weight as the rest. Hiding it
// would overstate how much of the fleet this actually understands.
func TestWriteWorkloads_ShowsTheUnclassifiedLoadAndWhyItStayedThere(t *testing.T) {
	var buf bytes.Buffer

	writeWorkloads(&buf, aWorkloadReport())

	out := buf.String()
	if !strings.Contains(out, "Sin clasificar") {
		t.Errorf("output hides the unclassified load:\n%s", out)
	}
	if !strings.Contains(out, workload.ReasonActivityTier) {
		t.Errorf("output missing the reason the load stayed unclassified:\n%s", out)
	}
}

// A route with no comparable figure has to be named as missing data. Silence
// reads as "it wasn't relevant", which is a different claim from "we don't know".
func TestWriteWorkloads_NamesTheRoutesWithNoDataInsteadOfDroppingThem(t *testing.T) {
	var buf bytes.Buffer

	writeWorkloads(&buf, aWorkloadReport())

	out := buf.String()
	if !strings.Contains(out, "Falta el dato · Cursor") {
		t.Errorf("output drops the route with no figure:\n%s", out)
	}
	if !strings.Contains(out, "no se interpola") {
		t.Errorf("output missing the un-computable counterfactual's reason:\n%s", out)
	}
}

// A claim limited by how little the cheap option was observed doing has to say
// so next to the figure; otherwise the reader reads a measurement where there's
// only a fraction of one.
func TestWriteWorkloads_SaysWhenTheClaimWasCappedByWhatWasObserved(t *testing.T) {
	var buf bytes.Buffer

	writeWorkloads(&buf, aWorkloadReport())

	out := buf.String()
	if !strings.Contains(out, "Topado por la observación") {
		t.Errorf("output missing the observation cap:\n%s", out)
	}
	if !strings.Contains(out, "extrapolar") {
		t.Errorf("output missing why the rest isn't claimed:\n%s", out)
	}
}

// An activity-tier figure must never sit next to a measured one looking like the
// same kind of evidence.
func TestWriteWorkloads_MarksAnEstimatedRouteWithTheApproximationSign(t *testing.T) {
	report := aWorkloadReport()
	report.Classes[0].Routes[1].Measured = false
	var buf bytes.Buffer

	writeWorkloads(&buf, report)

	if !strings.Contains(buf.String(), "≈ OpenClaw (estimado)") {
		t.Errorf("estimated route rendered as if it were measured:\n%s", buf.String())
	}
}

func TestWriteWorkloads_StaysSilentWithNothingMeasured(t *testing.T) {
	var buf bytes.Buffer

	writeWorkloads(&buf, workload.Report{})

	if buf.Len() != 0 {
		t.Errorf("printed a section with no workloads: %q", buf.String())
	}
}

func TestPlural_KeepsCountedNounsReadable(t *testing.T) {
	if got := plural(1, "turno", "turnos"); got != "1 turno" {
		t.Errorf("plural(1) = %q, want %q", got, "1 turno")
	}
	if got := plural(1200, "turno", "turnos"); got != "1,200 turnos" {
		t.Errorf("plural(1200) = %q, want %q", got, "1,200 turnos")
	}
}
