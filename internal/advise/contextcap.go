package advise

import (
	"fmt"
	"sort"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/contextcurve"
)

// contextRiskLimit bounds how many context curves the report carries. The list
// exists to name the sessions worth cutting, not to enumerate every stream.
const contextRiskLimit = 5

// ContextRisk is one context stream ranked by what cutting it at its own
// break-even would have saved. Label and Agent come from the session so the
// reader recognizes the task; Curve is the measurement (see internal/contextcurve).
type ContextRisk struct {
	Label string                `json:"label"`
	Agent string                `json:"agent"`
	Curve contextcurve.Analysis `json:"curve"`
}

// contextRisks measures every context stream in every session and returns the
// ones that ran past their own break-even, most expensive first.
//
// The grouping is by (session, thread), not by session: one session can run a
// main thread and several subagents at once, each with its own independent
// context, and a curve computed over the interleaved turns describes none of
// them (see aggregate.Record.ThreadID).
func contextRisks(sessions []session) []ContextRisk {
	out := make([]ContextRisk, 0, len(sessions))
	for _, s := range sessions {
		for _, stream := range streamsOf(s) {
			curve := contextcurve.Analyze(stream.sessionID, stream.threadID, stream.turns)
			if !curve.Known || curve.TurnsPastNoReturn == 0 || curve.SavingsUSD <= 0 {
				continue
			}
			out = append(out, ContextRisk{Label: labelOf(s), Agent: s.agent, Curve: curve})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Curve.SavingsUSD > out[j].Curve.SavingsUSD })
	return out
}

// topContextRisks is contextRisks capped at what the report shows.
func topContextRisks(sessions []session) []ContextRisk {
	risks := contextRisks(sessions)
	if len(risks) > contextRiskLimit {
		return risks[:contextRiskLimit]
	}
	return risks
}

// stream is one session's turns for a single context thread, in time order.
type stream struct {
	sessionID string
	threadID  string
	turns     []contextcurve.Turn
}

// streamsOf splits a session's turns into one stream per context thread and
// converts them to the shape the curve measurement takes. Activity-tier records
// are dropped: they carry no token buckets, so their "context" would be a zero
// that looks like a measurement.
func streamsOf(s session) []stream {
	byThread := map[string]*stream{}
	order := make([]string, 0, 1)
	for _, r := range s.turns {
		if r.Confidence != aggregate.ConfidenceMeasured || r.SessionID == "" {
			continue
		}
		cur := byThread[r.ThreadID]
		if cur == nil {
			cur = &stream{sessionID: r.SessionID, threadID: r.ThreadID}
			byThread[r.ThreadID] = cur
			order = append(order, r.ThreadID)
		}
		cur.turns = append(cur.turns, contextcurve.Turn{
			Model:      r.Model,
			Context:    r.Input + r.CacheRead + r.CacheWrite,
			CacheRead:  r.CacheRead,
			CacheWrite: r.CacheWrite,
		})
	}

	// s.turns is already chronological (groupSessions sorts it), so appending in
	// order keeps each thread chronological too.
	out := make([]stream, 0, len(order))
	for _, threadID := range order {
		out = append(out, *byThread[threadID])
	}
	return out
}

// contextNoReturnFinding reports the sessions that kept dragging a growing
// context past the point where restarting would have been cheaper.
//
// It is the concrete, per-session version of the cache-read finding (E-01): E-01
// says the fleet's money is in cache-read, this one names WHICH conversations,
// at WHICH turn they crossed the line, and what cutting them would have saved.
// That difference is the whole point — "haz sesiones más cortas" is not
// actionable, "esta sesión pasó el punto de no-retorno hace 4,690 turnos" is.
func contextNoReturnFinding(_ []aggregate.Record, sessions []session, report Report) (Finding, bool) {
	risks := contextRisks(sessions)
	if len(risks) == 0 {
		return Finding{}, false
	}

	var savingsUSD float64
	var turnsPast int
	affectedSessions := map[string]bool{}
	for _, r := range risks {
		savingsUSD += r.Curve.SavingsUSD
		turnsPast += r.Curve.TurnsPastNoReturn
		affectedSessions[r.Curve.SessionID] = true
	}
	impact := impactOf(savingsUSD, report.Fleet.CostUSD)
	if impact == ImpactLow {
		return Finding{}, false
	}

	return Finding{
		ID: FindingContextNoReturn,
		Title: fmt.Sprintf("%d sesiones (%d hilos) arrastraron contexto más allá de su punto de no-retorno",
			len(affectedSessions), len(risks)),
		Impact: impact,
		Evidence: fmt.Sprintf("%s Entre todos suman %s turnos pasados de ese punto y $%.2f de costo equivalente "+
			"que cortar a tiempo habría evitado. %s", worstEvidence(risks[0]), humanInt(turnsPast), savingsUSD, reworkCaveat),
		Recommendation: "El punto de no-retorno es una propiedad medida de cada sesión, no un criterio: cortar ahí no " +
			"depende de que alguien lo note. Cablea el corte donde se lanza la sesión (compactar/reiniciar al llegar al " +
			"turno), o baja el tope de contexto en la config del runtime que la corre. Ver docs/automejora.md.",
		SavingsUSD: savingsUSD,
		MetricName: MetricPastNoReturnCostShare,
		Metric:     shareOf(savingsUSD, report.Fleet.CostUSD),
	}, true
}

// reworkCaveat is attached to every savings figure this finding reports. The
// arithmetic is exact — those tokens really were paid — but it prices only the
// tokens. Cutting a conversation also throws away what it knew, and an agent that
// has to re-read the files it already had costs something this measurement cannot
// see. Saying so is the difference between an upper bound and an invented number.
const reworkCaveat = "Es un tope, no una promesa: la cuenta es exacta en tokens, pero cortar también tira lo que la " +
	"sesión ya sabía, y lo que cueste volver a leerlo NO está medido aquí."

// worstEvidence spells out the priciest offender's curve: the shape it had, where
// its break-even was, and how far past it ran.
func worstEvidence(worst ContextRisk) string {
	return fmt.Sprintf("La peor (%s, %s): %s turnos, el contexto arranca en %s tokens y sube %s tokens por turno "+
		"hasta un pico de %s (mediana %s); con esa forma, reiniciar sale más barato que seguir a partir del turno %d, "+
		"y corrió %s turnos más allá — $%.2f de los $%.2f que gastó en mover contexto.",
		worst.Agent, recorta(worst.Label, 60), humanInt(worst.Curve.Turns), humanInt(worst.Curve.Baseline),
		humanInt(int(worst.Curve.GrowthPerTurn)), humanInt(worst.Curve.PeakContext), humanInt(worst.Curve.MedianContext),
		worst.Curve.NoReturnTurn, humanInt(worst.Curve.TurnsPastNoReturn),
		worst.Curve.SavingsUSD, worst.Curve.ContextCostUSD)
}

// recorta trims a label for evidence text.
func recorta(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
