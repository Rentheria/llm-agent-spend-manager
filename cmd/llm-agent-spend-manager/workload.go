package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/Rentheria/llm-agent-spend-manager/internal/workload"
)

// This file renders Capa 2 (what shape each load had) and Capa 3 (what that
// shape cost through each route that ran it). It sits after the context-risk
// table because it generalizes it: that table names the sessions that ran too
// long, this one says which KIND of load each session was and therefore which
// lever it responds to.

// classLabel is the user-facing wording for each workload shape. The ids stay
// stable in the JSON; only this map decides how they read.
var classLabel = map[string]string{
	workload.ClassLongConversation: "Conversación larga",
	workload.ClassMechanicalBurst:  "Ráfaga mecánica",
	workload.ClassBigContext:       "Trabajo de contexto grande",
	workload.ClassOneShot:          "Disparo único",
	workload.ClassUnclassified:     "Sin clasificar",
}

// writeWorkloads prints the shapes table and then, for each shape that actually
// appeared, the per-route plan.
func writeWorkloads(out io.Writer, report workload.Report) {
	if report.Streams == 0 {
		return
	}
	writeWorkloadShapes(out, report)
	writeRoutePlans(out, report)
}

// writeWorkloadShapes is the Capa 2 table: how the fleet's load splits by shape,
// and which lever each shape responds to. The unclassified row is printed with
// the same weight as the rest — hiding it would overstate how much of the fleet
// this understands.
func writeWorkloadShapes(out io.Writer, report workload.Report) {
	fmt.Fprintln(out, "\nFORMA DE LA CARGA (qué palanca aplica a cada una)")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  FORMA\tCARGAS\tTURNOS\tCOSTO EQUIV.\t%\tPALANCA")
	for _, c := range report.Classes {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%.1f%%\t%s\n",
			classLabel[c.Class], thousands(c.Streams), thousands(c.Turns),
			usd(c.CostUSD), c.CostShare*100, palancaCorta(c.Class))
	}
	u := report.Unclassified
	fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%.1f%%\t%s\n",
		classLabel[workload.ClassUnclassified], thousands(u.Streams), thousands(u.Turns),
		usd(u.CostUSD), u.CostShare*100, "— falta el dato, no se fuerza a la forma más cercana")
	tw.Flush()

	fmt.Fprintf(out, "  Una carga = un hilo de contexto (una sesión con subagentes corre varios). %s de %s clasificadas.\n",
		thousands(report.Classified), thousands(report.Streams))
	for _, r := range u.Reasons {
		fmt.Fprintf(out, "    · %s cargas sin clasificar: %s\n", thousands(r.Streams), r.Reason)
	}
}

// palancaCorta is the one-line version of the lever for the table; the full
// wording is printed under each shape's plan.
func palancaCorta(class string) string {
	switch class {
	case workload.ClassLongConversation:
		return "tope de contexto / corte por tarea"
	case workload.ClassMechanicalBurst:
		return "rutear a modelo o agente barato"
	case workload.ClassBigContext:
		return "no releer: punteros, no archivos"
	case workload.ClassOneShot:
		return "no pedir caché"
	default:
		return "—"
	}
}

// writeRoutePlans is Capa 3: for every shape that ran, what each route charged
// for it and what the routes with no data are. A plan that compares a measured
// route against an estimated one has to say so — that's what the ≈ marker and
// the "falta el dato" lines are for.
func writeRoutePlans(out io.Writer, report workload.Report) {
	fmt.Fprintln(out, "\nPLAN DE AHORRO POR RUTA (contrafactual medido, no opinión)")
	printed := 0
	for _, c := range report.Classes {
		if c.Streams == 0 {
			continue
		}
		printed++
		writeClassPlan(out, c)
	}
	if printed == 0 {
		fmt.Fprintln(out, "  Ninguna carga de esta ventana cayó en una forma conocida: no hay plan que derivar.")
		return
	}
	fmt.Fprintf(out, "\n  %s\n", report.Caveat)
}

func writeClassPlan(out io.Writer, c workload.ClassPlan) {
	fmt.Fprintf(out, "\n  %s — %s en %s cargas (%.1f%% del costo equivalente)\n",
		classLabel[c.Class], usd(c.CostUSD), thousands(c.Streams), c.CostShare*100)
	fmt.Fprintf(out, "        Palanca: %s\n", c.Lever)

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "        RUTA\tCOSTO EQUIV.\tCOSTO/TURNO\tTURNOS\tCARGAS\tMODELO DOMINANTE")
	for _, r := range c.Routes {
		fmt.Fprintf(tw, "        %s\t%s\t%s\t%s\t%s\t%s\n",
			rutaConConfianza(r), usd(r.CostUSD), usd4(r.CostPerTurnUSD),
			thousands(r.Turns), thousands(r.Streams), modeloDominante(r))
	}
	tw.Flush()

	for _, m := range c.Missing {
		fmt.Fprintf(out, "        Falta el dato · %s: %s\n", m.Route, m.Reason)
	}
	writeCounterfactual(out, c.ByRoute)
	writeCounterfactual(out, c.ByModel)
}

// rutaConConfianza marks an activity-tier route so its figure is never read as
// equivalent evidence to a measured one (the "regla de oro" of
// docs/architecture.md §3.3).
func rutaConConfianza(r workload.RouteCost) string {
	if r.Measured {
		return r.Route
	}
	return "≈ " + r.Route + " (estimado)"
}

// modeloDominante names the model that carried most of the route's cost for this
// shape, plus how many models it ran in total — the mix is what makes "rutear a
// modelo barato" checkable instead of aspirational.
func modeloDominante(r workload.RouteCost) string {
	if len(r.ByModel) == 0 {
		return "—"
	}
	top := r.ByModel[0]
	name := top.Model
	if name == "" {
		name = "(sin modelo reportado)"
	}
	if len(r.ByModel) == 1 {
		return name
	}
	return fmt.Sprintf("%s (+%s)", name, plural(len(r.ByModel)-1, "modelo", "modelos"))
}

// plural keeps counted nouns readable ("1 turno", "13,572 turnos"); a report
// that says "los 1 turnos" reads as a machine and gets trusted like one.
func plural(n int, singular, many string) string {
	if n == 1 {
		return "1 " + singular
	}
	return thousands(n) + " " + many
}

// writeCounterfactual prints one comparison, or says why there wasn't one. The
// missing case gets as many lines as the present one on purpose: "falta el dato"
// is an answer, not an omission.
func writeCounterfactual(out io.Writer, cf workload.Counterfactual) {
	if !cf.Known {
		fmt.Fprintf(out, "        Contrafactual por %s: %s\n", cf.Dimension, cf.Reason)
		return
	}
	fmt.Fprintf(out, "        Contrafactual por %s: la opción más barata medida es %s a %s/turno, observada en %s de esta forma; "+
		"mover %s habría evitado %s.\n",
		cf.Dimension, cf.Cheapest, usd4(cf.CheapestCostPerTurnUSD),
		plural(cf.CheapestTurns, "turno", "turnos"), plural(cf.MovableTurns, "turno", "turnos"), usd(cf.SavingsUSD))
	if cf.Capped() {
		fmt.Fprintf(out, "          Topado por la observación: %s fueron por otra opción, pero solo se puede reclamar "+
			"lo que la barata ya demostró cargar (%s). El resto sería extrapolar, no medir.\n",
			plural(cf.TurnsElsewhere, "turno", "turnos"), plural(cf.CheapestTurns, "turno", "turnos"))
	}
	if len(cf.Excluded) > 0 {
		fmt.Fprintf(out, "          Fuera de la comparación (costo equivalente cero, corre local o sin precio): %s\n",
			listaCorta(cf.Excluded))
	}
}

// listaCorta joins names for a one-line note.
func listaCorta(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
