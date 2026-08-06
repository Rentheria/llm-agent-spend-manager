package advise

import (
	"fmt"
	"sort"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/humanize"
)

// Finding ids. Stable so a reader (or a commit message) can refer to "E-02"
// across runs, the same way the security review refers to M-01/L-01.
const (
	FindingDominantBucket = "E-01"
	FindingCacheWasted    = "E-02"
	FindingSessionChurn   = "E-03"
	FindingCronOverhead   = "E-04"
	FindingUnpricedModels = "E-05"
	FindingModelMix       = "E-06"
	// FindingContextNoReturn names the sessions behind E-01's cache-read bill: the
	// ones that kept growing their context past the turn where restarting was
	// cheaper than carrying it.
	FindingContextNoReturn = "E-07"
)

// Metric names: the one number each finding's recommendation exists to move.
// They are stable ids (the surfaces render them through a label map, like impact
// and trend) and every one of them is a SHARE, so more work never looks like a
// regression. The recurrence check in escalation.go compares them window over
// window, and only compares a finding against itself under the same metric name.
const (
	MetricDominantBucketShare   = "dominant-bucket-share"
	MetricWastedCacheShare      = "wasted-cache-cost-share"
	MetricShortSessionShare     = "short-session-share"
	MetricCronCostShare         = "cron-cost-share"
	MetricUnpricedTurnShare     = "unpriced-turn-share"
	MetricSmallTurnCostShare    = "expensive-model-small-turn-cost-share"
	MetricPastNoReturnCostShare = "past-no-return-context-cost-share"
)

// shareOf is part over total, guarding the empty-window case where every total is
// zero and a share would be meaningless rather than 0.
func shareOf(part, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return part / total
}

// findings runs every rule and keeps the ones that actually fired, sorted by
// impact (high first) then by the savings they'd avoid. A rule that finds
// nothing wrong stays silent instead of padding the report — a report that always
// lists six problems trains you to ignore it.
func findings(records []aggregate.Record, sessions []session, report Report) []Finding {
	rules := []func([]aggregate.Record, []session, Report) (Finding, bool){
		dominantBucketFinding,
		cacheWastedFinding,
		sessionChurnFinding,
		cronOverheadFinding,
		unpricedModelsFinding,
		modelMixFinding,
		contextNoReturnFinding,
	}

	out := make([]Finding, 0, len(rules))
	for _, rule := range rules {
		if f, ok := rule(records, sessions, report); ok {
			out = append(out, f)
		}
	}
	sortFindings(out)
	return out
}

func impactRank(impact string) int {
	switch impact {
	case ImpactHigh:
		return 0
	case ImpactMedium:
		return 1
	default:
		return 2
	}
}

// dominantBucketFinding names the billable bucket that carries most of the
// equivalent cost. This is the single most useful line in the report: it decides
// whether the lever is "write less", "read less context" or "restart less".
func dominantBucketFinding(_ []aggregate.Record, _ []session, report Report) (Finding, bool) {
	if len(report.Fleet.Buckets) == 0 || !report.Fleet.Measured {
		return Finding{}, false
	}
	top := report.Fleet.Buckets[0]
	// Strictly more than half: at exactly 50/50 there are two levers, not one, and
	// calling either of them "the" target would be misleading.
	if top.Share <= dominantBucketShare {
		return Finding{}, false
	}

	return Finding{
		ID:     FindingDominantBucket,
		Title:  fmt.Sprintf("El %.0f%% del costo equivalente se va en un solo bucket: %s", top.Share*100, top.Bucket),
		Impact: impactOf(top.CostUSD, report.Fleet.CostUSD),
		Evidence: fmt.Sprintf("%s: $%.2f de $%.2f (%.1f%%) con %s tokens. Los tokens no valen igual: "+
			"un token de cache-read cuesta ~1/10 de uno de input y ~1/50 de uno de output al mismo modelo.",
			top.Bucket, top.CostUSD, report.Fleet.CostUSD, top.Share*100, humanInt(top.Tokens)),
		Recommendation: recommendationForBucket(top.Bucket),
		// Qualified by bucket: the recommendation is per-bucket, so a window where
		// a DIFFERENT bucket dominates carries different advice, not repeated
		// advice, and must not count toward the same recurrence.
		MetricName: MetricDominantBucketShare + ":" + top.Bucket,
		Metric:     top.Share,
	}, true
}

// recommendationForBucket maps the dominant bucket to the lever that actually
// moves it. Each one is a different problem, which is why naming the bucket
// matters more than the total.
func recommendationForBucket(bucket string) string {
	switch bucket {
	case BucketCacheRead:
		return "El costo escala con (tamaño del contexto × turnos), no con lo que escribes. " +
			"Palancas, de más a menos efectiva: (1) sesiones más cortas por tarea en vez de una sesión eterna que arrastra todo el historial; " +
			"(2) compactar/reiniciar cuando el hilo ya no aporta al problema actual; " +
			"(3) no releer archivos grandes que ya están en contexto; " +
			"(4) mandar el trabajo mecánico y de contexto chico a un modelo más barato."
	case BucketCacheWrite:
		return "Estás pagando el sobreprecio de escribir caché (~1.25x input) sin amortizarlo. " +
			"Pasa cuando las sesiones mueren antes de reusar el prefijo: menos reinicios, y prefijo de sistema estable (no cambiarlo entre turnos)."
	case BucketOutput:
		return "El output es el bucket más caro por token (~25x el cache-read del mismo modelo). " +
			"Palancas: pedir respuestas más cortas, evitar que el agente reimprima archivos completos que ya escribió en disco, " +
			"y no pedir resúmenes largos de trabajo que ya quedó documentado."
	default:
		return "El contexto se está mandando sin caché. Verifica que el prefijo (system prompt + instrucciones) " +
			"sea estable entre turnos: si cambia, el caché se invalida y todo se cobra como input nuevo."
	}
}

// cacheWastedFinding finds sessions that wrote cache and never read any back:
// the write surcharge bought nothing. Unlike most findings this one has a hard
// savings figure — that money is provably wasted, not "optimizable".
func cacheWastedFinding(_ []aggregate.Record, sessions []session, report Report) (Finding, bool) {
	var wastedUSD float64
	var wastedTokens, affected int
	for _, s := range sessions {
		tokens, costs := sumBuckets(s.turns)
		if tokens.CacheWrite > 0 && tokens.CacheRead == 0 {
			wastedUSD += costs.CacheWrite
			wastedTokens += tokens.CacheWrite
			affected++
		}
	}
	if affected == 0 || wastedUSD <= 0 {
		return Finding{}, false
	}

	return Finding{
		ID:     FindingCacheWasted,
		Title:  fmt.Sprintf("%d sesiones escribieron caché y nunca la leyeron", affected),
		Impact: impactOf(wastedUSD, report.Fleet.CostUSD),
		Evidence: fmt.Sprintf("%s tokens de cache-write en %d sesiones con 0 tokens de cache-read = $%.2f "+
			"de sobreprecio que no compró nada (escribir caché cuesta ~1.25x input; solo se paga solo si se lee después).",
			humanInt(wastedTokens), affected, wastedUSD),
		Recommendation: "Son sesiones que murieron antes del segundo turno. Si vienen de tareas de un solo disparo " +
			"(scripts, healthchecks, notificaciones), conviene que NO pidan caché, o agruparlas en una sesión que sí la reuse.",
		SavingsUSD: wastedUSD,
		MetricName: MetricWastedCacheShare,
		Metric:     shareOf(wastedUSD, report.Fleet.CostUSD),
	}, true
}

// sessionChurnFinding flags a fleet that opens many sessions that die before
// amortizing the prompt prefix they had to ingest. The savings figure is an upper
// bound: it assumes that work could have ridden an existing session, which isn't
// always true (a cron run legitimately starts fresh).
func sessionChurnFinding(_ []aggregate.Record, sessions []session, report Report) (Finding, bool) {
	if len(sessions) == 0 {
		return Finding{}, false
	}
	var short int
	var prefixUSD float64
	for _, s := range sessions {
		if len(s.turns) > shortSessionTurns {
			continue
		}
		short++
		_, costs := sumBuckets(s.turns[:1]) // first turn = the one that ingests the prefix
		prefixUSD += costs.Input + costs.CacheWrite + costs.CacheRead
	}

	share := float64(short) / float64(len(sessions))
	if share < shortSessionShareFloor || prefixUSD <= 0 {
		return Finding{}, false
	}
	return Finding{
		ID:     FindingSessionChurn,
		Title:  fmt.Sprintf("%.0f%% de las sesiones mueren en ≤%d turnos (rotación de sesiones)", share*100, shortSessionTurns),
		Impact: impactOf(prefixUSD, report.Fleet.CostUSD),
		Evidence: fmt.Sprintf("%d de %d sesiones con ≤%d turnos. Ingerir el contexto inicial de esas sesiones "+
			"costó $%.2f: cada sesión nueva vuelve a pagar el prefijo (system prompt + instrucciones) y lo reparte entre casi nada.",
			short, len(sessions), shortSessionTurns, prefixUSD),
		Recommendation: "Cuando varias preguntas cortas van al mismo tema, mándalas en una sesión en vez de una por pregunta. " +
			"Para trabajo automático recurrente, revisa el prompt de sistema: si pesa mucho, cada disparo lo paga completo.",
		SavingsUSD: prefixUSD,
		MetricName: MetricShortSessionShare,
		Metric:     share,
	}, true
}

// cronOverheadFinding flags scheduled work that costs a meaningful slice of the
// bill. Automated turns are the easiest thing in the fleet to make cheaper
// because nobody is waiting on them: they can run less often or on a smaller model.
func cronOverheadFinding(records []aggregate.Record, _ []session, report Report) (Finding, bool) {
	cron := recordsInMode(records, aggregate.ModeCron)
	if len(cron) == 0 {
		return Finding{}, false
	}
	cronEff := efficiencyOf(aggregate.ModeCron, cron, nil)
	impact := impactOf(cronEff.CostUSD, report.Fleet.CostUSD)
	if impact == ImpactLow {
		return Finding{}, false
	}

	return Finding{
		ID:     FindingCronOverhead,
		Title:  fmt.Sprintf("El trabajo programado (cron/heartbeat) lleva $%.2f del costo equivalente", cronEff.CostUSD),
		Impact: impact,
		Evidence: fmt.Sprintf("%d turnos automáticos, $%.4f por turno, %s tokens por turno — contra $%.4f por turno de toda la flota.",
			cronEff.Turns, cronEff.CostPerTurnUSD, humanInt(cronEff.TokensPerTurn), report.Fleet.CostPerTurnUSD),
		Recommendation: "Nadie espera un turno automático: es el candidato natural a bajar de frecuencia o de modelo. " +
			"Revisa el intervalo del heartbeat y si cada disparo necesita todo el contexto que está cargando.",
		MetricName: MetricCronCostShare,
		Metric:     shareOf(cronEff.CostUSD, report.Fleet.CostUSD),
	}, true
}

// unpricedModelsFinding reports turns whose model isn't in the pricing table.
// It's a data-quality finding, not an efficiency one: while those turns are
// unpriced the whole report understates the real figure, so it comes first in
// practice — you can't optimize what you're not measuring.
func unpricedModelsFinding(records []aggregate.Record, _ []session, report Report) (Finding, bool) {
	turnsByModel := map[string]int{}
	unpriced := 0
	for _, r := range records {
		if r.CostKnown || r.Confidence != aggregate.ConfidenceMeasured {
			continue
		}
		turnsByModel[r.Model]++
		unpriced++
	}
	if unpriced == 0 {
		return Finding{}, false
	}

	return Finding{
		ID:    FindingUnpricedModels,
		Title: fmt.Sprintf("%d turnos medidos usan modelos sin precio en la tabla", unpriced),
		// Data quality caps at medium: the numbers are incomplete, not wasted.
		Impact: ImpactMedium,
		Evidence: fmt.Sprintf("Modelos sin precio (turnos): %s. Sus tokens sí se cuentan, su costo equivalente no, "+
			"así que el total del reporte es un piso, no la cifra real.", modelList(turnsByModel)),
		Recommendation: "Agrega esos modelos a internal/pricing con su precio de lista (o confirma que son locales/gratis " +
			"y por eso no llevan precio). Mientras falten, el reporte subestima y las comparaciones entre agentes cojean.",
		MetricName: MetricUnpricedTurnShare,
		Metric:     shareOf(float64(unpriced), float64(countMeasured(records))),
	}, true
}

// modelMixFinding looks for expensive-model turns doing small work: turns on the
// priciest model (by cost per turn) whose output came in under the fleet median.
// No savings figure — only the operator knows whether a cheaper model would have
// been good enough, and pretending otherwise would be inventing a number.
func modelMixFinding(records []aggregate.Record, _ []session, report Report) (Finding, bool) {
	priciest, pricedTurns := priciestModel(records)
	if priciest == "" {
		return Finding{}, false
	}
	medianOutput := medianOutputTokens(records)
	if medianOutput == 0 {
		return Finding{}, false
	}

	small, smallCost := 0, 0.0
	for _, r := range pricedTurns {
		if r.Model == priciest && r.Output < medianOutput {
			small++
			smallCost += r.CostUSD
		}
	}
	impact := impactOf(smallCost, report.Fleet.CostUSD)
	if small == 0 || impact == ImpactLow {
		return Finding{}, false
	}

	return Finding{
		ID:     FindingModelMix,
		Title:  fmt.Sprintf("%d turnos del modelo más caro (%s) devolvieron menos output que la mediana de la flota", small, priciest),
		Impact: impact,
		Evidence: fmt.Sprintf("Mediana de output de la flota: %s tokens. Esos %d turnos de %s suman $%.2f de costo equivalente.",
			humanInt(medianOutput), small, priciest, smallCost),
		Recommendation: "Trabajo de output chico (clasificar, extraer, responder sí/no, formatear) suele salir igual en un modelo " +
			"más barato. Ojo: output chico NO implica tarea fácil — un buen diagnóstico también cabe en dos líneas. " +
			"Usa esto para elegir qué revisar, no para migrar en automático.",
		MetricName: MetricSmallTurnCostShare,
		Metric:     shareOf(smallCost, report.Fleet.CostUSD),
	}, true
}

// priciestModel returns the priced model with the highest cost per turn, plus the
// priced measured records it was computed from.
func priciestModel(records []aggregate.Record) (string, []aggregate.Record) {
	costByModel := map[string]float64{}
	turnsByModel := map[string]int{}
	priced := make([]aggregate.Record, 0, len(records))
	for _, r := range records {
		if r.Confidence != aggregate.ConfidenceMeasured || !r.CostKnown || r.Model == "" {
			continue
		}
		costByModel[r.Model] += r.CostUSD
		turnsByModel[r.Model]++
		priced = append(priced, r)
	}

	best, bestPerTurn := "", 0.0
	for model, cost := range costByModel {
		if perTurn := cost / float64(turnsByModel[model]); perTurn > bestPerTurn {
			best, bestPerTurn = model, perTurn
		}
	}
	return best, priced
}

// medianOutputTokens is the median output size across measured turns. Median (not
// mean) so one 30k-token dump doesn't drag the reference point up.
func medianOutputTokens(records []aggregate.Record) int {
	outputs := make([]int, 0, len(records))
	for _, r := range records {
		if r.Confidence == aggregate.ConfidenceMeasured {
			outputs = append(outputs, r.Output)
		}
	}
	if len(outputs) == 0 {
		return 0
	}
	sort.Ints(outputs)
	return outputs[len(outputs)/2]
}

// recordsInMode filters records by execution mode.
func recordsInMode(records []aggregate.Record, mode string) []aggregate.Record {
	out := make([]aggregate.Record, 0, len(records))
	for _, r := range records {
		if r.Mode == mode {
			out = append(out, r)
		}
	}
	return out
}

// modelList renders "model (N turnos)" pairs, busiest first, for evidence text.
func modelList(turnsByModel map[string]int) string {
	type pair struct {
		model string
		turns int
	}
	pairs := make([]pair, 0, len(turnsByModel))
	for model, turns := range turnsByModel {
		pairs = append(pairs, pair{model, turns})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].turns != pairs[j].turns {
			return pairs[i].turns > pairs[j].turns
		}
		return pairs[i].model < pairs[j].model
	})

	out := ""
	for i, p := range pairs {
		if i > 0 {
			out += ", "
		}
		name := p.model
		if name == "" {
			name = "(sin modelo reportado)"
		}
		out += fmt.Sprintf("%s (%d)", name, p.turns)
	}
	return out
}

// humanInt formats an int with thousands separators, matching the CLI's number
// style so the same figure reads the same everywhere.
func humanInt(n int) string { return humanize.Int(n) }
