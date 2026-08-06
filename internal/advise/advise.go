// Package advise is the self-improvement layer: it reads the same normalized
// records the dashboard shows and answers two questions the raw totals can't.
//
//  1. Measurement — WHERE does the equivalent cost actually go? Not "how many
//     tokens" (all tokens are not equal: a cache-read token can be ~50x cheaper
//     than an output token of the same model), but how much of the bill each
//     billable bucket, agent, mode and model carries.
//  2. Self-improvement — is the fleet getting MORE efficient over time, and what
//     concrete change would move the needle?
//
// Every number here is derived from the records; nothing is guessed. Findings
// that can't be quantified from real data carry SavingsUSD = 0 rather than an
// invented figure — same rule as docs/calibracion.md ("no se inventa ningún
// número"). The method behind each finding is documented in docs/automejora.md.
//
// Like everywhere else in this project, the $ figures are estimated-equivalent
// costs, never money charged (see internal/aggregate and internal/pricing).
package advise

import (
	"sort"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/bootfiles"
	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
	"github.com/Rentheria/llm-agent-spend-manager/internal/workload"
)

// Impact ranks a finding by how much of the fleet's equivalent cost it touches,
// so a reader fixes the expensive thing first instead of the first thing listed.
const (
	ImpactHigh   = "high"
	ImpactMedium = "medium"
	ImpactLow    = "low"
)

// Trend directions for the self-improvement metric.
const (
	TrendImproving    = "improving"
	TrendWorsening    = "worsening"
	TrendFlat         = "flat"
	TrendInsufficient = "insufficient-data"
)

// Billable bucket names, used as Finding evidence and in the dominant-bucket
// finding so the CLI, API and dashboard all name the same thing the same way.
const (
	BucketInput      = "input"
	BucketOutput     = "output"
	BucketCacheWrite = "cache-write"
	BucketCacheRead  = "cache-read"
)

// Thresholds. These are policy, not measurements, so they live here as named
// constants with their reasoning instead of as literals buried in a condition.
const (
	// impactHighShare/impactMediumShare classify a finding by the share of the
	// fleet's equivalent cost it touches.
	impactHighShare   = 0.25
	impactMediumShare = 0.10

	// dominantBucketShare is when one billable bucket carries enough of the cost
	// that it IS the optimization target and the others are noise.
	dominantBucketShare = 0.50

	// shortSessionTurns is the turn count at or below which a session never
	// amortizes the prompt prefix it had to ingest: the first turn of a session
	// pays for the whole system prompt + context, and a session that ends after
	// one or two turns spreads that cost over almost nothing.
	shortSessionTurns = 2

	// shortSessionShareFloor keeps the churn finding quiet unless short sessions
	// are a real chunk of all sessions — a couple of one-shot runs are normal.
	shortSessionShareFloor = 0.30

	// trendWindowDays is the block size for the improving/worsening comparison:
	// the mean of the last N active days vs the N before. Small enough to react
	// within a week, big enough that one heavy day doesn't flip the verdict.
	trendWindowDays = 3

	// trendFlatBandPct is the dead band (in percent) where a change in cost per
	// turn is called flat instead of a trend — day-to-day work varies.
	trendFlatBandPct = 10.0

	// recurrenceBlockDays is how many active days make ONE window for the
	// recurrence check. Same block the trend uses, so "the advice was already
	// given last window" spans the same time as "efficiency moved vs last window"
	// and the two verdicts can be read side by side.
	recurrenceBlockDays = trendWindowDays

	// recurrenceWindows is how many consecutive windows — this one included — a
	// finding must have fired in before re-emitting it stops being useful. Three
	// is the number from the lesson this came out of: the first repeat can be bad
	// luck, the second is a pattern, and by the third, writing the advice again IS
	// the error (docs/automejora.md §6).
	recurrenceWindows = 3

	// recurrenceImprovementPct is how much a finding's own metric must have
	// IMPROVED (dropped) from the oldest of those windows to the newest for the
	// advice to count as working. Anything less — flat, or worse — means the
	// advice was given and nothing moved. Same 10% dead band as the trend, for the
	// same reason: day-to-day work varies.
	recurrenceImprovementPct = 10.0
)

// fleetName labels the whole-fleet rollup wherever an agent name is expected.
const fleetName = "flota"

// BucketCost is the fleet's (or one agent's) equivalent cost attributed to each
// billable token bucket, plus the share each one represents.
type BucketCost struct {
	Bucket  string  `json:"bucket"`
	CostUSD float64 `json:"costUsd"`
	Share   float64 `json:"share"` // 0..1 of the priced equivalent cost
	Tokens  int     `json:"tokens"`
}

// AgentEfficiency is the per-agent measurement half of the report: not just what
// an agent cost, but what it costs *per turn* and how well it reuses context.
type AgentEfficiency struct {
	Agent string `json:"agent"`
	// Measured is false for activity-tier agents (Cursor, Antigravity): they
	// expose no token buckets, so every ratio below is unavailable for them and
	// the report says so instead of showing a zero that looks like a real value.
	Measured        bool    `json:"measured"`
	Turns           int     `json:"turns"`
	Sessions        int     `json:"sessions"`
	TurnsPerSession float64 `json:"turnsPerSession"`
	CostUSD         float64 `json:"costUsd"`
	CostPerTurnUSD  float64 `json:"costPerTurnUsd"`
	TokensPerTurn   int     `json:"tokensPerTurn"`
	// CacheReadShare is cache-read tokens over all context tokens read
	// (input + cache-read + cache-write): how much of the context an agent gets
	// at the discounted rate instead of paying full input price.
	CacheReadShare float64 `json:"cacheReadShare"`
	// CacheReuseRatio is cache-read over cache-write tokens: how many times, on
	// average, a written cache entry gets read back. Below 1 means the fleet
	// writes cache it never reuses — pure surcharge.
	CacheReuseRatio float64      `json:"cacheReuseRatio"`
	Buckets         []BucketCost `json:"buckets"`
}

// DayPoint is one local calendar day of fleet efficiency, the series the trend
// is computed from and the dashboard plots.
type DayPoint struct {
	Day            string  `json:"day"`
	CostUSD        float64 `json:"costUsd"`
	Turns          int     `json:"turns"`
	CostPerTurnUSD float64 `json:"costPerTurnUsd"`
	TokensPerTurn  int     `json:"tokensPerTurn"`
	CacheReadShare float64 `json:"cacheReadShare"`
}

// Trend is the self-improvement verdict: mean cost per turn over the most recent
// block of active days versus the block before it. Cost per turn (not total
// cost) is the efficiency metric on purpose — working more should raise the
// total, and that isn't a regression.
type Trend struct {
	Direction         string   `json:"direction"`
	WindowDays        int      `json:"windowDays"`
	RecentCostPerTurn float64  `json:"recentCostPerTurnUsd"`
	PriorCostPerTurn  float64  `json:"priorCostPerTurnUsd"`
	DeltaPct          float64  `json:"deltaPct"` // negative = cheaper per turn = improving
	RecentDays        []string `json:"recentDays"`
	PriorDays         []string `json:"priorDays"`
}

// Finding is one actionable observation. Evidence carries the numbers it was
// derived from so a reader can check it instead of trusting it; SavingsUSD is an
// UPPER BOUND on the equivalent cost the change could avoid, and is 0 when the
// data doesn't support a defensible figure.
type Finding struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Impact         string  `json:"impact"`
	Evidence       string  `json:"evidence"`
	Recommendation string  `json:"recommendation"`
	SavingsUSD     float64 `json:"savingsUsd"`
	// MetricName names the one number this finding's recommendation exists to
	// move, and Metric is its current value. Metric is ALWAYS a share (0..1),
	// never an absolute cost: working more raises every total, and "we worked
	// more" must never read as "the advice was ignored". This is the pair the
	// recurrence check compares between windows — see escalation.go.
	MetricName string  `json:"metricName"`
	Metric     float64 `json:"metric"`
}

// Report is the whole self-improvement payload: measurement, trend, findings.
type Report struct {
	Window      string            `json:"window"`
	GeneratedAt time.Time         `json:"generatedAt"`
	CostLabel   string            `json:"costLabel"`
	Disclaimer  string            `json:"disclaimer"`
	Fleet       AgentEfficiency   `json:"fleet"`
	ByAgent     []AgentEfficiency `json:"byAgent"`
	Daily       []DayPoint        `json:"daily"`
	Trend       Trend             `json:"trend"`
	TopTasks    []TaskCost        `json:"topTasks"`
	// ContextRisks are the conversations that dragged a growing context past the
	// turn where restarting would have been cheaper — the measured, per-session
	// answer to "when should this have been cut". Empty when none did.
	ContextRisks []ContextRisk `json:"contextRisks"`
	// Workloads is the shape of each load and what that shape cost through each
	// route that ran it (T73, Capas 2 y 3 — see internal/workload).
	Workloads workload.Report `json:"workloads"`
	// BootFiles is the weight of the shared files every agent reads at session
	// start. Unlike everything above it does not come from the usage records: it
	// is measured off the filesystem and attached by WithBootFiles, so it is the
	// zero value on a report the caller never enriched.
	BootFiles bootfiles.Report `json:"bootFiles"`
	Findings  []Finding        `json:"findings"`
	// Escalations are the findings that were NOT emitted as tips because they had
	// already been emitted for recurrenceWindows windows without moving their
	// metric. Each one names the mechanism the situation calls for instead.
	Escalations []Escalation `json:"escalations"`
}

// Analyze builds the report from already-window-filtered records. The caller
// decides the window (see aggregate.FilterWindow) and passes the location used
// for day bucketing so days line up with the rest of the CLI.
func Analyze(records []aggregate.Record, window string, loc *time.Location) Report {
	sessions := groupSessions(records)
	report := Report{
		Window:      window,
		GeneratedAt: time.Now(),
		CostLabel:   aggregate.CostLabel,
		Disclaimer:  aggregate.CostDisclaimer,
		Fleet:       efficiencyOf(fleetName, records, sessions),
		ByAgent:     efficiencyByAgent(records, sessions),
		Daily:       dailySeries(records, loc),
	}
	report.Trend = trendOf(report.Daily)
	report.TopTasks = topTasks(sessions, report.Fleet.CostUSD, loc)
	report.ContextRisks = topContextRisks(sessions)
	report.Workloads = workloadReport(sessions)
	// A finding that has been given as advice for several windows without moving
	// its own number leaves Findings entirely and comes back as an Escalation.
	// Dropping it is the point: its impact rank grows with every recurrence, so a
	// report that only re-sorted would push the advice HARDEST exactly when the
	// evidence says the advice is the wrong remedy.
	report.Findings, report.Escalations = splitEscalated(findings(records, sessions, report), records, loc)
	return report
}

// session is one agent conversation: the turns that share a session id, in
// chronological order, so the first turn (the one that pays for ingesting the
// prompt prefix) can be told apart from the rest.
type session struct {
	agent string
	turns []aggregate.Record
}

// groupSessions buckets records by (agent, session id) and sorts each bucket in
// time order. Records with no session id are skipped: activity-tier adapters
// don't expose one, and inventing a bucket for them would fake precision.
func groupSessions(records []aggregate.Record) []session {
	byKey := map[string]*session{}
	for _, r := range records {
		if r.SessionID == "" {
			continue
		}
		key := r.Agent + "\x00" + r.SessionID
		s := byKey[key]
		if s == nil {
			s = &session{agent: r.Agent}
			byKey[key] = s
		}
		s.turns = append(s.turns, r)
	}

	out := make([]session, 0, len(byKey))
	for _, s := range byKey {
		sort.Slice(s.turns, func(i, j int) bool { return s.turns[i].Timestamp.Before(s.turns[j].Timestamp) })
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].agent < out[j].agent })
	return out
}

// efficiencyByAgent computes per-agent efficiency, sorted by agent name so the
// output is stable across runs.
func efficiencyByAgent(records []aggregate.Record, sessions []session) []AgentEfficiency {
	byAgent := map[string][]aggregate.Record{}
	for _, r := range records {
		byAgent[r.Agent] = append(byAgent[r.Agent], r)
	}

	out := make([]AgentEfficiency, 0, len(byAgent))
	for agent, agentRecords := range byAgent {
		out = append(out, efficiencyOf(agent, agentRecords, sessionsOf(sessions, agent)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// sessionsOf filters sessions down to one agent's.
func sessionsOf(sessions []session, agent string) []session {
	out := make([]session, 0, len(sessions))
	for _, s := range sessions {
		if s.agent == agent {
			out = append(out, s)
		}
	}
	return out
}

// efficiencyOf rolls a set of records into the efficiency view. Bucket costs and
// cache ratios only exist for measured records; a set with no measured record is
// marked Measured=false so consumers show "n/a" instead of a misleading 0.
func efficiencyOf(name string, records []aggregate.Record, sessions []session) AgentEfficiency {
	eff := AgentEfficiency{Agent: name, Turns: len(records), Sessions: len(sessions)}
	tokens, costs := sumBuckets(records)
	measuredTurns := countMeasured(records)
	eff.Measured = measuredTurns > 0

	for _, r := range records {
		eff.TokensPerTurn += r.PointTokens()
		if r.CostKnown {
			eff.CostUSD += r.CostUSD
		}
	}
	if eff.Turns > 0 {
		eff.TokensPerTurn /= eff.Turns
		eff.CostPerTurnUSD = eff.CostUSD / float64(eff.Turns)
	}
	if eff.Sessions > 0 {
		eff.TurnsPerSession = float64(eff.Turns) / float64(eff.Sessions)
	}
	if contextTokens := tokens.Input + tokens.CacheRead + tokens.CacheWrite; contextTokens > 0 {
		eff.CacheReadShare = float64(tokens.CacheRead) / float64(contextTokens)
	}
	if tokens.CacheWrite > 0 {
		eff.CacheReuseRatio = float64(tokens.CacheRead) / float64(tokens.CacheWrite)
	}
	eff.Buckets = bucketList(tokens, costs)
	return eff
}

// countMeasured counts the token-accurate records in a set.
func countMeasured(records []aggregate.Record) int {
	n := 0
	for _, r := range records {
		if r.Confidence == aggregate.ConfidenceMeasured {
			n++
		}
	}
	return n
}

// sumBuckets adds up token counts and per-bucket costs over the records that
// have both (measured tier with a priced model). Activity-tier records and
// unpriced models contribute nothing here — their tokens are still in the
// headline totals, they just can't be attributed to a billable bucket.
func sumBuckets(records []aggregate.Record) (pricing.TokenCounts, pricing.BucketCosts) {
	var tokens pricing.TokenCounts
	var costs pricing.BucketCosts
	for _, r := range records {
		if r.Confidence != aggregate.ConfidenceMeasured {
			continue
		}
		turn := pricing.TokenCounts{Input: r.Input, Output: r.Output, CacheWrite: r.CacheWrite, CacheRead: r.CacheRead}
		turnCosts, known := pricing.EstimateByBucket(r.Model, turn)
		if !known {
			continue
		}
		tokens.Input += turn.Input
		tokens.Output += turn.Output
		tokens.CacheWrite += turn.CacheWrite
		tokens.CacheRead += turn.CacheRead
		costs.Input += turnCosts.Input
		costs.Output += turnCosts.Output
		costs.CacheWrite += turnCosts.CacheWrite
		costs.CacheRead += turnCosts.CacheRead
	}
	return tokens, costs
}

// bucketList turns the bucket sums into a share-sorted list (most expensive
// first), which is the order every surface renders them in.
func bucketList(tokens pricing.TokenCounts, costs pricing.BucketCosts) []BucketCost {
	total := costs.Total()
	out := []BucketCost{
		{Bucket: BucketInput, CostUSD: costs.Input, Tokens: tokens.Input},
		{Bucket: BucketOutput, CostUSD: costs.Output, Tokens: tokens.Output},
		{Bucket: BucketCacheWrite, CostUSD: costs.CacheWrite, Tokens: tokens.CacheWrite},
		{Bucket: BucketCacheRead, CostUSD: costs.CacheRead, Tokens: tokens.CacheRead},
	}
	for i := range out {
		if total > 0 {
			out[i].Share = out[i].CostUSD / total
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	return out
}

// dailySeries builds the per-day efficiency series, oldest day first (a trend
// reads forward in time).
func dailySeries(records []aggregate.Record, loc *time.Location) []DayPoint {
	byDay := map[string][]aggregate.Record{}
	for _, r := range records {
		day := r.Timestamp.In(loc).Format("2006-01-02")
		byDay[day] = append(byDay[day], r)
	}

	out := make([]DayPoint, 0, len(byDay))
	for day, dayRecords := range byDay {
		eff := efficiencyOf(day, dayRecords, nil)
		out = append(out, DayPoint{
			Day:            day,
			CostUSD:        eff.CostUSD,
			Turns:          eff.Turns,
			CostPerTurnUSD: eff.CostPerTurnUSD,
			TokensPerTurn:  eff.TokensPerTurn,
			CacheReadShare: eff.CacheReadShare,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// trendOf compares the mean cost per turn of the last trendWindowDays active
// days against the trendWindowDays before them. Days with no turns aren't in the
// series at all, so a quiet weekend doesn't count as an improvement.
func trendOf(daily []DayPoint) Trend {
	t := Trend{Direction: TrendInsufficient, WindowDays: trendWindowDays}
	if len(daily) < 2*trendWindowDays {
		return t
	}
	recent := daily[len(daily)-trendWindowDays:]
	prior := daily[len(daily)-2*trendWindowDays : len(daily)-trendWindowDays]

	t.RecentCostPerTurn = meanCostPerTurn(recent)
	t.PriorCostPerTurn = meanCostPerTurn(prior)
	t.RecentDays = daysOf(recent)
	t.PriorDays = daysOf(prior)
	if t.PriorCostPerTurn == 0 {
		return t
	}

	t.DeltaPct = (t.RecentCostPerTurn - t.PriorCostPerTurn) / t.PriorCostPerTurn * 100
	switch {
	case t.DeltaPct < -trendFlatBandPct:
		t.Direction = TrendImproving
	case t.DeltaPct > trendFlatBandPct:
		t.Direction = TrendWorsening
	default:
		t.Direction = TrendFlat
	}
	return t
}

// meanCostPerTurn is the aggregate cost per turn of a block of days (total cost
// over total turns, not the mean of daily ratios — a 2-turn day shouldn't weigh
// the same as a 2000-turn day).
func meanCostPerTurn(days []DayPoint) float64 {
	var cost float64
	var turns int
	for _, d := range days {
		cost += d.CostUSD
		turns += d.Turns
	}
	if turns == 0 {
		return 0
	}
	return cost / float64(turns)
}

func daysOf(days []DayPoint) []string {
	out := make([]string, 0, len(days))
	for _, d := range days {
		out = append(out, d.Day)
	}
	return out
}

// impactOf classifies a finding by the share of fleet equivalent cost it covers.
func impactOf(costTouched, fleetCost float64) string {
	if fleetCost <= 0 {
		return ImpactLow
	}
	switch share := costTouched / fleetCost; {
	case share >= impactHighShare:
		return ImpactHigh
	case share >= impactMediumShare:
		return ImpactMedium
	default:
		return ImpactLow
	}
}

// topTasksLimit bounds how many tasks the report ranks. Long enough to see the
// pattern, short enough that the answer to "what should I look at" stays short.
const topTasksLimit = 10

// TaskCost is one session ranked as a unit of work. A session is the closest
// thing to a "task" the data actually has: it's what the user opened to ask for
// something. Label is what they asked, so the report answers "which TASK cost
// this", not just "which agent" — and TopBucket answers the "why", because the
// fix for a cache-read-heavy task (shrink the context) is not the fix for an
// output-heavy one (ask for less text).
type TaskCost struct {
	Label          string  `json:"label"`
	Agent          string  `json:"agent"`
	Day            string  `json:"day"`
	CostUSD        float64 `json:"costUsd"`
	Share          float64 `json:"share"` // 0..1 del costo de la ventana
	Turns          int     `json:"turns"`
	TokensPerTurn  int     `json:"tokensPerTurn"`
	TopBucket      string  `json:"topBucket"`
	TopBucketShare float64 `json:"topBucketShare"`
}

// topTasks ranks sessions by equivalent cost, most expensive first. Sessions
// with no label still rank (an unlabelled expensive task is exactly the kind of
// thing worth noticing) — they just render as "(sin etiqueta)".
func topTasks(sessions []session, fleetCost float64, loc *time.Location) []TaskCost {
	out := make([]TaskCost, 0, len(sessions))
	for _, s := range sessions {
		if len(s.turns) == 0 {
			continue
		}
		eff := efficiencyOf(s.agent, s.turns, nil)
		task := TaskCost{
			Label:         labelOf(s),
			Agent:         s.agent,
			Day:           s.turns[0].Timestamp.In(loc).Format("2006-01-02"),
			CostUSD:       eff.CostUSD,
			Turns:         eff.Turns,
			TokensPerTurn: eff.TokensPerTurn,
		}
		if fleetCost > 0 {
			task.Share = task.CostUSD / fleetCost
		}
		if len(eff.Buckets) > 0 && eff.Buckets[0].CostUSD > 0 {
			task.TopBucket = eff.Buckets[0].Bucket
			task.TopBucketShare = eff.Buckets[0].Share
		}
		out = append(out, task)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	if len(out) > topTasksLimit {
		out = out[:topTasksLimit]
	}
	return out
}

// labelOf returns the session's label, falling back to a marker rather than an
// empty cell so the row still reads as a real (just unnamed) task.
func labelOf(s session) string {
	for _, r := range s.turns {
		if r.SessionLabel != "" {
			return r.SessionLabel
		}
	}
	return "(sin etiqueta)"
}
