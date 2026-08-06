package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/contextfill"
	"github.com/Rentheria/llm-agent-spend-manager/internal/quota"
)

func sampleReport() quota.Report {
	now := time.Date(2026, 7, 28, 12, 41, 0, 0, time.UTC)
	return quota.Report{
		GeneratedAt: now,
		Cycles: []quota.CycleStatus{{
			Cycle: quota.Cycle{
				Provider: "Anthropic (Claude Max)",
				Plan:     "Max 5x",
				Name:     quota.CycleSessionWindow,
				Unit:     quota.UnitTokens,
				Start:    now.Add(-time.Hour),
				Reset:    now.Add(4 * time.Hour),
				Capacity: quota.Capacity{
					Known: true, Source: quota.SourceCalibrated, Dispersion: 0.24,
					Limit: quota.RangeMeasure(159e6, 339e6),
				},
				Used:  quota.ExactMeasure(8.6e6),
				Turns: 95,
			},
			Fill:      quota.RangeMeasure(0.03, 0.05),
			FillKnown: true,
			Burn:      quota.Burn{PerHour: 43.4e6, Over: 30 * time.Minute, Turns: 230},
			Forecast:  quota.Forecast{Known: true, Exhausts: true, TimeLeft: 3*time.Hour + 28*time.Minute},
		}},
		Breakdown: quota.Breakdown{
			Total: aggregate.Totals{TotalTokens: 1_685_000_000, Turns: 8752, CostUSD: 1083.68},
			ByWorkspace: []aggregate.WorkspaceTotals{{
				Workspace: "/home/user/.openclaw/workspace",
				Agent:     aggregate.AgentOpenClaw,
				Totals:    aggregate.Totals{TotalTokens: 828_000_000, Turns: 3923},
			}},
			ByModel: []aggregate.ModelTotals{{
				Model:  "claude-opus-5",
				Totals: aggregate.Totals{TotalTokens: 823_000_000, Turns: 5007},
			}},
			ByAgent: []aggregate.AgentTotals{{
				Agent:  aggregate.AgentOpenClaw,
				Totals: aggregate.Totals{TotalTokens: 873_000_000, Turns: 4180},
			}},
			TopSessions: []quota.SessionUsage{{
				SessionID: "abc", Label: "Contexto: en este workspace hay un…",
				Workspace: "/home/user/.openclaw/workspace",
				Totals:    aggregate.Totals{TotalTokens: 454_000_000, Turns: 881},
			}},
		},
		Levers: []quota.Lever{{
			ID: quota.LeverConcentration, Title: ".openclaw/workspace se lleva 49% de la cuota",
			Evidence: "828.0M de 1685.5M tokens a 211,068 tokens/turno", Action: "cortar el contexto que arrastra",
			QuotaShare: 0.49, TokensSaved: 20e6, WindowExtension: 90 * time.Minute,
		}},
		Calibration: quota.Calibration{
			Note:         "Techo estimado calibrado con 5 agotamientos observados en esta máquina.",
			ModelWeights: []quota.ModelWeight{{Model: "claude-opus-4-8", Observations: 3, Reason: "no hay con quién compararlo"}},
		},
		Unmetered:      quota.UnmeteredAgents,
		ContextWindows: sampleContextWindows(now),
	}
}

// sampleContextWindows is the T22 incident in the shape the report carries it: a
// session that switched from a 1M window to a 200k one mid-conversation and came
// out the other side above its own ceiling without writing anything.
func sampleContextWindows(now time.Time) quota.ContextWindows {
	before := contextfill.OccupancyOf("claude-sonnet-5", 600_000)
	after := contextfill.OccupancyOf("claude-haiku-4-5", 600_000)
	return quota.ContextWindows{
		WarnAt: 0.80,
		Streams: []quota.ContextStream{{
			Agent:     aggregate.AgentClaudeCode,
			Workspace: "/home/user/Develop/llm-agent-spend-manager",
			Label:     "arregla el cálculo de la ventana",
			Stream: contextfill.Stream{
				SessionID: "sess-switch", Turns: 412, LastActivity: now,
				Live: after, Status: contextfill.StatusCeiling,
			},
		}},
		Shifts: []quota.ContextShift{{
			Agent:     aggregate.AgentClaudeCode,
			SessionID: "sess-switch",
			Workspace: "/home/user/Develop/llm-agent-spend-manager",
			Shift: contextfill.Shift{
				At: now.Add(-30 * time.Minute), CarriedTokens: 600_000, Before: before, After: after,
			},
		}},
		SameCeilingShifts: 52,
		NoWindow: []quota.UnwindowedModel{{
			Model: "claude-sonnet-4-6", Streams: 7, Reason: "las dos fuentes locales se contradicen",
		}},
		Blind: quota.ContextBlindAgents,
	}
}

func TestWriteQuota_AnswersTheQuestionsInOrder(t *testing.T) {
	var buf bytes.Buffer
	writeQuota(&buf, sampleReport(), 3)
	out := buf.String()

	sections := []string{"CUÁNTO QUEDA", "CUÁNTO CONTEXTO QUEDA", "QUIÉN SE LA COME", "QUÉ PALANCA LA ESTIRA MÁS"}
	last := -1
	for _, s := range sections {
		at := strings.Index(out, s)
		if at < 0 {
			t.Fatalf("missing section %q", s)
		}
		if at < last {
			t.Errorf("section %q is out of order", s)
		}
		last = at
	}
}

// The $ is a secondary figure and must never appear without its label — the
// covered agents run on flat-price subscriptions (docs/architecture.md §3.1).
func TestWriteQuota_KeepsTheDollarFigureLabeledAsAnEquivalence(t *testing.T) {
	var buf bytes.Buffer
	writeQuota(&buf, sampleReport(), 3)
	out := buf.String()

	if !strings.Contains(out, aggregate.CostLabel) {
		t.Errorf("the $ appears without %q", aggregate.CostLabel)
	}
	if !strings.Contains(out, aggregate.CostDisclaimer) {
		t.Error("the mandatory disclaimer is missing")
	}
	if strings.Contains(out, "gasto real") && !strings.Contains(out, "NO es gasto real") {
		t.Error("the output claims real spend")
	}
	// The quota headline comes before the money: that is the whole point of T81.
	if strings.Index(out, "CUÁNTO QUEDA") > strings.Index(out, aggregate.CostLabel) {
		t.Error("the $ is presented ahead of the quota")
	}
}

func TestWriteQuota_ReportsANonDerivableModelWeightWithItsReason(t *testing.T) {
	var buf bytes.Buffer
	writeQuota(&buf, sampleReport(), 3)
	out := buf.String()

	if !strings.Contains(out, "no derivable") {
		t.Error("a model weight that could not be derived was silently omitted")
	}
	if !strings.Contains(out, "no hay con quién compararlo") {
		t.Error("the reason a weight is missing was not printed")
	}
}

// El renglón que el ticket pide poder leer: la misma sesión, el mismo contexto,
// dos ventanas, dos porcentajes — y el segundo arriba del 100%.
func TestWriteQuota_SaysTheSessionJumpedWithoutWritingATokenMore(t *testing.T) {
	var buf bytes.Buffer
	writeQuota(&buf, sampleReport(), 3)
	out := buf.String()

	for _, want := range []string{
		"sin escribir un solo token más",
		"claude-sonnet-5 → claude-haiku-4-5",
		"60% → 300% de la ventana",
		"600,000 tokens de contexto",
		// Los dos techos, exactos: redondeados a millones (1.0M → 0.2M) el lector
		// no puede comprobar de dónde salieron los dos porcentajes.
		"1,000,000 → 200,000 tokens de techo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q en la salida", want)
		}
	}
}

func TestWriteQuota_WarnsBeforeTheCeilingAndSaysAtWhichLine(t *testing.T) {
	var buf bytes.Buffer
	writeQuota(&buf, sampleReport(), 3)
	out := buf.String()

	if !strings.Contains(out, "aviso a partir de 80%") {
		t.Error("el umbral de aviso no aparece: un aviso sin su raya no se puede juzgar")
	}
	if !strings.Contains(out, "PASÓ EL TECHO") {
		t.Error("una sesión arriba del 100% no está marcada")
	}
}

func TestWriteQuota_ReportsAnUnderivableContextWindowWithItsReason(t *testing.T) {
	var buf bytes.Buffer
	writeQuota(&buf, sampleReport(), 3)
	out := buf.String()

	if !strings.Contains(out, "las dos fuentes locales se contradicen") {
		t.Error("el motivo de una ventana no derivable no se imprimió")
	}
	if !strings.Contains(out, "7 hilos") {
		t.Error("no se dice cuántos hilos quedaron sin porcentaje: se leerían como hilos con espacio")
	}
	if !strings.Contains(out, quota.ContextBlindAgents[aggregate.AgentCursor]) {
		t.Error("los agentes sin contexto medible no están listados")
	}
}

func TestWriteQuota_CountsSameCeilingSwitchesWithoutListingThem(t *testing.T) {
	var buf bytes.Buffer
	writeQuota(&buf, sampleReport(), 3)
	out := buf.String()

	if !strings.Contains(out, "52 cambios de modelo entre ventanas del mismo tamaño") {
		t.Error("los cambios que no movieron el techo no se resumen")
	}
}

func TestWindowPct_DoesNotCapAtTheCeiling(t *testing.T) {
	// Recortar a 100% borraría justo el número que hizo visible el problema.
	if got, want := windowPct(2.98), "298%"; got != want {
		t.Errorf("windowPct(2.98) = %q, want %q", got, want)
	}
	if got, want := windowPct(0.6), "60%"; got != want {
		t.Errorf("windowPct(0.6) = %q, want %q", got, want)
	}
}

func TestNoFillReason_DistinguishesTheThreeWaysToHaveNoPercentage(t *testing.T) {
	blind := quota.Cycle{Name: quota.CycleBillingMonth, Turns: 8, Unmeasured: 8}
	if got := noFillReason(blind, true); !strings.Contains(got, "legible") {
		t.Errorf("blind cycle reason = %q, want it to name the unreadable turns", got)
	}
	weekly := quota.Cycle{Name: quota.CycleWeeklyCap, Turns: 100}
	if got := noFillReason(weekly, false); !strings.Contains(got, "no publica") {
		t.Errorf("weekly reason = %q, want the unpublished cap", got)
	}
	window := quota.Cycle{Name: quota.CycleSessionWindow, Turns: 100}
	if got := noFillReason(window, false); !strings.Contains(got, "calibrar") {
		t.Errorf("window reason = %q, want the missing calibration", got)
	}
	if got := noFillReason(window, true); got != "" {
		t.Errorf("suppressed a percentage that was computable: %q", got)
	}
}

func TestUnmeasuredNote_OnlyAppearsWhenSomethingIsUnreadable(t *testing.T) {
	if got := unmeasuredNote(quota.Cycle{Turns: 10}); got != "" {
		t.Errorf("note = %q, want none", got)
	}
	if got := unmeasuredNote(quota.Cycle{Turns: 10, Unmeasured: 4}); !strings.Contains(got, "4") {
		t.Errorf("note = %q, want the count of unreadable turns", got)
	}
}

func TestDuration_ReadsTheWayAWaitIsSpoken(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Hour:                   "0 min",
		45 * time.Minute:             "45 min",
		3*time.Hour + 28*time.Minute: "3 h 28 min",
		30 * time.Hour:               "1 d 6 h",
	}
	for in, want := range cases {
		if got := duration(in); got != want {
			t.Errorf("duration(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestPercent_KeepsARangeARange(t *testing.T) {
	if got := percent(quota.ExactMeasure(0.68)); got != "68%" {
		t.Errorf("exact percent = %q", got)
	}
	if got := percent(quota.RangeMeasure(0.51, 0.85)); !strings.Contains(got, "rango") {
		t.Errorf("range percent = %q, want it bracketed", got)
	}
}

func TestBreakdownSince_ZeroDaysMeansAllHistory(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if got := breakdownSince(now, 0); !got.IsZero() {
		t.Errorf("since = %s, want the zero time", got)
	}
	if got := breakdownSince(now, 3); !got.Equal(now.AddDate(0, 0, -3)) {
		t.Errorf("since = %s, want 3 days back", got)
	}
}

func TestCmdQuota_RejectsANegativeWindow(t *testing.T) {
	var buf bytes.Buffer
	if code := cmdQuota([]string{"--days", "-1"}, &buf); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

// fleetSampleReport adds a second cycle (Cursor) and a second agent's totals
// to sampleReport, so the fleet view has something to show for all four
// agents instead of just the one Anthropic cycle the rest of the suite covers.
func fleetSampleReport() quota.Report {
	report := sampleReport()
	report.Cycles = append(report.Cycles, quota.CycleStatus{
		Cycle: quota.Cycle{
			Provider: "Cursor",
			Plan:     "plan mensual",
			Name:     quota.CycleBillingMonth,
			Unit:     quota.UnitUSD,
			Start:    report.GeneratedAt.Add(-10 * 24 * time.Hour),
			Reset:    report.GeneratedAt.Add(20 * 24 * time.Hour),
			Capacity: quota.Capacity{Known: true, Source: quota.SourcePlan, Limit: quota.ExactMeasure(20)},
			Used:     quota.RangeMeasure(1, 4),
			Turns:    40,
		},
		Fill:      quota.RangeMeasure(0.05, 0.20),
		FillKnown: true,
		Burn:      quota.Burn{PerHour: 0.1, Over: 30 * time.Minute, Turns: 3},
		Forecast:  quota.Forecast{Known: true, Exhausts: false, TimeLeft: 20 * 24 * time.Hour},
	})
	report.Breakdown.ByAgent = append(report.Breakdown.ByAgent,
		aggregate.AgentTotals{Agent: aggregate.AgentClaudeCode, Totals: aggregate.Totals{TotalTokens: 500_000, Turns: 12, CostUSD: 4.2}},
		aggregate.AgentTotals{Agent: aggregate.AgentCursor, Totals: aggregate.Totals{TotalTokens: 300_000, Turns: 40, CostUSD: 9.44}},
	)
	return report
}

func TestWriteFleet_ListsAllFourAgents(t *testing.T) {
	var buf bytes.Buffer
	writeFleet(&buf, fleetSampleReport())
	out := buf.String()

	for _, agent := range []string{aggregate.AgentClaudeCode, aggregate.AgentOpenClaw, aggregate.AgentCursor, aggregate.AgentAntigravity} {
		if !strings.Contains(out, agent) {
			t.Errorf("fleet view is missing agent %q", agent)
		}
	}
}

// Claude Code and OpenClaw share one Anthropic account cycle, so their fill/
// forecast column must read the same — a difference here would mean the view
// invented a per-agent split the account doesn't have.
func TestWriteFleet_ClaudeCodeAndOpenClawShareTheAnthropicCycle(t *testing.T) {
	var buf bytes.Buffer
	writeFleet(&buf, fleetSampleReport())
	lines := strings.Split(buf.String(), "\n")

	var ccLine, ocLine string
	for _, l := range lines {
		if strings.Contains(l, aggregate.AgentClaudeCode) {
			ccLine = l
		}
		if strings.Contains(l, aggregate.AgentOpenClaw) {
			ocLine = l
		}
	}
	if ccLine == "" || ocLine == "" {
		t.Fatal("could not find both rows")
	}
	if !strings.Contains(ccLine, "4%") || !strings.Contains(ocLine, "4%") {
		t.Errorf("Claude Code and OpenClaw rows should show the same fill:\n  %s\n  %s", ccLine, ocLine)
	}
}

func TestWriteFleet_NamesTheUnmeteredReasonInsteadOfAZero(t *testing.T) {
	var buf bytes.Buffer
	writeFleet(&buf, fleetSampleReport())
	out := buf.String()

	if !strings.Contains(out, quota.UnmeteredAgents[aggregate.AgentAntigravity]) {
		t.Error("Antigravity's unmetered reason is missing from the fleet view")
	}
}

func TestWriteFleet_LabelsCostAndKeepsTheDisclaimer(t *testing.T) {
	var buf bytes.Buffer
	writeFleet(&buf, fleetSampleReport())
	out := buf.String()

	if !strings.Contains(out, aggregate.CostLabel) {
		t.Errorf("the $ column is missing its mandatory label %q", aggregate.CostLabel)
	}
	if !strings.Contains(out, aggregate.CostDisclaimer) {
		t.Error("the mandatory disclaimer is missing")
	}
}

// The Cursor cycle is activity-tier (a range), unlike the Anthropic one — different
// buckets, different measurement honesty, and the view must say so per agent.
func TestWriteFleet_FlagsARangeAsARangeNotAnExactFigure(t *testing.T) {
	var buf bytes.Buffer
	writeFleet(&buf, fleetSampleReport())
	lines := strings.Split(buf.String(), "\n")

	for _, l := range lines {
		if strings.Contains(l, aggregate.AgentCursor) && !strings.Contains(l, "rango") {
			t.Errorf("Cursor's activity-tier fill was not flagged as a range: %q", l)
		}
	}
}

func TestFleetBucket_SeparatesTheFourBolsas(t *testing.T) {
	cases := map[string]string{
		aggregate.AgentClaudeCode:  "Anthropic",
		aggregate.AgentOpenClaw:    "Anthropic",
		aggregate.AgentCursor:      "Cursor",
		aggregate.AgentAntigravity: "suscripción gratis",
	}
	for agent, want := range cases {
		if got := fleetBucket(agent); got != want {
			t.Errorf("fleetBucket(%q) = %q, want %q", agent, got, want)
		}
	}
}

func TestRun_ExposesQuotaAsAFirstClassCommand(t *testing.T) {
	var buf bytes.Buffer
	run(nil, &buf)
	if !strings.Contains(buf.String(), "quota") {
		t.Error("the usage text does not list the quota command")
	}
}
