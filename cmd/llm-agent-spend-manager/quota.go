package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/contextfill"
	"github.com/Rentheria/llm-agent-spend-manager/internal/humanize"
	"github.com/Rentheria/llm-agent-spend-manager/internal/quota"
)

// defaultBreakdownDays is how far back "who is eating the quota" looks by
// default. Three days is long enough that a single heavy afternoon doesn't
// dominate the table and short enough that the answer still describes how the
// fleet is being used right now.
const defaultBreakdownDays = 3

// cmdQuota prints the quota report: how much of each cycle is gone and how long
// it lasts at the current rate, how much context each live conversation has left
// before its own window fills (T22), who is consuming it, and which lever
// stretches it the most. This is the command that replaces digging through
// transcripts by hand. The $ appears only as a secondary figure under its
// mandatory label — the fleet runs on flat-price subscriptions, so what runs out
// is quota, not money.
//
// The context window landed here rather than in a command of its own because this
// is already the command about windows running out: the account's cycle and a
// session's context are two ceilings that both stop the work mid-task, and whoever
// is about to send a turn needs both answers at once.
func cmdQuota(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("quota", flag.ContinueOnError)
	fs.SetOutput(out)
	days := fs.Int("days", defaultBreakdownDays, "days the breakdown covers (0 = all history)")
	asJSON := fs.Bool("json", false, "emit the report as JSON instead of a text table")
	fleet := fs.Bool("fleet", false, "compact per-agent view: fill/forecast, cost estimate, bucket (T20)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *days < 0 {
		fmt.Fprintln(out, "--days no puede ser negativo")
		return 2
	}

	cfg, err := quota.LoadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(out, "configuración de planes inválida:", err)
		return 1
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(out, "cannot resolve home directory:", err)
		return 1
	}
	snapshot, err := aggregate.CollectSnapshot(homeDir)
	if err != nil {
		fmt.Fprintln(out, "failed to read usage data:", err)
		return 1
	}

	now := time.Now()
	report := quota.Analyze(snapshot, cfg, breakdownSince(now, *days), now)

	if *fleet {
		writeFleet(out, report)
		return 0
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(out, "failed to encode report:", err)
			return 1
		}
		return 0
	}
	writeQuota(out, report, *days)
	return 0
}

// breakdownSince turns a day count into the lower bound of the breakdown
// period. Zero days means all history, expressed as the zero time.
func breakdownSince(now time.Time, days int) time.Time {
	if days == 0 {
		return time.Time{}
	}
	return now.AddDate(0, 0, -days)
}

// writeQuota renders the report in the order the questions are asked.
func writeQuota(out io.Writer, report quota.Report, days int) {
	fmt.Fprintf(out, "llm-agent-spend-manager — cuota · desglose de %s\n\n", periodLabel(days))
	writeCycles(out, report)
	writeContextWindows(out, report.ContextWindows)
	writeBreakdown(out, report.Breakdown)
	writeLevers(out, report.Levers)
	fmt.Fprintf(out, "\n%s\n", aggregate.CostDisclaimer)
}

func periodLabel(days int) string {
	if days == 0 {
		return "todo el histórico"
	}
	if days == 1 {
		return "las últimas 24 h"
	}
	return fmt.Sprintf("los últimos %d días", days)
}

// writeCycles answers "how much of the window is gone and how long is left".
func writeCycles(out io.Writer, report quota.Report) {
	fmt.Fprintln(out, "CUÁNTO QUEDA")
	if len(report.Cycles) == 0 {
		fmt.Fprintln(out, "  Ningún ciclo de cuota en curso: sin actividad reciente en los planes medidos.")
	}
	for _, status := range report.Cycles {
		writeCycle(out, status)
	}
	if report.Calibration.Note != "" {
		fmt.Fprintf(out, "\n  Calibración: %s\n", report.Calibration.Note)
	}
	writeModelWeights(out, report.Calibration.ModelWeights)
	writeUnmetered(out, report.Unmetered)
	fmt.Fprintln(out)
}

func writeCycle(out io.Writer, status quota.CycleStatus) {
	c := status.Cycle
	fmt.Fprintf(out, "\n  %s · %s (%s)\n", c.Provider, c.Name, c.Plan)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "    Periodo\t%s → %s\n", clock(c.Start), clock(c.Reset))
	fmt.Fprintf(tw, "    Consumido\t%s en %s turnos%s\n",
		amount(c.Used, c.Unit), thousands(c.Turns), unmeasuredNote(c))
	if reason := noFillReason(c, status.FillKnown); reason != "" {
		fmt.Fprintf(tw, "    Ventana\tsin %% calculable: %s\n", reason)
	} else {
		fmt.Fprintf(tw, "    Ventana\t%s del techo (%s)\n", percent(status.Fill), capacityNote(c.Capacity))
	}
	fmt.Fprintf(tw, "    Ritmo\t%s\n", burnLine(status.Burn, c.Unit))
	fmt.Fprintf(tw, "    Pronóstico\t%s\n", forecastLine(status.Forecast))
	tw.Flush()
}

// capacityNote says where a ceiling came from, and how scattered it is when it
// was calibrated rather than published.
func capacityNote(cap quota.Capacity) string {
	if cap.Dispersion > 0 {
		return fmt.Sprintf("%s, dispersión ±%.0f%%", cap.Source, cap.Dispersion*100)
	}
	return cap.Source
}

// unmeasuredNote flags the turns Used could not see. Without it a plan whose
// turns are all unpriced reads as a plan nobody is using.
func unmeasuredNote(c quota.Cycle) string {
	if c.Unmeasured == 0 {
		return ""
	}
	return fmt.Sprintf(" (+%s turnos sin consumo legible: el real es mayor)", thousands(c.Unmeasured))
}

// noFillReason explains an absent percentage instead of printing a hopeful zero,
// and returns "" when the percentage is worth printing. There are three honest
// ways to have no percentage and they are not interchangeable.
func noFillReason(c quota.Cycle, fillKnown bool) string {
	if c.Turns > 0 && c.Unmeasured == c.Turns {
		return fmt.Sprintf("ninguno de los %s turnos del ciclo trae consumo legible; "+
			"un 0%% mediría el medidor, no el plan", thousands(c.Turns))
	}
	if fillKnown {
		return ""
	}
	if c.Name == quota.CycleWeeklyCap {
		return "Anthropic no publica el tope semanal y aquí nunca se ha tocado; " +
			"el consumo y el ritmo sí son reales"
	}
	return "todavía no hay agotamientos suficientes para calibrar el techo"
}

func burnLine(burn quota.Burn, unit string) string {
	if burn.PerHour <= 0 {
		return fmt.Sprintf("sin actividad en los últimos %s", duration(burn.Over))
	}
	return fmt.Sprintf("%s/h, medido sobre %s turnos en los últimos %s",
		unitAmount(burn.PerHour, unit), thousands(burn.Turns), duration(burn.Over))
}

func forecastLine(f quota.Forecast) string {
	if !f.Known {
		return fmt.Sprintf("no calculable sin techo o sin ritmo; el ciclo refila en %s", duration(f.TimeLeft))
	}
	if !f.Exhausts {
		return fmt.Sprintf("al ritmo actual el ciclo aguanta hasta el refill, en %s", duration(f.TimeLeft))
	}
	return fmt.Sprintf("⚠ se agota en %s, antes del refill", duration(f.TimeLeft))
}

// writeModelWeights prints what could and could not be derived about per-model
// weighting. The non-derivable case is printed too, and with its reason: an
// omitted line would read as "no difference between models", which is a claim
// this data does not support either way.
func writeModelWeights(out io.Writer, weights []quota.ModelWeight) {
	if len(weights) == 0 {
		return
	}
	fmt.Fprintln(out, "\n  Peso por modelo contra la cuota (Anthropic no lo publica; solo se deriva de lo observado)")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, w := range weights {
		if w.Derivable {
			fmt.Fprintf(tw, "    %s\t×%.2f\t(%d ventanas dominadas)\n", w.Model, w.Weight, w.Observations)
			continue
		}
		fmt.Fprintf(tw, "    %s\tno derivable\t%s\n", w.Model, w.Reason)
	}
	tw.Flush()
}

func writeUnmetered(out io.Writer, unmetered map[string]string) {
	if len(unmetered) == 0 {
		return
	}
	fmt.Fprintln(out, "\n  Sin cuota medible")
	for agent, reason := range unmetered {
		fmt.Fprintf(out, "    · %s: %s\n", agent, reason)
	}
}

// writeContextWindows answers the second "how much is left": not of the account's
// quota cycle but of each live conversation's own context window, which runs out
// separately and stops that conversation mid-task (T22).
func writeContextWindows(out io.Writer, cw quota.ContextWindows) {
	fmt.Fprintln(out, "CUÁNTO CONTEXTO QUEDA (por sesión, contra la ventana del modelo activo)")
	if len(cw.Streams) == 0 && len(cw.NoWindow) == 0 {
		fmt.Fprintln(out, "  Sin sesiones medidas en el periodo: nada que ocupe una ventana de contexto.")
	}
	writeFullestStreams(out, cw)
	writeContextShifts(out, cw)
	writeUnwindowedModels(out, cw.NoWindow)
	writeContextBlind(out, cw.Blind)
	fmt.Fprintln(out)
}

func writeFullestStreams(out io.Writer, cw quota.ContextWindows) {
	if len(cw.Streams) == 0 {
		return
	}
	fmt.Fprintf(out, "\n  Sesiones más llenas · aviso a partir de %s de la ventana\n", windowPct(cw.WarnAt))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    \tOCUPADO\tCONTEXTO / VENTANA\tMODELO ACTIVO\tTURNOS\t")
	for _, s := range cw.Streams {
		live := s.Stream.Live
		// Las ventanas van en cifra exacta, no en millones: 1,048,576 y 1,000,000
		// se redondean los dos a "1.0M", y entonces dos ocupaciones distintas del
		// mismo contexto se leerían como un error de cálculo en vez de como el
		// techo que de verdad se movió.
		fmt.Fprintf(tw, "    %s\t%s\t%s / %s\t%s\t%s\t%s\n",
			contextRowName(s.Stream.SessionID, s.Stream.ThreadID, s.Workspace, s.Label), windowPct(live.Share),
			thousands(live.Tokens), thousands(live.Limit),
			live.Model, thousands(s.Stream.Turns), contextStatusNote(s.Stream.Status))
	}
	tw.Flush()
}

// contextStatusNote is what turns a percentage into a warning. The ceiling case
// is not a prediction: past 100% the runtime is already compacting or refusing.
func contextStatusNote(status string) string {
	switch status {
	case contextfill.StatusCeiling:
		return "⚠ PASÓ EL TECHO: el runtime ya está compactando o negándose"
	case contextfill.StatusWarning:
		return "⚠ cerca del techo"
	default:
		return ""
	}
}

// writeContextShifts prints the model changes that re-scored a conversation
// without a token being written. This is the section T22 exists for: it is the
// only place the report can say "esta sesión pasó de X% a Y% sin escribir nada".
func writeContextShifts(out io.Writer, cw quota.ContextWindows) {
	if len(cw.Shifts) == 0 && cw.SameCeilingShifts == 0 {
		return
	}
	fmt.Fprintln(out, "\n  Cambios de modelo a media sesión (mismo contexto, otro techo)")
	for _, s := range cw.Shifts {
		shift := s.Shift
		fmt.Fprintf(out, "    · %s · %s: %s → %s\n",
			contextRowName(s.SessionID, s.ThreadID, s.Workspace, s.Label),
			clock(shift.At), shift.Before.Model, shift.After.Model)
		fmt.Fprintf(out, "      %s tokens de contexto, sin escribir un solo token más: %s\n",
			thousands(shift.CarriedTokens), rescoreLine(shift))
	}
	if cw.SameCeilingShifts > 0 {
		fmt.Fprintf(out, "    (+%s cambios de modelo entre ventanas del mismo tamaño: el techo no se movió, "+
			"la ocupación no cambió)\n", thousands(cw.SameCeilingShifts))
	}
}

// rescoreLine states the re-scoring, or says which side of it is missing. A shift
// with an underivable window on one side is still real — the ceiling did move —
// so it is reported with the reason where the percentage would be.
func rescoreLine(shift contextfill.Shift) string {
	if shift.Rescored() {
		return fmt.Sprintf("%s → %s de la ventana (%s → %s tokens de techo)",
			windowPct(shift.Before.Share), windowPct(shift.After.Share),
			thousands(shift.Before.Limit), thousands(shift.After.Limit))
	}
	if !shift.Before.Known {
		return fmt.Sprintf("el salto no es medible — ventana de %s no derivable: %s",
			shift.Before.Model, shift.Before.Reason)
	}
	return fmt.Sprintf("%s de la ventana de %s → el salto no es medible: %s",
		windowPct(shift.Before.Share), shift.Before.Model, shift.After.Reason)
}

// writeUnwindowedModels names the models live streams are running on with no
// derivable ceiling. Omitting them would let those streams read as having room.
func writeUnwindowedModels(out io.Writer, models []quota.UnwindowedModel) {
	if len(models) == 0 {
		return
	}
	fmt.Fprintln(out, "\n  Hilos activos sin % calculable (ventana del modelo no derivable)")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, m := range models {
		fmt.Fprintf(tw, "    %s\t%s hilos\tno derivable: %s\n", m.Model, thousands(m.Streams), m.Reason)
	}
	tw.Flush()
}

func writeContextBlind(out io.Writer, blind map[string]string) {
	if len(blind) == 0 {
		return
	}
	fmt.Fprintln(out, "\n  Sin contexto medible")
	for _, agent := range sortedKeys(blind) {
		fmt.Fprintf(out, "    · %s: %s\n", agent, blind[agent])
	}
}

// sortedKeys keeps map-driven output stable run to run: a table whose row order
// changes on every run is a table nobody can diff. It is generic over the value
// type so the same helper orders the proxy's flag-option maps too.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// contextRowName identifies a context stream in a row: the workspace when there
// is one, the session id otherwise, the thread when the session ran more than its
// main one (a spawned subagent carries its own independent context), and what was
// asked, which is what makes the row recognizable instead of merely unique.
func contextRowName(sessionID, threadID, workspace, label string) string {
	name := recorta(sessionID, sessionIDWidth)
	if workspace != "" {
		name = quota.WorkspaceName(workspace)
	}
	if threadID != "" {
		name += " · hilo " + recorta(threadID, threadIDWidth)
	}
	if label != "" {
		name += " — " + recorta(label, contextLabelWidth)
	}
	return name
}

const (
	// threadIDWidth is enough of a subagent's id to tell two of them apart.
	threadIDWidth = 8
	// contextLabelWidth is shorter than the breakdown's: these rows already carry
	// a workspace and a thread before the prompt starts.
	contextLabelWidth = 40
)

// windowPct renders a share of a context window. It deliberately does not cap at
// 100%: a session past its ceiling reads "298%", which is exactly the number the
// incident behind T22 produced and the one a reader needs to see.
func windowPct(share float64) string {
	return fmt.Sprintf("%.0f%%", share*100)
}

// writeBreakdown answers "who is eating it", from the dimension that separates
// the chat from the work (workspace) down to the individual conversation.
func writeBreakdown(out io.Writer, b quota.Breakdown) {
	fmt.Fprintln(out, "QUIÉN SE LA COME")
	if b.Total.Turns == 0 {
		fmt.Fprintln(out, "  Sin actividad en el periodo.")
		fmt.Fprintln(out)
		return
	}
	fmt.Fprintf(out, "  Total del periodo: %s tokens en %s turnos · %s %s\n\n",
		millions(float64(b.Total.TotalTokens)), thousands(b.Total.Turns),
		usd(b.Total.CostUSD), aggregate.CostLabel)

	total := b.Total.TotalTokens
	writeSection(out, "Por espacio de trabajo (separa la plática del trabajo de código)", total,
		workspaceRows(b.ByWorkspace))
	writeSection(out, "Por modelo", total, modelRows(b.ByModel))
	writeSection(out, "Por agente", total, agentRows(b.ByAgent))
	writeSection(out, "Sesiones más pesadas", total, sessionRows(b.TopSessions))
}

// row is one line of a breakdown table: what it is, and what it consumed.
type row struct {
	name   string
	note   string
	totals aggregate.Totals
}

func writeSection(out io.Writer, title string, total int, rows []row) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(out, "  %s\n", title)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    \tTOKENS\t%\tTURNOS\tTOKENS/TURNO\t")
	for _, r := range rows {
		fmt.Fprintf(tw, "    %s\t%s\t%.1f%%\t%s\t%s\t%s\n",
			r.name, millions(float64(r.totals.TotalTokens)), sharePct(r.totals.TotalTokens, total),
			thousands(r.totals.Turns), thousands(perTurn(r.totals)), r.note)
	}
	tw.Flush()
	fmt.Fprintln(out)
}

func workspaceRows(workspaces []aggregate.WorkspaceTotals) []row {
	out := make([]row, 0, len(workspaces))
	for _, w := range workspaces {
		out = append(out, row{name: quota.WorkspaceName(w.Workspace), note: w.Agent, totals: w.Totals})
	}
	return out
}

func modelRows(models []aggregate.ModelTotals) []row {
	out := make([]row, 0, len(models))
	for _, m := range models {
		name := m.Model
		if name == "" {
			name = "(sin modelo reportado)"
		}
		out = append(out, row{name: name, totals: m.Totals})
	}
	return out
}

func agentRows(agents []aggregate.AgentTotals) []row {
	out := make([]row, 0, len(agents))
	for _, a := range agents {
		out = append(out, row{name: a.Agent, totals: a.Totals})
	}
	return out
}

func sessionRows(sessions []quota.SessionUsage) []row {
	out := make([]row, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, row{name: sessionName(s), note: recorta(s.Label, sessionLabelWidth), totals: s.Totals})
	}
	return out
}

// sessionLabelWidth keeps the opening prompt readable as a reminder of what the
// session was without wrapping the table. sessionIDWidth is the fallback when a
// session has no workspace: enough of a UUID to tell two rows apart.
const (
	sessionLabelWidth = 60
	sessionIDWidth    = 12
)

func sessionName(s quota.SessionUsage) string {
	if s.Workspace != "" {
		return quota.WorkspaceName(s.Workspace)
	}
	return recorta(s.SessionID, sessionIDWidth)
}

// writeLevers answers "what stretches it the most". This is the section the
// whole command exists for: a report nobody can act on is the thing being
// replaced, not the thing being built.
func writeLevers(out io.Writer, levers []quota.Lever) {
	fmt.Fprintln(out, "QUÉ PALANCA LA ESTIRA MÁS")
	if len(levers) == 0 {
		fmt.Fprintln(out, "  Ninguna concentración destaca sobre las demás: el consumo está repartido.")
		return
	}
	for _, l := range levers {
		fmt.Fprintf(out, "\n  [%s] %s\n", l.ID, l.Title)
		fmt.Fprintf(out, "      Evidencia: %s\n", l.Evidence)
		if l.TokensSaved > 0 {
			fmt.Fprintf(out, "      Efecto:    %s tokens de cuota liberados%s\n",
				millions(l.TokensSaved), extensionNote(l.WindowExtension))
		}
		fmt.Fprintf(out, "      Acción:    %s\n", l.Action)
	}
}

func extensionNote(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf(" ≈ %s más de ventana al ritmo actual", duration(d))
}

// amount renders a consumption figure in its cycle's unit, keeping a range a
// range: an activity-tier estimate collapsed to a point would claim a precision
// the underlying data never had.
func amount(m quota.Measure, unit string) string {
	if m.Exact {
		return unitAmount(m.Point, unit)
	}
	return "≈" + unitAmount(m.Low, unit) + "–" + unitAmount(m.High, unit)
}

func unitAmount(v float64, unit string) string {
	if unit == quota.UnitUSD {
		return usd(v)
	}
	return millions(v) + " tokens"
}

func percent(m quota.Measure) string {
	if m.Exact {
		return fmt.Sprintf("%.0f%%", m.Point*100)
	}
	return fmt.Sprintf("%.0f%% (rango %.0f–%.0f%%)", m.Point*100, m.Low*100, m.High*100)
}

func millions(v float64) string { return humanize.Millions(v) }

func sharePct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func perTurn(t aggregate.Totals) int {
	if t.Turns == 0 {
		return 0
	}
	return t.TotalTokens / t.Turns
}

// fleetAgents is the display order for --fleet (T20 §3.2): the two agents that
// share the Anthropic account cycle first, so the shared number is obviously
// shared, then Cursor, then the unmetered agent. Itzamná is left out — it has
// no remote-orchestration path to route to (T20 design doc §5, decision 7).
var fleetAgents = []string{
	aggregate.AgentClaudeCode,
	aggregate.AgentOpenClaw,
	aggregate.AgentCursor,
	aggregate.AgentAntigravity,
}

// writeFleet renders the compact per-agent view T20 asked for: fill/forecast
// first, because that's the real routing signal (§2.2), cost second and
// always under its mandatory label, whether the fill is a measured figure or
// a range, and which bucket the agent bills against — comparing bolsas
// (Anthropic vs Cursor vs a free subscription vs local hardware) is exactly
// the mistake this column exists to prevent. It renders new tables over
// numbers Analyze already computed; no new data is collected for this view.
func writeFleet(out io.Writer, report quota.Report) {
	fmt.Fprintln(out, "FLOTA — quién puede tomar la siguiente tarea")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	// The header carries CostLabel verbatim (not upper-cased like its neighbors):
	// this is the mandatory wording for the $ column (aggregate.go's own doc
	// comment), so it must appear exactly, not as a paraphrase.
	fmt.Fprintf(tw, "  AGENTE\tBOLSA\tLLENADO / PRONÓSTICO\t%s\tMEDIDA\n", aggregate.CostLabel)
	for _, agent := range fleetAgents {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			agent, fleetBucket(agent), fleetFillLine(report, agent),
			fleetCostLine(report.Breakdown.ByAgent, agent), fleetMeasureNote(report, agent))
	}
	tw.Flush()
	fmt.Fprintf(out, "\n%s\n", aggregate.CostDisclaimer)
}

// fleetBucket names the billing bucket an agent draws from. Claude Code and
// OpenClaw share one Anthropic account cycle; Cursor has its own allowance;
// Antigravity runs on a subscription Google doesn't meter; anything else is
// local hardware with no cycle to report.
func fleetBucket(agent string) string {
	switch agent {
	case aggregate.AgentClaudeCode, aggregate.AgentOpenClaw:
		return "Anthropic"
	case aggregate.AgentCursor:
		return "Cursor"
	case aggregate.AgentAntigravity:
		return "suscripción gratis"
	default:
		return "local"
	}
}

// fleetProvider maps an agent to the Plan.Provider() string that covers it, so
// fleetCycle can find that agent's cycle among report.Cycles without
// re-deriving the coverage rules that already live in plan.go. It returns ""
// for agents no Plan covers.
func fleetProvider(agent string) string {
	switch agent {
	case aggregate.AgentClaudeCode, aggregate.AgentOpenClaw:
		return "Anthropic (Claude Max)"
	case aggregate.AgentCursor:
		return "Cursor"
	default:
		return ""
	}
}

// fleetCycle picks the one cycle to show for an agent: its provider's session
// window or billing month when one is in flight (the cycle someone deciding
// where to route work actually cares about), falling back to whatever other
// cycle that provider has (e.g. the weekly cap with no ceiling to report).
func fleetCycle(cycles []quota.CycleStatus, agent string) (quota.CycleStatus, bool) {
	provider := fleetProvider(agent)
	if provider == "" {
		return quota.CycleStatus{}, false
	}
	var fallback quota.CycleStatus
	haveFallback := false
	for _, cs := range cycles {
		if cs.Cycle.Provider != provider {
			continue
		}
		if cs.Cycle.Name == quota.CycleSessionWindow || cs.Cycle.Name == quota.CycleBillingMonth {
			return cs, true
		}
		if !haveFallback {
			fallback, haveFallback = cs, true
		}
	}
	return fallback, haveFallback
}

// fleetFillLine is the routing signal: how full the agent's cycle is and when
// it runs out, or — when there's no percentage to show — the specific reason,
// same as the full report does (an omitted reason reads as "0%", which is a
// claim this data doesn't support).
func fleetFillLine(report quota.Report, agent string) string {
	cs, ok := fleetCycle(report.Cycles, agent)
	if !ok {
		if reason, unmetered := report.Unmetered[agent]; unmetered {
			return "sin cuota medible: " + reason
		}
		return "sin ciclo activo"
	}
	if reason := noFillReason(cs.Cycle, cs.FillKnown); reason != "" {
		return "sin % calculable: " + reason
	}
	return fmt.Sprintf("%s · %s", percent(cs.Fill), forecastLine(cs.Forecast))
}

// fleetCostLine is the secondary, always-labeled figure (the column header
// carries the "costo equivalente estimado" label so it isn't repeated on every
// row). An agent absent from the period's breakdown didn't run, not "cost
// nothing to run".
func fleetCostLine(byAgent []aggregate.AgentTotals, agent string) string {
	for _, a := range byAgent {
		if a.Agent == agent {
			return usd(a.Totals.CostUSD)
		}
	}
	return "sin actividad en el periodo"
}

// fleetMeasureNote says whether the fill shown is a measured figure or a
// range, per §3.2's requirement to surface Measure.Exact rather than let a
// range read as a precise percentage.
func fleetMeasureNote(report quota.Report, agent string) string {
	cs, ok := fleetCycle(report.Cycles, agent)
	if !ok || !cs.FillKnown {
		return "—"
	}
	if cs.Fill.Exact {
		return "exacta"
	}
	return "rango"
}

// clock prints a cycle boundary with its date: a 5-hour window fits in one day
// but a billing month does not, and "01" alone next to "15:04" is ambiguous.
func clock(t time.Time) string { return t.Format("02/01 15:04") }

// duration renders a span the way someone waiting on it would say it. Anything
// already elapsed reads as zero rather than as a negative number of minutes.
func duration(d time.Duration) string {
	if d <= 0 {
		return "0 min"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d d %d h", int(d.Hours())/24, int(d.Hours())%24)
}
