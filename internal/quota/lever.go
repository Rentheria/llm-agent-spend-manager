package quota

import (
	"fmt"
	"sort"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// sessionWindowLength is the span of the Anthropic cycle every quota figure in
// this package is measured against.
const sessionWindowLength = claudecode.WindowLength

// Lever ids. Stable, so a commit or a chat message can refer to "P-01" across
// runs — same convention as the findings in internal/advise.
const (
	LeverConcentration = "P-01"
	LeverTokensPerTurn = "P-02"
	LeverModelMix      = "P-03"
)

// Lever thresholds. Policy, not measurement.
const (
	// concentrationShare is when one workspace carries enough of the quota that
	// it IS the answer to "who is eating it" and everything else is a rounding
	// error. Half the quota in one place is not a distribution, it's a target.
	concentrationShare = 0.40

	// heavyTurnRatio is how many times the fleet's median tokens-per-turn a
	// workspace must run at before its context growth is the problem rather than
	// its volume. At 2x, the same number of turns costs twice the quota.
	heavyTurnRatio = 2.0

	// minTurnsForLever keeps a lever quiet unless there is enough work behind it
	// to be worth changing anything. A workspace with nine heavy turns is an
	// anecdote.
	minTurnsForLever = 20

	// expensiveModelShare is when one model carries enough of the quota that
	// routing part of its work elsewhere is the biggest single change available.
	expensiveModelShare = 0.40
)

// Lever is a change that would stretch the quota, with the size of the effect
// derived from what was actually measured. Nothing here extrapolates beyond the
// observed data: a counterfactual is always capped at a target the fleet has
// already demonstrated it can run at (an uncapped counterfactual once claimed
// many times the savings it could actually show).
type Lever struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Evidence is the measurement, stated so the reader can check the claim
	// instead of trusting it.
	Evidence string `json:"evidence"`
	// Action is what to actually change. A report that stops at the diagnosis
	// makes the reader do the last and hardest step.
	Action string `json:"action"`
	// QuotaShare is how much of the period's consumption this lever touches.
	QuotaShare float64 `json:"quotaShare"`
	// TokensSaved is the consumption the lever would avoid, and WindowExtension
	// is what that buys in wall-clock at the current burn rate — the unit the
	// reader actually feels when an agent stops mid-task.
	TokensSaved     float64       `json:"tokensSaved"`
	WindowExtension time.Duration `json:"windowExtension"`
}

// levers runs every rule and keeps the ones that fired, biggest effect first. A
// rule that finds nothing stays silent rather than padding the report.
func levers(breakdown Breakdown, cycles []CycleStatus) []Lever {
	burn := sessionBurn(cycles)
	rules := []func(Breakdown, Burn) (Lever, bool){
		concentrationLever,
		tokensPerTurnLever,
		modelMixLever,
	}

	var out []Lever
	for _, rule := range rules {
		if lever, ok := rule(breakdown, burn); ok {
			out = append(out, lever)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QuotaShare > out[j].QuotaShare })
	return out
}

// sessionBurn picks the rate the levers are valued against: the one draining the
// cycle that actually stops agents mid-task.
func sessionBurn(cycles []CycleStatus) Burn {
	for _, c := range cycles {
		if c.Cycle.Name == CycleSessionWindow {
			return c.Burn
		}
	}
	return Burn{}
}

// concentrationLever fires when one workspace carries most of the quota. This is
// the lever that has to shout: a fleet can be convinced it is spending its quota
// on code while most of it goes somewhere else entirely.
func concentrationLever(b Breakdown, burn Burn) (Lever, bool) {
	if len(b.ByWorkspace) == 0 || b.Total.TotalTokens == 0 {
		return Lever{}, false
	}
	top := b.ByWorkspace[0]
	share := float64(top.Totals.TotalTokens) / float64(b.Total.TotalTokens)
	if share < concentrationShare || top.Totals.Turns < minTurnsForLever {
		return Lever{}, false
	}

	perTurn := tokensPerTurn(top.Totals)
	median := medianTokensPerTurn(b.ByWorkspace)
	return Lever{
		ID:         LeverConcentration,
		Title:      fmt.Sprintf("%s se lleva %.0f%% de la cuota", WorkspaceName(top.Workspace), share*100),
		QuotaShare: share,
		Evidence: fmt.Sprintf("%s · %s de %s tokens en %s turnos, a %s tokens/turno (la mediana de los espacios es %s)",
			top.Agent, millions(float64(top.Totals.TotalTokens)), millions(float64(b.Total.TotalTokens)),
			count(top.Totals.Turns), count(perTurn), count(median)),
		Action: fmt.Sprintf("Aquí es donde se decide la ventana, no en los repos de código. "+
			"Si %s no es trabajo de código, la palanca es cortar el contexto que arrastra: "+
			"sesiones más cortas, resumen en vez de historial completo, o mover ese tráfico a un modelo más ligero.",
			WorkspaceName(top.Workspace)),
	}, true
}

// tokensPerTurnLever fires on the compounding: a workspace that pays several
// times the fleet's median for every single turn. Volume is a choice; this is
// the tax on every turn regardless of volume.
func tokensPerTurnLever(b Breakdown, burn Burn) (Lever, bool) {
	median := medianTokensPerTurn(b.ByWorkspace)
	if median <= 0 || b.Total.TotalTokens == 0 {
		return Lever{}, false
	}

	worst, ratio := heaviestWorkspace(b.ByWorkspace, median)
	if ratio < heavyTurnRatio {
		return Lever{}, false
	}

	perTurn := tokensPerTurn(worst.Totals)
	// Counterfactual capped at a rate the fleet already runs at: bringing this
	// workspace down to the MEDIAN of the others, never to zero and never to an
	// invented target.
	saved := float64((perTurn - median) * worst.Totals.Turns)
	return Lever{
		ID:              LeverTokensPerTurn,
		Title:           fmt.Sprintf("%s paga %.1fx la mediana en cada turno", WorkspaceName(worst.Workspace), ratio),
		QuotaShare:      float64(worst.Totals.TotalTokens) / float64(b.Total.TotalTokens),
		TokensSaved:     saved,
		WindowExtension: extension(saved, burn),
		Evidence: fmt.Sprintf("%s tokens/turno contra %s de mediana, en %s turnos: %s tokens de más solo por el tamaño del contexto",
			count(perTurn), count(median), count(worst.Totals.Turns), millions(saved)),
		Action: fmt.Sprintf("Bajar %s a la mediana de la flota (%s tokens/turno) libera %s de cuota. "+
			"El contexto es lo que compone: cada turno vuelve a pagar todo lo acumulado.",
			WorkspaceName(worst.Workspace), count(median), millions(saved)),
	}, true
}

// modelMixLever fires when one model carries most of the quota AND another model
// is observed to do its turns for fewer tokens. Both halves matter: "the biggest
// model has the biggest share" alone is not a lever, and the second-biggest model
// by volume is not necessarily the cheaper one — on this fleet it has been the
// more expensive one per turn.
//
// It deliberately attaches no saving figure. Two things stand between the
// measurement and a number: Anthropic does not publish how it weighs one model
// against the plan and this machine's exhaustions were not enough to derive it
// (calibrate.go), and the per-turn gap partly reflects WHICH work gets routed to
// each model rather than the model itself. Either one alone would make the
// number a guess.
func modelMixLever(b Breakdown, burn Burn) (Lever, bool) {
	if len(b.ByModel) < 2 || b.Total.TotalTokens == 0 {
		return Lever{}, false
	}
	top := b.ByModel[0]
	share := float64(top.Totals.TotalTokens) / float64(b.Total.TotalTokens)
	if share < expensiveModelShare {
		return Lever{}, false
	}
	lighter, ok := lightestModel(b.ByModel, top)
	if !ok || tokensPerTurn(lighter.Totals) >= tokensPerTurn(top.Totals) {
		return Lever{}, false
	}

	return Lever{
		ID:         LeverModelMix,
		Title:      fmt.Sprintf("%s concentra %.0f%% de la cuota y no es el más barato por turno", top.Model, share*100),
		QuotaShare: share,
		Evidence: fmt.Sprintf("%s: %s tokens en %s turnos (%s tokens/turno). %s hace turnos por %s tokens, %.0f%% menos, sobre %s turnos observados",
			top.Model, millions(float64(top.Totals.TotalTokens)), count(top.Totals.Turns), count(tokensPerTurn(top.Totals)),
			lighter.Model, count(tokensPerTurn(lighter.Totals)),
			(1-float64(tokensPerTurn(lighter.Totals))/float64(tokensPerTurn(top.Totals)))*100,
			count(lighter.Totals.Turns)),
		Action: fmt.Sprintf("Ruta el trabajo rutinario a %s. Cuánta cuota ahorra no se puede afirmar y no se "+
			"inventa aquí: Anthropic no publica cómo pondera cada modelo contra el plan (ver la nota de "+
			"calibración), y parte de la diferencia por turno es el tipo de trabajo que recibe cada modelo, "+
			"no el modelo.", lighter.Model),
	}, true
}

// comparableTurnShare is how much of the dominant model's workload a candidate
// must already carry before its per-turn rate can be compared against it. A
// model used for 48 trivial turns will always look cheap; that says nothing
// about what it would cost doing the other model's work.
const comparableTurnShare = 0.10

// lightestModel is the model that does its turns for the fewest tokens, among
// those already carrying a comparable share of the work. It is measured, not
// assumed from a price list: which model is cheaper per turn on this fleet
// depends on what gets routed to it.
func lightestModel(models []aggregate.ModelTotals, dominant aggregate.ModelTotals) (aggregate.ModelTotals, bool) {
	floor := int(float64(dominant.Totals.Turns) * comparableTurnShare)
	if floor < minTurnsForLever {
		floor = minTurnsForLever
	}

	var lightest aggregate.ModelTotals
	found := false
	for _, m := range models {
		if m.Model == dominant.Model || m.Model == "" || m.Totals.Turns < floor {
			continue
		}
		if !found || tokensPerTurn(m.Totals) < tokensPerTurn(lightest.Totals) {
			lightest, found = m, true
		}
	}
	return lightest, found
}

// heaviestWorkspace returns the workspace furthest above the median cost per
// turn, among those with enough turns to matter.
func heaviestWorkspace(workspaces []aggregate.WorkspaceTotals, median int) (aggregate.WorkspaceTotals, float64) {
	var worst aggregate.WorkspaceTotals
	var ratio float64
	for _, w := range workspaces {
		if w.Totals.Turns < minTurnsForLever {
			continue
		}
		if r := float64(tokensPerTurn(w.Totals)) / float64(median); r > ratio {
			worst, ratio = w, r
		}
	}
	return worst, ratio
}

// medianTokensPerTurn is the fleet's typical per-turn weight, over workspaces
// with enough turns to be typical of anything. It is the reference every
// counterfactual is capped at, so it must be an observed rate, not a target.
func medianTokensPerTurn(workspaces []aggregate.WorkspaceTotals) int {
	rates := make([]float64, 0, len(workspaces))
	for _, w := range workspaces {
		if w.Totals.Turns >= minTurnsForLever {
			rates = append(rates, float64(tokensPerTurn(w.Totals)))
		}
	}
	if len(rates) == 0 {
		return 0
	}
	sort.Float64s(rates)
	return int(median(rates))
}

func tokensPerTurn(t aggregate.Totals) int {
	if t.Turns == 0 {
		return 0
	}
	return t.TotalTokens / t.Turns
}

// extension converts saved consumption into the wall-clock it buys at the
// current drain rate. Zero when there is no measured rate to convert with.
func extension(tokensSaved float64, burn Burn) time.Duration {
	if burn.PerHour <= 0 || tokensSaved <= 0 {
		return 0
	}
	return time.Duration(tokensSaved / burn.PerHour * float64(time.Hour))
}

func sortByTokens(sessions []SessionUsage) {
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Totals.TotalTokens != sessions[j].Totals.TotalTokens {
			return sessions[i].Totals.TotalTokens > sessions[j].Totals.TotalTokens
		}
		return sessions[i].SessionID < sessions[j].SessionID
	})
}
