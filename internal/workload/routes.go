package workload

import (
	"fmt"
	"sort"
)

// Capa 3 — the savings plan per route.
//
// The fleet runs similar work through different routes (Claude Code, OpenClaw,
// Cursor, Antigravity). That is an asset almost
// nobody has: a MEASURED counterfactual. "This shape of load cost $X through
// route A and $Y through route B" is not an opinion, it's two observations put
// side by side.
//
// Three rules keep it honest, and they are the whole reason this file is longer
// than the arithmetic in it:
//
//  1. Only routes that actually ran this shape take part. A route with no
//     observation for a class is reported as missing data — never interpolated
//     from what it did elsewhere.
//  2. Only measured routes take part. Cursor and Antigravity are activity
//     estimates (a range, "≈"), so putting their figure next to a measured one as
//     if the two carried the same weight would be the exact error the confidence
//     tiers exist to prevent (docs/architecture.md §3.3).
//  3. Same shape is not the same deliverable. The arithmetic is exact on tokens,
//     but nothing here verifies that route A's output would have been acceptable
//     coming from route B. So every figure is an upper bound with that said out
//     loud, not a promise.

// Counterfactual dimensions: the two ways the same measured turns can be
// re-read. They are alternative readings of the SAME turns, never additive.
const (
	DimensionRoute = "ruta"
	DimensionModel = "modelo"
)

// Reasons a counterfactual can't be computed, and reasons a route is absent from
// a class. User-facing wording — the report prints them where the number would
// have gone.
const (
	ReasonSingleObservation = "solo se midió una opción de %s para esta forma de carga: no hay con qué compararla. " +
		"Falta el dato de las demás; no se interpola."
	ReasonNoObservation = "ninguna ruta medida corrió esta forma en esta ventana"

	MissingNoShapeInClass = "corrió en esta ventana, pero ninguna de sus cargas tuvo esta forma"
	MissingActivityOnly   = "solo expone actividad estimada (rango, ≈): no se puede medir la forma de sus cargas, " +
		"así que no entra a la comparación"
	MissingNoActivity = "sin actividad observada en esta ventana"
)

// EquivalenceCaveat is attached to every savings figure this package produces.
// It is the difference between an upper bound and an invented number, and it
// says the one thing the arithmetic cannot check.
const EquivalenceCaveat = "La misma FORMA de carga no es el mismo ENTREGABLE: la cuenta es exacta en tokens observados, " +
	"pero nada aquí verifica que la ruta barata hubiera entregado lo mismo. Es un tope y una hipótesis a probar, " +
	"no una orden de mover el trabajo."

// zeroCostExclusion explains why a route or model whose equivalent cost is zero
// is left out of the comparison instead of winning it. A locally-run model costs
// $0 because it runs on owned hardware, not because it is more efficient;
// letting it be "the cheapest" would produce a 100% savings figure that means
// nothing (see pricing.IsFreeLocal).
const zeroCostExclusion = "costo equivalente cero (modelo local / sin precio): no es un contrafactual de costo"

// ModelCost is what one model cost inside one route for one workload class.
type ModelCost struct {
	Model          string  `json:"model"`
	Streams        int     `json:"streams"`
	Turns          int     `json:"turns"`
	CostUSD        float64 `json:"costUsd"`
	CostPerTurnUSD float64 `json:"costPerTurnUsd"`
}

// RouteCost is what one route cost for one workload class. Measured is false for
// activity-tier routes; those never reach here today (they can't be classified
// at all) but the flag travels with the figure so no surface can lose it.
type RouteCost struct {
	Route          string      `json:"route"`
	Measured       bool        `json:"measured"`
	Streams        int         `json:"streams"`
	Turns          int         `json:"turns"`
	CostUSD        float64     `json:"costUsd"`
	CostPerTurnUSD float64     `json:"costPerTurnUsd"`
	ByModel        []ModelCost `json:"byModel"`
}

// MissingRoute is a fleet route with no comparable figure for a class, and why.
// This is the "se dice que falta el dato" half of the plan, and it is a first
// class part of the output rather than an omission.
type MissingRoute struct {
	Route  string `json:"route"`
	Reason string `json:"reason"`
}

// Counterfactual is one measured comparison: the cheapest observed option along
// a dimension, and what the turns that went the other way would have cost there.
type Counterfactual struct {
	Dimension              string  `json:"dimension"`
	Cheapest               string  `json:"cheapest,omitempty"`
	CheapestCostPerTurnUSD float64 `json:"cheapestCostPerTurnUsd"`
	// CheapestTurns is how many turns of this shape were actually observed going
	// through the cheapest option. It is the ceiling on what can be claimed — see
	// MovableTurns.
	CheapestTurns int `json:"cheapestTurns"`
	// TurnsElsewhere is how many observed turns went through something other than
	// the cheapest option, and MovableTurns is how many of those the observation
	// actually supports moving: min(TurnsElsewhere, CheapestTurns).
	//
	// The cap is the whole difference between deriving and predicting. Saying
	// "45,000 turns should have gone to the cheap model" when the cheap model was
	// only ever observed doing 900 of them is an extrapolation of 50x dressed up
	// as arithmetic. Capped at what was observed, the claim stays inside the data:
	// "we watched this option handle N turns of this exact shape, and N turns that
	// went elsewhere cost this much more".
	TurnsElsewhere int `json:"turnsElsewhere"`
	MovableTurns   int `json:"movableTurns"`
	// SavingsUSD prices MovableTurns at the gap between the cheapest option's own
	// measured cost per turn and the measured cost per turn of everything else.
	// Both sides are observed figures.
	SavingsUSD float64 `json:"savingsUsd"`
	// Excluded lists options left out for having zero equivalent cost.
	Excluded []string `json:"excluded,omitempty"`
	// Known is false when there was nothing to compare; Reason says so.
	Known  bool   `json:"known"`
	Reason string `json:"reason,omitempty"`
}

// Capped reports whether the claim was limited by how little the cheapest option
// was observed doing, which is worth saying out loud next to the figure.
func (c Counterfactual) Capped() bool { return c.MovableTurns < c.TurnsElsewhere }

// ClassPlan is one workload shape: how much of the bill it carries, which lever
// applies to it, what each route charged for it, and which routes we have no
// data for.
type ClassPlan struct {
	Class     string  `json:"class"`
	Lever     string  `json:"lever"`
	Streams   int     `json:"streams"`
	Turns     int     `json:"turns"`
	CostUSD   float64 `json:"costUsd"`
	CostShare float64 `json:"costShare"`

	Routes  []RouteCost    `json:"routes"`
	Missing []MissingRoute `json:"missing"`

	ByRoute Counterfactual `json:"byRoute"`
	ByModel Counterfactual `json:"byModel"`
}

// ReasonCount is how many streams stayed unclassified for one reason.
type ReasonCount struct {
	Reason  string  `json:"reason"`
	Streams int     `json:"streams"`
	CostUSD float64 `json:"costUsd"`
}

// Unclassified is the part of the load whose shape could not be established.
// It is reported, not hidden: a report that silently dropped it would overstate
// how much of the fleet it understands.
type Unclassified struct {
	Streams   int           `json:"streams"`
	Turns     int           `json:"turns"`
	CostUSD   float64       `json:"costUsd"`
	CostShare float64       `json:"costShare"`
	Reasons   []ReasonCount `json:"reasons"`
}

// Report is Capa 2 and Capa 3 together: every stream's shape, and what each
// shape cost through each route that ran it.
type Report struct {
	Streams      int          `json:"streams"`
	Classified   int          `json:"classified"`
	CostUSD      float64      `json:"costUsd"`
	Classes      []ClassPlan  `json:"classes"`
	Unclassified Unclassified `json:"unclassified"`
	Caveat       string       `json:"caveat"`
}

// classOrder fixes the order the shapes are reported in — the same order the
// design doc lists them in (docs/workload-classes.md) — so two runs never
// shuffle the reader's mental model and the report can be read next to the doc.
var classOrder = []string{ClassLongConversation, ClassMechanicalBurst, ClassBigContext, ClassOneShot}

// Analyze classifies every stream and builds the per-route plan for each shape.
// knownRoutes is the universe of fleet routes the plan is allowed to report as
// missing — passing it in (instead of reading it off the data) is what lets the
// report say "this route has no data" rather than quietly omitting it.
func Analyze(shapes []Shape, knownRoutes []string) Report {
	byClass := map[string][]Shape{}
	var totalCost float64
	for _, s := range shapes {
		class := Classify(s).Class
		byClass[class] = append(byClass[class], s)
		totalCost += s.CostUSD
	}

	report := Report{Streams: len(shapes), CostUSD: totalCost, Caveat: EquivalenceCaveat}
	for _, class := range classOrder {
		members := byClass[class]
		report.Classified += len(members)
		report.Classes = append(report.Classes, classPlanOf(class, members, shapes, knownRoutes, totalCost))
	}
	report.Unclassified = unclassifiedOf(byClass[ClassUnclassified], totalCost)
	return report
}

// classPlanOf builds one shape's plan: its weight, its routes, its missing
// routes and the two counterfactuals.
func classPlanOf(class string, members, allShapes []Shape, knownRoutes []string, totalCost float64) ClassPlan {
	plan := ClassPlan{Class: class, Lever: leverByClass[class], Streams: len(members)}
	for _, s := range members {
		plan.Turns += s.Turns
		plan.CostUSD += s.CostUSD
	}
	plan.CostShare = shareOf(plan.CostUSD, totalCost)
	plan.Routes = routeCosts(members)
	plan.Missing = missingRoutes(plan.Routes, allShapes, knownRoutes)
	plan.ByRoute = counterfactualOf(DimensionRoute, routeBuckets(plan.Routes))
	plan.ByModel = counterfactualOf(DimensionModel, modelBuckets(members))
	return plan
}

// unclassifiedOf rolls up the streams that matched no shape, grouped by why.
func unclassifiedOf(members []Shape, totalCost float64) Unclassified {
	out := Unclassified{Streams: len(members)}
	byReason := map[string]*ReasonCount{}
	for _, s := range members {
		out.Turns += s.Turns
		out.CostUSD += s.CostUSD
		reason := Classify(s).Reason
		rc := byReason[reason]
		if rc == nil {
			rc = &ReasonCount{Reason: reason}
			byReason[reason] = rc
		}
		rc.Streams++
		rc.CostUSD += s.CostUSD
	}
	out.CostShare = shareOf(out.CostUSD, totalCost)

	for _, rc := range byReason {
		out.Reasons = append(out.Reasons, *rc)
	}
	sort.Slice(out.Reasons, func(i, j int) bool {
		if out.Reasons[i].Streams != out.Reasons[j].Streams {
			return out.Reasons[i].Streams > out.Reasons[j].Streams
		}
		return out.Reasons[i].Reason < out.Reasons[j].Reason
	})
	return out
}

// routeCosts sums one class's streams per route, most expensive route first.
func routeCosts(members []Shape) []RouteCost {
	byRoute := map[string]*RouteCost{}
	models := map[string]map[string]*ModelCost{}
	for _, s := range members {
		rc := byRoute[s.Agent]
		if rc == nil {
			rc = &RouteCost{Route: s.Agent, Measured: true}
			byRoute[s.Agent] = rc
			models[s.Agent] = map[string]*ModelCost{}
		}
		rc.Measured = rc.Measured && s.Measured
		rc.Streams++
		rc.Turns += s.Turns
		rc.CostUSD += s.CostUSD
		addModel(models[s.Agent], s)
	}

	out := make([]RouteCost, 0, len(byRoute))
	for route, rc := range byRoute {
		rc.CostPerTurnUSD = perTurn(rc.CostUSD, rc.Turns)
		rc.ByModel = sortedModels(models[route])
		out = append(out, *rc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		return out[i].Route < out[j].Route
	})
	return out
}

func addModel(models map[string]*ModelCost, s Shape) {
	mc := models[s.Model]
	if mc == nil {
		mc = &ModelCost{Model: s.Model}
		models[s.Model] = mc
	}
	mc.Streams++
	mc.Turns += s.Turns
	mc.CostUSD += s.CostUSD
}

func sortedModels(models map[string]*ModelCost) []ModelCost {
	out := make([]ModelCost, 0, len(models))
	for _, mc := range models {
		mc.CostPerTurnUSD = perTurn(mc.CostUSD, mc.Turns)
		out = append(out, *mc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// missingRoutes names every known fleet route with no figure for this class, and
// why it has none. A route absent from the plan without an explanation reads as
// "it wasn't relevant"; with one, it reads as "we don't know", which is true.
func missingRoutes(present []RouteCost, allShapes []Shape, knownRoutes []string) []MissingRoute {
	inClass := map[string]bool{}
	for _, rc := range present {
		inClass[rc.Route] = true
	}
	anyShape, anyMeasured := map[string]bool{}, map[string]bool{}
	for _, s := range allShapes {
		anyShape[s.Agent] = true
		if s.Measured {
			anyMeasured[s.Agent] = true
		}
	}

	out := make([]MissingRoute, 0, len(knownRoutes))
	for _, route := range knownRoutes {
		if inClass[route] {
			continue
		}
		out = append(out, MissingRoute{Route: route, Reason: missingReason(route, anyShape, anyMeasured)})
	}
	return out
}

func missingReason(route string, anyShape, anyMeasured map[string]bool) string {
	switch {
	case !anyShape[route]:
		return MissingNoActivity
	case !anyMeasured[route]:
		return MissingActivityOnly
	default:
		return MissingNoShapeInClass
	}
}

// bucket is one comparable option along a counterfactual dimension.
type bucket struct {
	key      string
	turns    int
	costUSD  float64
	measured bool
}

func routeBuckets(routes []RouteCost) []bucket {
	out := make([]bucket, 0, len(routes))
	for _, rc := range routes {
		out = append(out, bucket{key: rc.Route, turns: rc.Turns, costUSD: rc.CostUSD, measured: rc.Measured})
	}
	return out
}

func modelBuckets(members []Shape) []bucket {
	byModel := map[string]*bucket{}
	for _, s := range members {
		b := byModel[s.Model]
		if b == nil {
			b = &bucket{key: s.Model, measured: true}
			byModel[s.Model] = b
		}
		b.measured = b.measured && s.Measured
		b.turns += s.Turns
		b.costUSD += s.CostUSD
	}
	out := make([]bucket, 0, len(byModel))
	for _, b := range byModel {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// counterfactualOf picks the cheapest measured option per turn and prices the
// turns that went elsewhere at that rate. Both sides of the subtraction are
// observed figures — nothing is modelled.
//
// Options with zero equivalent cost are excluded rather than allowed to win: see
// zeroCostExclusion.
func counterfactualOf(dimension string, buckets []bucket) Counterfactual {
	cf := Counterfactual{Dimension: dimension}
	eligible := make([]bucket, 0, len(buckets))
	for _, b := range buckets {
		switch {
		case !b.measured || b.turns <= 0:
			continue
		case b.costUSD <= 0:
			cf.Excluded = append(cf.Excluded, labelOrUnknown(b.key))
		default:
			eligible = append(eligible, b)
		}
	}
	sort.Strings(cf.Excluded)

	if len(eligible) < 2 {
		cf.Reason = reasonForMissingComparison(dimension, len(eligible))
		return cf
	}

	cheapest := eligible[0]
	for _, b := range eligible[1:] {
		if perTurn(b.costUSD, b.turns) < perTurn(cheapest.costUSD, cheapest.turns) {
			cheapest = b
		}
	}
	cf.Cheapest = labelOrUnknown(cheapest.key)
	cf.CheapestCostPerTurnUSD = perTurn(cheapest.costUSD, cheapest.turns)
	cf.CheapestTurns = cheapest.turns

	var elsewhereCost float64
	for _, b := range eligible {
		if b.key == cheapest.key {
			continue
		}
		cf.TurnsElsewhere += b.turns
		elsewhereCost += b.costUSD
	}
	cf.MovableTurns = min(cf.TurnsElsewhere, cf.CheapestTurns)
	gapPerTurn := perTurn(elsewhereCost, cf.TurnsElsewhere) - cf.CheapestCostPerTurnUSD
	cf.SavingsUSD = gapPerTurn * float64(cf.MovableTurns)
	cf.Known = true
	return cf
}

// reasonForMissingComparison words the two ways a comparison can be impossible:
// nothing observed at all, or a single observation with nothing to sit next to.
func reasonForMissingComparison(dimension string, eligible int) string {
	if eligible == 0 {
		return ReasonNoObservation
	}
	return fmt.Sprintf(ReasonSingleObservation, dimension)
}

func labelOrUnknown(key string) string {
	if key == "" {
		return "(sin modelo reportado)"
	}
	return key
}

func perTurn(cost float64, turns int) float64 {
	if turns <= 0 {
		return 0
	}
	return cost / float64(turns)
}

func shareOf(part, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return part / total
}
