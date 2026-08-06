package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/humanize"
)

// cmdStatus prints the per-agent estimated-equivalent cost as a text table for
// the chosen window. Same wording rules as every other surface: the $ is a
// "costo equivalente estimado", never "gasto real".
func cmdStatus(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(out)
	window := fs.String("window", "today", "time window: today | week | all")
	if err := fs.Parse(args); err != nil {
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
	writeStatus(out, w, aggregate.ByAgent(filtered), aggregate.ByMode(filtered), aggregate.Grand(filtered))
	return 0
}

// parseWindow maps a flag value to a Window, defaulting to today for anything
// empty or unrecognized (mirrors the server's parseWindow).
func parseWindow(v string) aggregate.Window {
	switch aggregate.Window(v) {
	case aggregate.WindowWeek:
		return aggregate.WindowWeek
	case aggregate.WindowAll:
		return aggregate.WindowAll
	default:
		return aggregate.WindowToday
	}
}

var windowLabel = map[aggregate.Window]string{
	aggregate.WindowToday: "hoy (desde medianoche local)",
	aggregate.WindowWeek:  "esta semana (desde el lunes)",
	aggregate.WindowAll:   "todo el historial",
}

// modeLabel is the user-facing name for each execution mode. An unlisted mode
// falls back to its raw key so a new mode never renders blank.
var modeLabel = map[string]string{
	aggregate.ModeInteractive: "interactivo (chat)",
	aggregate.ModeCron:        "cron / heartbeat",
	aggregate.ModeEditor:      "editor (Cursor/Antigravity)",
}

// writeStatus renders the header, the per-agent rows, the per-mode breakdown,
// the fleet total, and the disclaimer to out.
func writeStatus(out io.Writer, w aggregate.Window, byAgent []aggregate.AgentTotals, byMode []aggregate.ModeTotals, grand aggregate.Totals) {
	fmt.Fprintf(out, "llm-agent-spend-manager — %s · %s\n\n", aggregate.CostLabel, windowLabel[w])

	if len(byAgent) == 0 {
		fmt.Fprintln(out, "Sin actividad en esta ventana.")
		fmt.Fprintf(out, "\n%s\n", aggregate.CostDisclaimer)
		return
	}

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENTE\tCOSTO EQUIV.\tTOKENS\tTURNOS")
	for _, a := range byAgent {
		writeTotalsRow(tw, a.Agent, a.Totals)
	}
	writeTotalsRow(tw, "TOTAL FLOTA", grand)
	tw.Flush()

	writeModeBreakdown(out, byMode)

	if grand.ActivityTurns > 0 {
		fmt.Fprintf(out, "\n%s\n", activityLegend)
	}
	fmt.Fprintf(out, "\n%s\n", aggregate.CostDisclaimer)
}

// activityLegend explains the ≈range rows so the lower-confidence, activity-only
// figures (Cursor/Antigravity) are never read as measured spend. The full visual
// hierarchy on the web dashboard is T12.
const activityLegend = "≈ rango = actividad estimada (sin tokens medidos, p.ej. Cursor): " +
	"confianza menor que el costo equivalente estimado de los agentes medidos."

// writeTotalsRow prints one totals row, rendering activity-tier buckets as a
// range (≈low–high) and measured ones as a single figure.
func writeTotalsRow(tw io.Writer, label string, t aggregate.Totals) {
	fmt.Fprintf(tw, "%s\t%s\t%s\t%d%s\n",
		label, costCell(t), tokensCell(t), t.Turns, unpriced(t.UnpricedTurns))
}

// costCell shows a single estimated-equivalent cost for measured totals, or a
// range for totals that include activity-tier turns. When an activity total has
// no priced model at all (e.g. Antigravity/Gemini), it shows "—" rather than a
// misleading $0.00 — the tokens still render, and "(+N sin precio)" flags why.
func costCell(t aggregate.Totals) string {
	if t.ActivityTurns == 0 {
		return usd(t.CostUSD)
	}
	if t.CostLowUSD == 0 && t.CostHighUSD == 0 {
		return "—"
	}
	return "≈" + usd(t.CostLowUSD) + "–" + usd(t.CostHighUSD)
}

// tokensCell mirrors costCell for token counts.
func tokensCell(t aggregate.Totals) string {
	if t.ActivityTurns == 0 {
		return thousands(t.TotalTokens)
	}
	return "≈" + thousands(t.TokensLow) + "–" + thousands(t.TokensHigh)
}

// writeModeBreakdown renders the per-mode table (interactive vs cron/heartbeat),
// so a scheduled-run heavy total is visible rather than hidden
// inside the agent's single number.
func writeModeBreakdown(out io.Writer, byMode []aggregate.ModeTotals) {
	if len(byMode) == 0 {
		return
	}
	fmt.Fprintln(out, "\nPor modo:")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODO\tCOSTO EQUIV.\tTOKENS\tTURNOS")
	for _, m := range byMode {
		writeTotalsRow(tw, modeName(m.Mode), m.Totals)
	}
	tw.Flush()
}

// modeName maps a mode key to its label, falling back to the raw key.
func modeName(mode string) string {
	if label, ok := modeLabel[mode]; ok {
		return label
	}
	return mode
}

func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }

// unpriced annotates a row when some turns used a model with no price in the
// table, so the reader knows the cost is a lower bound.
func unpriced(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (+%d sin precio)", n)
}

// thousands formats an int with comma separators
// (e.g. 10107657766 -> "10,107,657,766").
func thousands(n int) string { return humanize.Int(n) }
