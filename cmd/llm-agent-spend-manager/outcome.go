package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/advise"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/outcome"
)

// defaultReposDir and defaultLogPath are where the fleet's changes live on this
// machine. They are flags with defaults, not constants: another machine keeps its
// checkouts and its log somewhere else, and a path baked into the binary would
// silently produce an empty ledger there instead of an error.
const (
	defaultReposDir = "Develop"
	defaultLogPath  = ".openclaw/workspace/log.ndjson"
)

// cmdOutcome prints the outcome ledger: the changes that were really made, and for
// each tracked metric whether its LEVEL moved and which of those changes was in
// the window when it did.
//
// This is the command that lets the report say "this improved after that change,
// and that other one did nothing" instead of only "this is not improving" — and,
// just as often, that the data supports neither claim. It is a separate command
// from `advise` because it needs I/O the analyzer deliberately doesn't do: advise
// is a pure function of the usage records, and commits and log entries are not
// usage records.
func cmdOutcome(args []string, out io.Writer) int {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(out, "cannot resolve home directory:", err)
		return 1
	}

	fs := flag.NewFlagSet("outcome", flag.ContinueOnError)
	fs.SetOutput(out)
	window := fs.String("window", "all", "time window: today | week | all")
	reposDir := fs.String("repos", filepath.Join(homeDir, defaultReposDir),
		"directory holding the fleet's git repositories (scanned one level deep)")
	logPath := fs.String("log", filepath.Join(homeDir, defaultLogPath), "path to the fleet log (ndjson)")
	asJSON := fs.Bool("json", false, "emit the ledger as JSON instead of a text report")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	records, err := aggregate.Collect(homeDir)
	if err != nil {
		fmt.Fprintln(out, "failed to read usage data:", err)
		return 1
	}
	changes, err := outcome.CollectChanges(context.Background(), *reposDir, *logPath)
	if err != nil {
		fmt.Fprintln(out, "failed to read the marked changes:", err)
		return 1
	}

	w := parseWindow(*window)
	filtered := aggregate.FilterWindow(records, w, time.Now())
	report := advise.Analyze(filtered, string(w), time.Local)
	ledger := advise.BuildOutcomeLedger(filtered, report, changes.Changes, time.Local)

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Changes  outcome.ChangeLedger `json:"changes"`
			Outcomes advise.OutcomeLedger `json:"outcomes"`
		}{changes, ledger}); err != nil {
			fmt.Fprintln(out, "failed to encode the ledger:", err)
			return 1
		}
		return 0
	}
	writeOutcome(out, w, changes, ledger)
	return 0
}

// writeOutcome renders the whole text report.
func writeOutcome(out io.Writer, w aggregate.Window, changes outcome.ChangeLedger, ledger advise.OutcomeLedger) {
	fmt.Fprintf(out, "llm-agent-spend-manager — bitácora de resultado · %s\n\n", windowLabel[w])
	writeMarkedChanges(out, changes)
	writeLevelShifts(out, ledger.Outcomes)
	writeAdviceLoop(out, ledger)
	fmt.Fprintf(out, "\n%s\n", aggregate.CostDisclaimer)
}

// writeMarkedChanges prints what the metrics get contrasted against, including
// what could not be read. The unreadable count is not a footnote: without it,
// "ningún cambio en esa ventana" and "no pudimos leer esa parte" look identical.
func writeMarkedChanges(out io.Writer, changes outcome.ChangeLedger) {
	fmt.Fprintln(out, "CAMBIOS MARCADOS (contra esto se contrasta la métrica)")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "  Commits\t%s\ten %s repos bajo el directorio escaneado\n",
		thousands(changes.Commits), thousands(len(changes.Repos)))
	fmt.Fprintf(tw, "  Entradas de log\t%s\tde las cuales %s marcan un cambio real\n",
		thousands(changes.LogEntries), thousands(changes.LogEntries-changes.LogNotAChange))
	fmt.Fprintf(tw, "  Líneas ilegibles\t%s\tno se cuentan (JSON inválido en el log)\n",
		thousands(changes.LogUnreadable))
	fmt.Fprintf(tw, "  Total de eventos\t%s\t%s\n", thousands(len(changes.Changes)), changeSpan(changes.Changes))
	tw.Flush()
}

// changeSpan describes the period the marked changes cover.
func changeSpan(changes []outcome.Change) string {
	if len(changes) == 0 {
		return "(ninguno)"
	}
	return fmt.Sprintf("del %s al %s",
		changes[0].At.In(time.Local).Format("2006-01-02"),
		changes[len(changes)-1].At.In(time.Local).Format("2006-01-02"))
}

// verdictLabel is the user-facing wording for each level-change verdict. "Muestra
// insuficiente" is a result like any other, so it reads as one.
var verdictLabel = map[string]string{
	outcome.VerdictShiftDown:          "BAJÓ DE NIVEL",
	outcome.VerdictShiftUp:            "SUBIÓ DE NIVEL",
	outcome.VerdictNoShift:            "sin cambio de nivel",
	outcome.VerdictInsufficientSample: "muestra insuficiente",
}

// writeLevelShifts prints the summary table and then, for every series whose level
// actually moved, the arithmetic behind it and the changes that were in scope.
func writeLevelShifts(out io.Writer, outcomes []advise.Outcome) {
	fmt.Fprintln(out, "\n¿CAMBIÓ DE NIVEL? (medias comparadas contra su dispersión, tipo CUSUM)")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  MÉTRICA\tDÍAS\tANTES\tDESPUÉS\tΔ\tσ AGRUPADA\tDESPLAZ.\tVEREDICTO")
	for _, o := range outcomes {
		s := o.Shift
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			o.Series, thousands(len(s.Series)), meanCell(o, s.MeanBefore), meanCell(o, s.MeanAfter),
			deltaCell(o.Series, s), meanCell(o, s.PooledStdDev), shiftCell(s), verdictLabel[s.Verdict])
	}
	tw.Flush()
	fmt.Fprintln(out, "  Se reporta UN cambio de nivel por métrica: el más grande de la serie (el pico del CUSUM).")
	fmt.Fprintln(out, "  Un segundo escalón dentro del mismo periodo NO aparece aquí — acota la ventana para verlo.")

	for _, o := range outcomes {
		if o.Shift.Verdict == outcome.VerdictShiftDown || o.Shift.Verdict == outcome.VerdictShiftUp {
			writeShiftDetail(out, o)
		}
	}
}

// metricCell renders a series value in its own units: a share reads as a
// percentage, a cost per turn reads as money.
func metricCell(series string, value float64) string {
	if series == advise.SeriesCostPerTurn {
		return usd4(value)
	}
	return fmt.Sprintf("%.1f%%", value*100)
}

// meanCell is metricCell for the summary table, where a sample too small to split
// has no means at all. Printing the zeroed struct field there would put a $0.0000
// on the page that nobody measured — the one thing this whole project refuses to
// do — so it prints the same em dash the rest of the row uses.
func meanCell(o advise.Outcome, value float64) string {
	if o.Shift.Verdict == outcome.VerdictInsufficientSample {
		return "—"
	}
	return metricCell(o.Series, value)
}

func deltaCell(series string, s outcome.LevelShift) string {
	if s.Verdict == outcome.VerdictInsufficientSample {
		return "—"
	}
	if series == advise.SeriesCostPerTurn {
		return fmt.Sprintf("%+.1f%%", s.DeltaPct)
	}
	return fmt.Sprintf("%+.1f pp", s.Delta*100)
}

// shiftCell renders the figure the verdict is decided on. A pooled dispersion of
// zero has no sigmas to report — the step is clean — and says so instead of
// printing a division that didn't happen.
func shiftCell(s outcome.LevelShift) string {
	if s.Verdict == outcome.VerdictInsufficientSample {
		return "—"
	}
	if s.PooledStdDev == 0 {
		return "escalón limpio"
	}
	return fmt.Sprintf("%+.1fσ", s.ShiftStdDevs)
}

// writeShiftDetail spells out one level change so the reader can redo it by hand:
// the two means with their day counts, the dispersion they were judged against,
// the CUSUM peak that picked the day, and the marked changes in the window.
func writeShiftDetail(out io.Writer, o advise.Outcome) {
	s := o.Shift
	fmt.Fprintf(out, "\n  [%s] %s el %s\n", o.Series, verdictLabel[s.Verdict], s.ChangeDay)
	fmt.Fprintf(out, "        Cuenta:    media %s en %d días → %s en %d días (Δ %s); "+
		"σ agrupada %s; desplazamiento %s\n",
		metricCell(o.Series, s.MeanBefore), s.DaysBefore, metricCell(o.Series, s.MeanAfter), s.DaysAfter,
		deltaCell(o.Series, s), metricCell(o.Series, s.PooledStdDev), shiftCell(s))
	fmt.Fprintf(out, "        CUSUM:     pico %.4f (en unidades de la métrica); el día sale de ahí\n", s.CusumPeak)
	writeCandidates(out, o.Attribution)
}

// candidateLimit bounds how many marked changes a window lists. When it truncates
// it says how many it dropped: a list that quietly stopped at five would read like
// a window with five changes in it.
const candidateLimit = 8

// writeCandidates prints the marked changes that were in the attribution window,
// and the two things the report is obliged to say about them: that several changes
// in one window are not separable, and that coincidence in time is not causality.
func writeCandidates(out io.Writer, attribution outcome.Attribution) {
	// The span is measured in ACTIVE days, so it can cover a lot of calendar when
	// the fleet was quiet. Saying so keeps a wide window from reading like a bug.
	fmt.Fprintf(out, "        Ventana:   %s … %s (%d cambios marcados; %d días activos hacia atrás,\n",
		attribution.From.Format("2006-01-02"),
		attribution.Through.AddDate(0, 0, -1).Format("2006-01-02"),
		len(attribution.Candidates), outcome.AttributionLagDays)
	fmt.Fprintln(out, "                   los días de calendario sin actividad medida van incluidos)")

	if len(attribution.Candidates) == 0 {
		fmt.Fprintln(out, "        Candidato: ninguno. El nivel se movió y no hay cambio marcado justo antes:")
		fmt.Fprintln(out, "                   lo que lo movió no está en esta bitácora.")
	}
	for i, c := range attribution.Candidates {
		if i == candidateLimit {
			fmt.Fprintf(out, "                   … y %d más (ver --json)\n", len(attribution.Candidates)-candidateLimit)
			break
		}
		fmt.Fprintf(out, "                   %s %s %s · %s · %s\n",
			c.At.In(time.Local).Format("2006-01-02 15:04"), c.Source, c.Ref, changeWhere(c), recorta(c.Note, 72))
	}
	if attribution.InseparableNote != "" {
		fmt.Fprintf(out, "        Ojo:       %s\n", attribution.InseparableNote)
	}
	if attribution.Separable {
		fmt.Fprintln(out, "        Ojo:       Un solo cambio marcado en la ventana. Es el único candidato,")
		fmt.Fprintln(out, "                   lo cual no lo vuelve la causa.")
	}
	fmt.Fprintf(out, "        Cautela:   %s\n", attribution.Caveat)
}

// changeWhere names the repository a commit came from, or the actor for a log
// entry — whichever locates the change for someone who wants to go look at it.
func changeWhere(c outcome.Change) string {
	if c.Repo != "" {
		return c.Repo
	}
	return c.Actor
}

// writeAdviceLoop is the section the whole layer exists for: for each piece of
// advice the report carries, what happened to the metric it was supposed to move.
func writeAdviceLoop(out io.Writer, ledger advise.OutcomeLedger) {
	fmt.Fprintln(out, "\nCICLO CONSEJO → CAMBIO REAL → ¿SE MOVIÓ LA MÉTRICA?")
	graded := 0
	for _, o := range ledger.Outcomes {
		if o.FindingID == "" {
			continue
		}
		graded++
		fmt.Fprintf(out, "\n  [%s]%s %s\n", o.FindingID, escalatedMark(o.Escalated), o.FindingText)
		fmt.Fprintf(out, "        Serie:     %s (%d días activos medidos)\n", o.Series, len(o.Shift.Series))
		fmt.Fprintf(out, "        Resultado: %s%s\n", verdictLabel[o.Shift.Verdict], shiftDay(o.Shift))
		fmt.Fprintf(out, "        Lectura:   %s\n", adviceReading(o))
	}
	if graded == 0 {
		fmt.Fprintln(out, "  Ningún consejo del reporte tiene serie diaria que medir en esta ventana.")
	}
	writeUnmeasured(out, ledger.Unmeasured)
}

func escalatedMark(escalated bool) string {
	if escalated {
		return " (escalado: ya había dejado de ser un tip)"
	}
	return ""
}

func shiftDay(s outcome.LevelShift) string {
	if s.ChangeDay == "" {
		return ""
	}
	if s.Verdict == outcome.VerdictShiftDown || s.Verdict == outcome.VerdictShiftUp {
		return " el " + s.ChangeDay
	}
	return ""
}

// adviceReading states what the ledger concluded about one piece of advice, in the
// only four shapes the data allows.
func adviceReading(o advise.Outcome) string {
	switch o.Shift.Verdict {
	case outcome.VerdictInsufficientSample:
		return "Muestra insuficiente: no hay días activos suficientes a los dos lados de un día candidato " +
			"para comparar medias. No es un fallo — es lo que estos datos aguantan."
	case outcome.VerdictNoShift:
		return "El nivel de la métrica NO cambió: la diferencia entre las dos medias cabe dentro de su propia " +
			"dispersión diaria. Con los cambios que se hicieron, esta métrica sigue donde estaba."
	case outcome.VerdictShiftDown:
		return "La métrica bajó de nivel. " + separabilityReading(o.Attribution)
	default:
		return "La métrica SUBIÓ de nivel: empeoró. " + separabilityReading(o.Attribution)
	}
}

func separabilityReading(attribution outcome.Attribution) string {
	switch {
	case len(attribution.Candidates) == 0:
		return "Ningún cambio marcado cae en la ventana, así que la bitácora no tiene a qué atribuirlo."
	case attribution.Separable:
		return "Hay un único cambio marcado en la ventana (arriba). Es el único candidato; no es la causa probada."
	default:
		return fmt.Sprintf("Hay %d cambios marcados en la ventana y NO son separables con estos datos (arriba).",
			len(attribution.Candidates))
	}
}

// writeUnmeasured admits the advice this layer cannot grade. Leaving it out would
// make the ledger look like a verdict on the whole report.
func writeUnmeasured(out io.Writer, unmeasured []advise.UnmeasuredAdvice) {
	if len(unmeasured) == 0 {
		return
	}
	fmt.Fprintln(out, "\n  SIN SERIE DIARIA (no se puede calificar, y por eso no se califica)")
	for _, u := range unmeasured {
		fmt.Fprintf(out, "        %s%s · métrica %s\n", u.FindingID, escalatedMark(u.Escalated), u.MetricName)
	}
	fmt.Fprintf(out, "        Por qué: %s.\n", advise.NoSeriesReason)
}
