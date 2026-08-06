// Package quota expresses fleet usage in the unit that actually hurts.
//
// The $ figure the rest of this project computes is an estimated-equivalent
// cost, never money charged (see internal/aggregate and docs/architecture.md
// §3.1): the fleet runs on flat-price subscriptions with no per-token billing.
// What runs out is not money, it's the provider's quota — and when it runs out
// every agent stops mid-task. So the primary unit here is "how much of the
// current quota cycle is gone, and how long does the current burn rate leave".
// The $ stays available as a secondary comparison figure, under its mandatory
// label.
//
// Each provider meters quota its own way and this package models them as such,
// rather than forcing everyone into Anthropic's shape:
//
//   - Anthropic (Claude Max): a rolling 5-hour session window plus a weekly cap.
//     The ceiling is unpublished, so it is CALIBRATED from the exhaustions
//     observed on this machine (calibrate.go) and carried as a range with its
//     dispersion — never as a fabricated point.
//   - Cursor: a monthly plan with an allowance denominated in dollars. Here the
//     $ is real money and the percentage is exact.
//   - Antigravity: no quota signal exists at all, so it gets no cycle and the
//     report says so instead of showing a hopeful zero.
package quota

import (
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// Unit is what a provider denominates its quota in. It decides how a turn is
// converted into quota spent, and how a figure is rendered.
const (
	// UnitTokens is Anthropic's case. Anthropic does not publish how it weighs a
	// turn against a subscription cap, so tokens are what is actually observable
	// — see calibrate.go for why no per-model weighting is applied.
	UnitTokens = "tokens"
	// UnitUSD is a plan whose allowance is published in dollars (Cursor). Unlike
	// everywhere else in this project, that $ IS money: it's what the plan buys.
	UnitUSD = "usd"
)

// Capacity sources, so a reader can weigh a ceiling by where it came from.
const (
	// SourcePlan is a figure the provider publishes.
	SourcePlan = "plan publicado"
	// SourceCalibrated is a figure derived from quota exhaustions observed on
	// this machine. It always travels with its dispersion.
	SourceCalibrated = "estimado calibrado"
)

// Measure is one figure that may be exact or bracketed. Nothing in this package
// returns a bare float for a quantity that isn't exactly known: a calibrated
// ceiling and an activity-tier consumption are both honestly ranges, and
// collapsing either to a point is the exact failure mode this project exists to
// avoid.
type Measure struct {
	Low   float64 `json:"low"`
	Point float64 `json:"point"`
	High  float64 `json:"high"`
	// Exact is true when Low == Point == High because the figure was measured,
	// not estimated.
	Exact bool `json:"exact"`
}

// ExactMeasure builds a measure for a figure that was actually counted.
func ExactMeasure(v float64) Measure {
	return Measure{Low: v, Point: v, High: v, Exact: true}
}

// RangeMeasure builds a bracketed measure whose point figure is the midpoint.
func RangeMeasure(low, high float64) Measure {
	return Measure{Low: low, Point: (low + high) / 2, High: high}
}

// Capacity is how much one quota cycle holds. Known is false when the provider
// publishes nothing and nothing has been observed either — the honest state for
// a cap that has simply never been hit.
type Capacity struct {
	Limit  Measure `json:"limit"`
	Known  bool    `json:"known"`
	Source string  `json:"source,omitempty"`
	// Dispersion is the coefficient of variation of the observations behind a
	// calibrated limit: 0.40 means the observed ceilings spread ±40% around their
	// own mean. It is what stops a calibrated number from being read as precise.
	Dispersion float64 `json:"dispersion,omitempty"`
}

// Cycle is one quota accounting period in flight: what it meters, when it
// opened, when it refills, how much it holds and how much is gone.
type Cycle struct {
	Provider string `json:"provider"`
	Plan     string `json:"plan"`
	// Name is what to call this cycle to a human ("ventana de 5 h").
	Name     string    `json:"name"`
	Unit     string    `json:"unit"`
	Start    time.Time `json:"start"`
	Reset    time.Time `json:"reset"`
	Capacity Capacity  `json:"capacity"`
	Used     Measure   `json:"used"`
	Turns    int       `json:"turns"`
	// Unmeasured counts turns inside the cycle whose consumption in this unit
	// could not be read at all — a Cursor conversation on a model with no price,
	// for instance. Used understates the cycle by exactly these turns, so a
	// surface that hides the count turns "we cannot see it" into "there is none".
	Unmeasured int `json:"unmeasured"`
}

// Fill is the share of the cycle's capacity already consumed. The bounds are
// deliberately crossed — the worst case is the most consumption against the
// smallest ceiling — so the high end is the one to plan against. ok is false
// when the capacity isn't known, in which case there is no percentage to show
// and the caller must say that rather than print 0%.
func (c Cycle) Fill() (Measure, bool) {
	if !c.Capacity.Known || c.Capacity.Limit.Point <= 0 {
		return Measure{}, false
	}
	fill := Measure{
		Low:   share(c.Used.Low, c.Capacity.Limit.High),
		Point: share(c.Used.Point, c.Capacity.Limit.Point),
		High:  share(c.Used.High, c.Capacity.Limit.Low),
		Exact: c.Used.Exact && c.Capacity.Limit.Exact,
	}
	return fill, true
}

// Remaining is how much capacity is left before the cycle runs out, floored at
// zero. ok is false when the capacity isn't known.
func (c Cycle) Remaining() (Measure, bool) {
	if !c.Capacity.Known {
		return Measure{}, false
	}
	return Measure{
		Low:   atLeastZero(c.Capacity.Limit.Low - c.Used.High),
		Point: atLeastZero(c.Capacity.Limit.Point - c.Used.Point),
		High:  atLeastZero(c.Capacity.Limit.High - c.Used.Low),
		Exact: c.Used.Exact && c.Capacity.Limit.Exact,
	}, true
}

// Burn is the recent consumption rate: what the cycle is being drained at right
// now, which is what decides how long is left — not the average since it opened.
type Burn struct {
	PerHour float64       `json:"perHour"`
	Over    time.Duration `json:"over"`
	Turns   int           `json:"turns"`
}

// Forecast is the answer to "how much time do I have left". Exhausts is false
// when the current burn rate does not empty the cycle before it refills, which
// is the healthy case and deserves to be said in those words.
type Forecast struct {
	Known      bool      `json:"known"`
	Exhausts   bool      `json:"exhausts"`
	ExhaustsAt time.Time `json:"exhaustsAt,omitempty"`
	// TimeLeft is the wait until exhaustion when Exhausts, and until the reset
	// otherwise — in both cases "how long until the situation changes".
	TimeLeft time.Duration `json:"timeLeft"`
}

// Project turns a cycle plus its recent burn rate into a forecast. It uses the
// pessimistic end of both ranges (most consumed, smallest ceiling) because the
// cost of being wrong is asymmetric: an agent that stops mid-task costs more
// than one that finishes with quota to spare.
func Project(c Cycle, burn Burn, now time.Time) Forecast {
	untilReset := c.Reset.Sub(now)
	remaining, known := c.Remaining()
	if !known || burn.PerHour <= 0 {
		return Forecast{Known: false, TimeLeft: untilReset}
	}

	hoursLeft := remaining.Low / burn.PerHour
	exhaustsIn := time.Duration(hoursLeft * float64(time.Hour))
	if exhaustsIn >= untilReset {
		return Forecast{Known: true, Exhausts: false, TimeLeft: untilReset}
	}
	return Forecast{
		Known:      true,
		Exhausts:   true,
		ExhaustsAt: now.Add(exhaustsIn),
		TimeLeft:   exhaustsIn,
	}
}

// BurnRate measures how fast the cycle is draining over the trailing lookback,
// which is the horizon that predicts the next hour — a rate averaged over the
// whole cycle would still be reporting this morning's calm at 3pm.
func BurnRate(records []aggregate.Record, unit string, lookback time.Duration, now time.Time) Burn {
	since := now.Add(-lookback)
	recent := make([]aggregate.Record, 0, len(records))
	for _, r := range records {
		if r.Timestamp.After(since) {
			recent = append(recent, r)
		}
	}
	if len(recent) == 0 {
		return Burn{Over: lookback}
	}
	used := Used(recent, unit)
	return Burn{
		PerHour: used.Point / lookback.Hours(),
		Over:    lookback,
		Turns:   len(recent),
	}
}

// Used totals what a set of turns consumed in the given unit. Activity-tier
// records (Cursor) carry a range rather than a count, so the total is a range
// too whenever any of them is estimated.
func Used(records []aggregate.Record, unit string) Measure {
	total := Measure{Exact: true}
	for _, r := range records {
		total = addMeasure(total, consumed(r, unit))
	}
	return total
}

// Unmeasured counts the turns that contribute nothing to Used because their
// consumption in this unit is unreadable, not because they consumed nothing.
func Unmeasured(records []aggregate.Record, unit string) int {
	blind := 0
	for _, r := range records {
		if m := consumed(r, unit); m.High == 0 && m.Point == 0 {
			blind++
		}
	}
	return blind
}

// consumed is one turn's contribution to a quota cycle, in the cycle's unit.
func consumed(r aggregate.Record, unit string) Measure {
	if unit == UnitUSD {
		if !r.CostKnown {
			return Measure{Exact: true}
		}
		if r.Confidence == aggregate.ConfidenceActivity {
			return RangeMeasure(r.CostLowUSD, r.CostHighUSD)
		}
		return ExactMeasure(r.CostUSD)
	}
	if r.Confidence == aggregate.ConfidenceActivity {
		return RangeMeasure(float64(r.TokensLow), float64(r.TokensHigh))
	}
	return ExactMeasure(float64(r.TotalTokens()))
}

func addMeasure(a, b Measure) Measure {
	return Measure{
		Low:   a.Low + b.Low,
		Point: a.Point + b.Point,
		High:  a.High + b.High,
		Exact: a.Exact && b.Exact,
	}
}

func share(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part / whole
}

func atLeastZero(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}
