package quota

import (
	"sort"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// Report is this tool's answer to the three questions that matter when the
// quota — not the money — is what runs out:
//
//  1. How much of the window is gone and how long is left at this pace (Cycles).
//  2. How much context each live conversation has left before it hits its own
//     ceiling (ContextWindows) — a different window from the account's quota,
//     which runs out separately and stops the work just as hard (T22).
//  3. Who is eating it, by agent × model × workspace × session (Breakdown).
//  4. Which lever stretches it the most (Levers).
//
// The estimated-equivalent $ is still here, in Breakdown, as a secondary
// comparison figure under its mandatory label — it is not the headline anymore.
type Report struct {
	GeneratedAt    time.Time      `json:"generatedAt"`
	Cycles         []CycleStatus  `json:"cycles"`
	ContextWindows ContextWindows `json:"contextWindows"`
	Breakdown      Breakdown      `json:"breakdown"`
	Levers         []Lever        `json:"levers"`
	Calibration    Calibration    `json:"calibration"`
	// Unmetered names the agents no plan can meter and why, so an agent missing
	// from the cycle table doesn't read as an agent consuming nothing.
	Unmetered map[string]string `json:"unmetered"`
}

// CycleStatus is one quota cycle with everything derived from it: how full, how
// fast it's draining, and when it runs out at that rate.
type CycleStatus struct {
	Cycle     Cycle    `json:"cycle"`
	Fill      Measure  `json:"fill"`
	FillKnown bool     `json:"fillKnown"`
	Burn      Burn     `json:"burn"`
	Forecast  Forecast `json:"forecast"`
}

// Breakdown is the "who is eating it" view over the analysis period. Every table
// is sorted heaviest-first: the answer is meant to be the first row, not
// something to hunt for.
type Breakdown struct {
	Since       time.Time                   `json:"since"`
	Total       aggregate.Totals            `json:"total"`
	ByAgent     []aggregate.AgentTotals     `json:"byAgent"`
	ByModel     []aggregate.ModelTotals     `json:"byModel"`
	ByWorkspace []aggregate.WorkspaceTotals `json:"byWorkspace"`
	TopSessions []SessionUsage              `json:"topSessions"`
}

// SessionUsage is one conversation's share of the period, labeled with what was
// actually asked so a heavy row can be recognized instead of just identified.
type SessionUsage struct {
	SessionID string           `json:"sessionId"`
	Label     string           `json:"label"`
	Agent     string           `json:"agent"`
	Workspace string           `json:"workspace"`
	Totals    aggregate.Totals `json:"totals"`
}

// burnLookback is the horizon the drain rate is measured over. Short enough to
// reflect what the fleet is doing right now (a rate averaged over the whole
// window would still be reporting the morning's calm at 3pm), long enough that
// one heavy turn doesn't spike the forecast.
const burnLookback = 30 * time.Minute

// Analyze builds the whole report from one snapshot of the machine. records is
// every turn read; since bounds the breakdown period; now is the instant the
// cycles are evaluated at.
func Analyze(snapshot aggregate.Snapshot, cfg Config, since, now time.Time) Report {
	calibration := Calibrate(snapshot.Records, snapshot.SessionLimits, sessionWindowLength)
	period := RecordsIn(snapshot.Records, since, now)
	breakdown := breakdownOf(period, since)

	report := Report{
		GeneratedAt:    now,
		Cycles:         cycleStatuses(cfg.Plans(calibration), snapshot.Records, now),
		ContextWindows: contextWindowsOf(snapshot.Records, since, cfg.ContextWarnShare),
		Breakdown:      breakdown,
		Calibration:    calibration,
		Unmetered:      UnmeteredAgents,
	}
	report.Levers = levers(breakdown, report.Cycles)
	return report
}

// cycleStatuses evaluates every plan's in-flight cycles against the turns that
// plan covers.
func cycleStatuses(plans []Plan, records []aggregate.Record, now time.Time) []CycleStatus {
	var out []CycleStatus
	for _, plan := range plans {
		covered := filter(records, plan.Covers)
		for _, cycle := range plan.Cycles(records, now) {
			burn := BurnRate(covered, cycle.Unit, burnLookback, now)
			fill, fillKnown := cycle.Fill()
			out = append(out, CycleStatus{
				Cycle:     cycle,
				Fill:      fill,
				FillKnown: fillKnown,
				Burn:      burn,
				Forecast:  Project(cycle, burn, now),
			})
		}
	}
	return out
}

// topSessionCount bounds the session table. Beyond a handful the tail is noise,
// and the point of the table is the top of it.
const topSessionCount = 8

func breakdownOf(records []aggregate.Record, since time.Time) Breakdown {
	return Breakdown{
		Since:       since,
		Total:       aggregate.Grand(records),
		ByAgent:     heaviestAgentsFirst(aggregate.ByAgent(records)),
		ByModel:     aggregate.ByModel(records),
		ByWorkspace: aggregate.ByWorkspace(records),
		TopSessions: topSessions(records, topSessionCount),
	}
}

// heaviestAgentsFirst reorders the per-agent rollup, which aggregate sorts by
// name for stable output. Here the question is "who is eating the quota", so the
// answer belongs in the first row.
func heaviestAgentsFirst(agents []aggregate.AgentTotals) []aggregate.AgentTotals {
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Totals.TotalTokens != agents[j].Totals.TotalTokens {
			return agents[i].Totals.TotalTokens > agents[j].Totals.TotalTokens
		}
		return agents[i].Agent < agents[j].Agent
	})
	return agents
}

// topSessions rolls turns up per conversation and keeps the heaviest.
func topSessions(records []aggregate.Record, limit int) []SessionUsage {
	index := map[string]*SessionUsage{}
	order := make([]string, 0, len(records))
	for _, r := range records {
		usage := index[r.SessionID]
		if usage == nil {
			usage = &SessionUsage{
				SessionID: r.SessionID,
				Label:     r.SessionLabel,
				Agent:     r.Agent,
				Workspace: r.Workspace,
			}
			index[r.SessionID] = usage
			order = append(order, r.SessionID)
		}
		if usage.Label == "" {
			usage.Label = r.SessionLabel
		}
		usage.Totals = aggregate.Add(usage.Totals, r)
	}

	all := make([]SessionUsage, 0, len(order))
	for _, id := range order {
		all = append(all, *index[id])
	}
	sortByTokens(all)
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}
