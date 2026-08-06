package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/advise"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/bootfiles"
	"github.com/Rentheria/llm-agent-spend-manager/internal/contextcurve"
)

// cmdAdvise prints the self-improvement report: where the equivalent cost really
// goes (per billable bucket, not per token), whether efficiency is improving,
// and the concrete findings derived from the data. --json emits the same report
// as machine-readable output so another agent can consume it, and --alerts emits
// only the actionable part of it, for the unattended notifier described in
// deploy/openclaw/README.md.
func cmdAdvise(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("advise", flag.ContinueOnError)
	fs.SetOutput(out)
	window := fs.String("window", "all", "time window: today | week | all")
	asJSON := fs.Bool("json", false, "emit the report as JSON instead of a text table")
	asAlerts := fs.Bool("alerts", false,
		"emit only the actionable items (findings and escalations) as a JSON feed, each with a stable fingerprint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Rejected rather than resolved by precedence: a silent winner between two
	// output flags would send an unattended caller the wrong payload without
	// saying so, and the whole point of --alerts is being consumed unattended.
	if *asJSON && *asAlerts {
		fmt.Fprintln(out, "--json and --alerts are mutually exclusive: --alerts is the actionable subset of --json")
		return 2
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(out, "cannot resolve home directory:", err)
		return 1
	}
	records, err := aggregate.Collect(homeDir)
	if err != nil {
		fmt.Fprintln(out, "failed to read usage data:", err)
		return 1
	}

	w := parseWindow(*window)
	filtered := aggregate.FilterWindow(records, w, time.Now())
	report := advise.Analyze(filtered, string(w), time.Local)

	// The boot-file weight is measured off disk, not off the usage records, so it
	// is attached after the analysis. This is also the only surface that WRITES
	// the snapshot: the dashboard reads it (see internal/server).
	bootReport, snapshot, err := bootfiles.Collect(os.Getenv, homeDir, time.Now())
	if err != nil {
		fmt.Fprintln(out, "failed to measure boot files:", err)
		return 1
	}
	if err := bootfiles.SaveSnapshot(bootfiles.SnapshotPath(homeDir), snapshot); err != nil {
		fmt.Fprintln(out, "failed to save the boot-file snapshot:", err)
		return 1
	}
	report = advise.WithBootFiles(report, bootReport)

	// Both machine surfaces are emitted after WithBootFiles, so E-08 is in them.
	if *asAlerts {
		return encodeJSON(out, advise.Alerts(report))
	}
	if *asJSON {
		return encodeJSON(out, report)
	}
	writeAdvice(out, w, report)
	return 0
}

// encodeJSON writes one machine-readable payload, indented so a human can still
// read what the notifier received when they go looking for why it fired.
func encodeJSON(out io.Writer, payload any) int {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintln(out, "failed to encode report:", err)
		return 1
	}
	return 0
}

// writeAdvice renders the whole text report.
func writeAdvice(out io.Writer, w aggregate.Window, report advise.Report) {
	fmt.Fprintf(out, "llm-agent-spend-manager — automejora · %s\n\n", windowLabel[w])
	if report.Fleet.Turns == 0 {
		fmt.Fprintln(out, "Sin actividad en esta ventana.")
		// El peso de los archivos de arranque no sale de los registros de uso: se
		// mide en disco. Una ventana sin turnos no lo vuelve irrelevante — el
		// impuesto se paga igual en el siguiente arranque.
		writeBootFiles(out, report.BootFiles)
		fmt.Fprintf(out, "\n%s\n", aggregate.CostDisclaimer)
		return
	}

	writeFleetEfficiency(out, report.Fleet)
	writeBuckets(out, report.Fleet.Buckets)
	writeAgentEfficiency(out, report.ByAgent)
	writeTopTasks(out, report.TopTasks)
	writeContextRisks(out, report.ContextRisks)
	writeWorkloads(out, report.Workloads) // T73 Capas 2 y 3 — ver workload.go
	writeTrend(out, report.Trend)
	writeBootFiles(out, report.BootFiles)
	writeFindings(out, report.Findings)
	writeEscalations(out, report.Escalations)
	fmt.Fprintf(out, "\n%s\n", aggregate.CostDisclaimer)
}

// writeFleetEfficiency prints the headline efficiency block for the whole fleet.
func writeFleetEfficiency(out io.Writer, fleet advise.AgentEfficiency) {
	fmt.Fprintln(out, "EFICIENCIA DE LA FLOTA")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "  Costo equivalente\t%s\n", usd(fleet.CostUSD))
	fmt.Fprintf(tw, "  Turnos / sesiones\t%s / %s\t(%.1f turnos por sesión)\n",
		thousands(fleet.Turns), thousands(fleet.Sessions), fleet.TurnsPerSession)
	fmt.Fprintf(tw, "  Costo por turno\t%s\n", usd4(fleet.CostPerTurnUSD))
	fmt.Fprintf(tw, "  Tokens por turno\t%s\n", thousands(fleet.TokensPerTurn))
	if fleet.Measured {
		fmt.Fprintf(tw, "  Contexto desde caché\t%.1f%%\t(reuso: %.1fx lo escrito)\n",
			fleet.CacheReadShare*100, fleet.CacheReuseRatio)
	}
	tw.Flush()
}

// writeBuckets prints the per-bucket cost split — the table that tells you which
// lever to pull, since a token's price depends entirely on which bucket it's in.
func writeBuckets(out io.Writer, buckets []advise.BucketCost) {
	if len(buckets) == 0 {
		return
	}
	fmt.Fprintln(out, "\nCOSTO EQUIVALENTE POR BUCKET FACTURABLE")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  BUCKET\tCOSTO EQUIV.\t%\tTOKENS")
	for _, b := range buckets {
		fmt.Fprintf(tw, "  %s\t%s\t%.1f%%\t%s\n", b.Bucket, usd(b.CostUSD), b.Share*100, thousands(b.Tokens))
	}
	tw.Flush()
}

// writeAgentEfficiency prints the per-agent comparison. Activity-tier agents show
// "n/a" where a measured-only ratio would otherwise print a meaningless 0.
func writeAgentEfficiency(out io.Writer, byAgent []advise.AgentEfficiency) {
	if len(byAgent) == 0 {
		return
	}
	fmt.Fprintln(out, "\nPOR AGENTE")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  AGENTE\tCOSTO/TURNO\tTOKENS/TURNO\tTURNOS\tSESIONES\tDESDE CACHÉ")
	for _, a := range byAgent {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			a.Agent, costPerTurnCell(a), tokensPerTurnCell(a),
			thousands(a.Turns), thousands(a.Sessions), cacheShareCell(a))
	}
	tw.Flush()
}

// costPerTurnCell / tokensPerTurnCell keep the project's core rule intact on this
// table too: an activity-tier figure is never shown as a point (T12). It gets the
// ≈ marker, and an agent with no priced model at all shows "—" rather than a
// $0.0000 that reads like "free".
func costPerTurnCell(a advise.AgentEfficiency) string {
	if a.Measured {
		return usd4(a.CostPerTurnUSD)
	}
	if a.CostUSD == 0 {
		return "—"
	}
	return "≈" + usd4(a.CostPerTurnUSD)
}

func tokensPerTurnCell(a advise.AgentEfficiency) string {
	if a.Measured {
		return thousands(a.TokensPerTurn)
	}
	return "≈" + thousands(a.TokensPerTurn)
}

// cacheShareCell renders the cache share, or n/a for agents with no measured
// token buckets.
func cacheShareCell(a advise.AgentEfficiency) string {
	if !a.Measured {
		return "n/a (estimado)"
	}
	return fmt.Sprintf("%.1f%%", a.CacheReadShare*100)
}

// writeTopTasks prints the most expensive tasks. This is the table that answers
// "qué me costó tanto y por qué": la etiqueta dice QUÉ se pidió, el costo CUÁNTO,
// y el bucket dominante POR QUÉ — porque el arreglo de una tarea cara en
// cache-read (achicar contexto) no es el mismo que el de una cara en output
// (pedir menos texto).
func writeTopTasks(out io.Writer, tareas []advise.TaskCost) {
	if len(tareas) == 0 {
		return
	}
	fmt.Fprintln(out, "\nTAREAS MÁS CARAS (una sesión = una tarea)")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  COSTO EQUIV.\t%\tTURNOS\tPOR QUÉ (bucket dominante)\tQUÉ SE PIDIÓ")
	for _, t := range tareas {
		fmt.Fprintf(tw, "  %s\t%.1f%%\t%s\t%s\t%s\n",
			usd(t.CostUSD), t.Share*100, thousands(t.Turns), porQue(t), recorta(t.Label, 64))
	}
	tw.Flush()
}

// porQue resume la causa del costo de una tarea en una celda.
func porQue(t advise.TaskCost) string {
	if t.TopBucket == "" {
		return "sin buckets medidos"
	}
	return fmt.Sprintf("%s %.0f%% · %s tok/turno", t.TopBucket, t.TopBucketShare*100, thousands(t.TokensPerTurn))
}

func recorta(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// writeContextRisks prints the conversations that ran past their own break-even.
// It sits right under the expensive-tasks table because it answers the question
// that table raises: a task is expensive in cache-read, fine — at which turn did
// it stop being worth continuing? That number is per session and measured, so the
// row can say "cut here" instead of "make sessions shorter". Silent when nothing
// crossed the line, same rule as the findings.
func writeContextRisks(out io.Writer, risks []advise.ContextRisk) {
	if len(risks) == 0 {
		return
	}
	fmt.Fprintln(out, "\nCONTEXTO: SESIONES PASADAS DE SU PUNTO DE NO-RETORNO")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  AHORRO SI SE CORTA\tCORTAR EN\tSE PASÓ POR\tCURVA DE CONTEXTO\tQUÉ SE PIDIÓ")
	for _, r := range risks {
		c := r.Curve
		fmt.Fprintf(tw, "  %s\tturno %s\t%s turnos\t%s\t%s\n",
			usd(c.SavingsUSD), thousands(c.NoReturnTurn), thousands(c.TurnsPastNoReturn),
			curvaDeContexto(c), recorta(r.Label, 44))
	}
	tw.Flush()
	fmt.Fprintln(out, "  El punto de no-retorno es el turno en que lo acumulado por arrastrar el contexto")
	fmt.Fprintln(out, "  supera lo que cuesta re-armarlo desde cero. Sale de la forma medida de cada sesión.")
}

// curvaDeContexto resume la forma medida de la sesión en una celda: de dónde
// arranca el contexto, qué tan rápido sube y hasta dónde llegó.
func curvaDeContexto(c contextcurve.Analysis) string {
	return fmt.Sprintf("%s +%s/turno → pico %s (%d reinicios)",
		thousands(c.Baseline), thousands(int(c.GrowthPerTurn)), thousands(c.PeakContext), c.Resets)
}

// writeTrend prints the self-improvement verdict.
func writeTrend(out io.Writer, t advise.Trend) {
	fmt.Fprintln(out, "\nAUTOMEJORA (¿estamos gastando mejor?)")
	if t.Direction == advise.TrendInsufficient {
		fmt.Fprintf(out, "  Sin datos suficientes: hacen falta %d días activos (%d por bloque) con costo medible.\n",
			2*t.WindowDays, t.WindowDays)
		return
	}
	fmt.Fprintf(out, "  Costo por turno: %s (%s…%s) → %s (%s…%s)\n",
		usd4(t.PriorCostPerTurn), firstDay(t.PriorDays), lastDay(t.PriorDays),
		usd4(t.RecentCostPerTurn), firstDay(t.RecentDays), lastDay(t.RecentDays))
	fmt.Fprintf(out, "  Cambio: %+.1f%% → %s\n", t.DeltaPct, trendLabel[t.Direction])
}

// trendLabel is the user-facing wording for each trend direction.
var trendLabel = map[string]string{
	advise.TrendImproving: "MEJORANDO (cada turno cuesta menos)",
	advise.TrendWorsening: "EMPEORANDO (cada turno cuesta más)",
	advise.TrendFlat:      "estable (dentro del ruido normal)",
}

// writeBootFiles prints the weight of the shared files every agent reads at the
// start of every session. Unlike the findings it is NOT silent when everything
// is fine: a threshold you only ever see when it's already crossed is a
// threshold nobody calibrates. Seeing the number drift toward the limit run
// after run is the whole point.
func writeBootFiles(out io.Writer, boot bootfiles.Report) {
	if len(boot.Files) == 0 {
		return
	}
	fmt.Fprintln(out, "\nARCHIVOS DE ARRANQUE (los lee cada agente en cada sesión)")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  ARCHIVO\tTAMAÑO\tUMBRAL\tCAMBIO\tESTADO")
	for _, f := range boot.Files {
		if !f.Measured() {
			fmt.Fprintf(tw, "  %s\t—\t%s\t—\t%s\n", f.Path, kb(f.ThresholdBytes), f.Unreadable)
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			f.Path, kb(f.Bytes), kb(f.ThresholdBytes), cambioDeTamano(f), estadoDeTamano(f))
	}
	tw.Flush()
	fmt.Fprintln(out, "  El umbral es 2x una línea base medida, no un límite validado: punto de partida")
	fmt.Fprintln(out, "  a recalibrar con más corridas (ver docs/archivos-arranque.md).")
}

// cambioDeTamano dice cuánto se movió el archivo y desde cuándo. Sin medición
// previa dice que no la hay: un "0" se leería como "no creció", que es una
// afirmación distinta.
func cambioDeTamano(f bootfiles.FileSize) string {
	if !f.HasPrevious {
		return "primera medición"
	}
	if f.DeltaBytes == 0 {
		return fmt.Sprintf("sin cambio desde %s", f.PreviousAt.Format("2006-01-02"))
	}
	signo := "+"
	delta := f.DeltaBytes
	if delta < 0 {
		signo, delta = "-", -delta
	}
	return fmt.Sprintf("%s%s desde %s", signo, kb(delta), f.PreviousAt.Format("2006-01-02"))
}

// estadoDeTamano traduce el umbral a algo accionable: cuánto margen queda antes
// de que esto se vuelva un hallazgo.
func estadoDeTamano(f bootfiles.FileSize) string {
	if f.OverThreshold {
		return fmt.Sprintf("CRUZÓ EL UMBRAL (+%s)", kb(f.Bytes-f.ThresholdBytes))
	}
	return fmt.Sprintf("dentro (margen %s)", kb(f.ThresholdBytes-f.Bytes))
}

// kb formatea un tamaño de archivo. Los archivos de arranque viven en decenas de
// KB, así que los bytes crudos solo estorban.
func kb(n int64) string { return fmt.Sprintf("%.1f KB", float64(n)/1024) }

// impactLabel keeps the CLI's impact wording in one place.
var impactLabel = map[string]string{
	advise.ImpactHigh:   "ALTO",
	advise.ImpactMedium: "MEDIO",
	advise.ImpactLow:    "BAJO",
}

// writeFindings prints each finding with the numbers behind it, so the reader can
// check the claim instead of trusting it.
func writeFindings(out io.Writer, findings []advise.Finding) {
	fmt.Fprintln(out, "\nHALLAZGOS Y TIPS")
	if len(findings) == 0 {
		fmt.Fprintln(out, "  Nada que reportar con estos datos: ningún patrón de desperdicio medible.")
		return
	}
	for _, f := range findings {
		fmt.Fprintf(out, "\n  [%s] %s — %s\n", impactLabel[f.Impact], f.ID, f.Title)
		fmt.Fprintf(out, "        Evidencia:   %s\n", f.Evidence)
		fmt.Fprintf(out, "        Qué hacer:   %s\n", f.Recommendation)
		if f.SavingsUSD > 0 {
			fmt.Fprintf(out, "        Ahorro tope: %s de costo equivalente\n", usd(f.SavingsUSD))
		}
	}
}

// metricLabel is the user-facing wording for each finding metric, so the CLI, the
// API and the dashboard don't each invent their own name for the same number.
var metricLabel = map[string]string{
	advise.MetricDominantBucketShare:    "share del bucket dominante",
	advise.MetricWastedCacheShare:       "share del costo en caché escrita y nunca leída",
	advise.MetricShortSessionShare:      "share de sesiones que mueren en pocos turnos",
	advise.MetricCronCostShare:          "share del costo en trabajo programado",
	advise.MetricUnpricedTurnShare:      "share de turnos medidos sin precio",
	advise.MetricSmallTurnCostShare:     "share del costo en turnos chicos del modelo caro",
	advise.MetricPastNoReturnCostShare:  "share del costo en contexto arrastrado de más",
	advise.MetricOversizedBootFileShare: "share de archivos de arranque por encima de su umbral",
}

// metricWording resolves a metric name to its label, keeping any qualifier the
// finding attached (E-01 names the bucket it's talking about).
func metricWording(metricName string) string {
	name, qualifier, hasQualifier := strings.Cut(metricName, ":")
	label, known := metricLabel[name]
	if !known {
		return metricName
	}
	if hasQualifier {
		return label + " (" + qualifier + ")"
	}
	return label
}

// writeEscalations prints the advice that stopped being advice. It goes after the
// tips on purpose: the reader has just seen what a tip looks like, and this
// section says which ones a tip can no longer fix. It stays silent when there's
// nothing to escalate, same rule as the findings.
func writeEscalations(out io.Writer, escalations []advise.Escalation) {
	if len(escalations) == 0 {
		return
	}
	fmt.Fprintln(out, "\nBRECHA DE ARQUITECTURA (esto ya no se arregla con un tip)")
	for _, e := range escalations {
		fmt.Fprintf(out, "\n  [%s] %s\n", e.FindingID, e.FindingTitle)
		fmt.Fprintf(out, "        Reincidencia: %d ventanas de %d días activos · %s %+.1f%%\n",
			e.Windows, e.WindowDays, metricWording(e.MetricName), e.MetricDeltaPct)
		fmt.Fprintf(out, "        Evidencia:    %s\n", e.Evidence)
		fmt.Fprintf(out, "        Qué falta:    %s\n", e.Mechanism)
	}
}

// usd4 formats a per-turn figure, which is often fractions of a cent.
func usd4(v float64) string { return fmt.Sprintf("$%.4f", v) }

func firstDay(days []string) string {
	if len(days) == 0 {
		return "?"
	}
	return days[0]
}

func lastDay(days []string) string {
	if len(days) == 0 {
		return "?"
	}
	return days[len(days)-1]
}
