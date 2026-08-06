package quota

import (
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// Plan is how one provider meters its quota. Each provider gets its own
// implementation on purpose: their quotas are not the same object wearing
// different numbers. Anthropic's is an unpublished ceiling over a rolling
// 5-hour window that has to be inferred from refusals; Cursor's is a dollar
// allowance over a billing month that the provider states up front. Flattening
// both into one shape would mean lying about one of them.
type Plan interface {
	Provider() string
	// Covers reports whether a turn is charged against this plan's quota.
	Covers(r aggregate.Record) bool
	// Cycles are the accounting periods in flight at now, already filled with
	// what the given turns consumed. Callers pass every turn; the plan filters.
	Cycles(records []aggregate.Record, now time.Time) []Cycle
}

// Cycle names, so the CLI, the JSON API and the docs call the same period the
// same thing.
const (
	CycleSessionWindow = "ventana de 5 h"
	CycleWeeklyCap     = "tope semanal del plan"
	CycleBillingMonth  = "mes del plan"
)

// AnthropicMax models a Claude Max subscription: a rolling session window plus a
// weekly cap, both metered by a formula Anthropic does not publish. The session
// window's ceiling is calibrated from observed exhaustions; the weekly one has
// never been hit on this machine, so it has no ceiling to report and says so.
type AnthropicMax struct {
	// Tier is the plan label as the provider names it ("Max 5x"), used for
	// display only — nothing here is derived from it, because Anthropic
	// publishes no per-tier quota figure to derive anything from.
	Tier string
	// Calibration carries the session-window ceiling and its error bars.
	Calibration Calibration
}

func (p AnthropicMax) Provider() string { return "Anthropic (Claude Max)" }

// Covers is by agent, not by model: the quota belongs to the ACCOUNT, so every
// agent driving the Claude CLI on it drains the same window — an interactive
// Claude Code session and an OpenClaw agent both do.
func (p AnthropicMax) Covers(r aggregate.Record) bool {
	return r.Agent == aggregate.AgentClaudeCode || r.Agent == aggregate.AgentOpenClaw
}

func (p AnthropicMax) Cycles(records []aggregate.Record, now time.Time) []Cycle {
	covered := filter(records, p.Covers)
	var cycles []Cycle
	if window, ok := p.sessionCycle(covered, now); ok {
		cycles = append(cycles, window)
	}
	return append(cycles, p.weeklyCycle(covered, now))
}

// sessionCycle is the window in flight right now. ok is false when the last
// window already refilled: with nothing consumed there is no percentage to show,
// and reporting 0% of a window that hasn't opened would be noise.
func (p AnthropicMax) sessionCycle(records []aggregate.Record, now time.Time) (Cycle, bool) {
	windows := SessionWindows(records, claudecode.WindowLength)
	window, ok := CurrentWindow(windows, now)
	if !ok {
		return Cycle{}, false
	}
	inWindow := RecordsIn(records, window.Start, window.Reset)
	return Cycle{
		Provider:   p.Provider(),
		Plan:       p.Tier,
		Name:       CycleSessionWindow,
		Unit:       UnitTokens,
		Start:      window.Start,
		Reset:      window.Reset,
		Capacity:   p.Calibration.Capacity,
		Used:       Used(inWindow, UnitTokens),
		Turns:      len(inWindow),
		Unmeasured: Unmeasured(inWindow, UnitTokens),
	}, true
}

// weeklyCycle reports the week's consumption with no ceiling attached. Two
// things are unknown and neither is guessed: the weekly allowance (Anthropic
// publishes none) and the anchor the week resets on (never observed, since the
// weekly cap has never been hit here). The ISO week is used as the reporting
// convention and labeled as such — the consumption and its pace are real even
// when the ceiling isn't.
func (p AnthropicMax) weeklyCycle(records []aggregate.Record, now time.Time) Cycle {
	start := aggregate.StartOfWindow(aggregate.WindowWeek, now)
	inWeek := RecordsIn(records, start, now)
	return Cycle{
		Provider:   p.Provider(),
		Plan:       p.Tier,
		Name:       CycleWeeklyCap,
		Unit:       UnitTokens,
		Start:      start,
		Reset:      start.AddDate(0, 0, 7),
		Used:       Used(inWeek, UnitTokens),
		Turns:      len(inWeek),
		Unmeasured: Unmeasured(inWeek, UnitTokens),
	}
}

// CursorPlan models a Cursor subscription: one billing month with an allowance
// denominated in dollars. This is the one plan in the fleet whose $ is money —
// the allowance is what the plan buys — so its percentage is a real percentage,
// not an equivalence. Consumption is still activity-tier (Cursor records no
// tokens), so it arrives as a range and stays one.
type CursorPlan struct {
	// MonthlyUSD is the plan's allowance. It is configuration, not a constant:
	// plans change price and different users are on different ones (see Config).
	MonthlyUSD float64
	// RenewalDay is the day of the month the allowance refills on.
	RenewalDay int
}

func (p CursorPlan) Provider() string { return "Cursor" }

func (p CursorPlan) Covers(r aggregate.Record) bool { return r.Agent == aggregate.AgentCursor }

func (p CursorPlan) Cycles(records []aggregate.Record, now time.Time) []Cycle {
	start, reset := billingMonth(now, p.RenewalDay)
	inMonth := RecordsIn(filter(records, p.Covers), start, reset)
	return []Cycle{{
		Provider:   p.Provider(),
		Plan:       "plan mensual",
		Name:       CycleBillingMonth,
		Unit:       UnitUSD,
		Start:      start,
		Reset:      reset,
		Capacity:   Capacity{Known: true, Source: SourcePlan, Limit: ExactMeasure(p.MonthlyUSD)},
		Used:       Used(inMonth, UnitUSD),
		Turns:      len(inMonth),
		Unmeasured: Unmeasured(inMonth, UnitUSD),
	}}
}

// billingMonth returns the current billing period's bounds for a plan that
// renews on renewalDay each month.
func billingMonth(now time.Time, renewalDay int) (start, reset time.Time) {
	loc := now.Location()
	start = time.Date(now.Year(), now.Month(), renewalDay, 0, 0, 0, 0, loc)
	if start.After(now) {
		start = start.AddDate(0, -1, 0)
	}
	return start, start.AddDate(0, 1, 0)
}

func filter(records []aggregate.Record, keep func(aggregate.Record) bool) []aggregate.Record {
	out := make([]aggregate.Record, 0, len(records))
	for _, r := range records {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// UnmeteredAgents are the agents no plan can meter, with the reason. They are
// listed in the report rather than omitted: an agent missing from a quota table
// otherwise reads as "consumes nothing" when the truth is "nobody can tell".
var UnmeteredAgents = map[string]string{
	aggregate.AgentAntigravity: "no expone tokens ni cuota; solo actividad (bloqueo del lado de Google, ver architecture.md §4.2)",
}
