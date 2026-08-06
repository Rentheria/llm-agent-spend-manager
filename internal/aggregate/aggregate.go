// Package aggregate turns the raw per-turn Entry streams from the adapters
// (claudecode, openclaw, ...) into a single normalized record type and rolls them up
// by agent and by time window (today, this week).
//
// All $ figures here are estimated-equivalent costs (real tokens × public API
// list price), NOT money charged: the covered agents run on flat-price
// subscriptions with no per-token billing. Callers must label the number
// "estimated equivalent cost", never "real spend" (see docs/architecture.md
// §3.1 and internal/pricing).
package aggregate

import (
	"sort"
	"strings"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/antigravity"
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/cursor"
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/openclaw"
)

// Agent display names, stable identifiers used across the CLI, JSON API and
// dashboard so a given agent always renders under the same label.
const (
	AgentClaudeCode  = "Claude Code"
	AgentOpenClaw    = "OpenClaw"
	AgentCursor      = "Cursor"
	AgentAntigravity = "Antigravity"
)

// Execution modes an agent's usage can be attributed to. The breakdown answers
// "where is an agent's spend going": interactive
// chat vs. scheduled cron/heartbeat runs vs. spawned work. The mode is derived
// from the OpenClaw session_key, which lives in the state DB, not in the JSONL
// transcript — so it's assigned per source at collection time (see the From*
// converters below), not read off each turn.
const (
	ModeInteractive = "interactive" // live chat / terminal session
	ModeCron        = "cron"        // scheduled runs and heartbeats
	ModeEditor      = "editor"      // in-editor coding agent (Cursor / Antigravity)
)

// Confidence tiers rank how trustworthy a record's numbers are (the "regla de
// oro" of docs/architecture.md §3.3): measured real tokens outrank
// activity-derived estimates. Agents that report real per-turn token counts
// (Claude Code, OpenClaw) are Measured; agents that expose only activity signals
// (Cursor/Antigravity) are Activity — an estimate shown as a range, never as a
// point, and never labeled real spend.
const (
	ConfidenceMeasured = "measured" // real tokens × list price (estimated-equivalent cost)
	ConfidenceActivity = "activity" // tokens/cost inferred from activity signals (A/B)
)

// CostLabel and CostDisclaimer are the mandatory wording for the $ figure on
// every surface (CLI, JSON API, dashboard). The figure is NOT money charged —
// the covered agents run on flat-price subscriptions — so it must never be
// presented as "gasto real". See docs/architecture.md §3.1.
const (
	CostLabel      = "costo equivalente estimado"
	CostDisclaimer = "Tokens reales × precio de lista pública de la API, como si se pagara por token. " +
		"NO es gasto real: los agentes cubiertos corren sobre suscripción de precio fijo. " +
		"Útil para comparar peso relativo entre agentes/tareas y como proxy de cercanía al tope de la suscripción."
)

// Record is one assistant turn, normalized across adapters and enriched with
// the self-computed estimated-equivalent cost. Adapters disagree only on field
// names (see claudecode.Entry vs openclaw.Entry); this is the shape everything
// downstream (aggregation, JSON, dashboard) consumes.
type Record struct {
	Agent     string `json:"agent"`
	Mode      string `json:"mode"`
	SessionID string `json:"sessionId"`
	// SessionLabel is what the user asked at the start of this session, verbatim
	// and truncated. It's what lets a cost report answer "which TASK cost this"
	// instead of only "which agent". Empty for adapters that expose no prompt.
	SessionLabel string `json:"sessionLabel,omitempty"`
	// ThreadID names the context stream inside the session. One session can run
	// several independent streams at once (a main thread plus spawned subagents),
	// and each carries its own context — so anything that measures how context
	// GROWS must group by (session, thread), not by session alone. Empty means the
	// session's main thread; adapters with no subagent concept always leave it so.
	ThreadID string `json:"threadId,omitempty"`
	// Workspace is the directory the session ran in. It answers a question the
	// agent name can't: a chat agent's whole conversation is ONE agent but many
	// sessions, and every code repo is a different workspace — so this
	// is what separates "the chat" from "the work" when asking where the quota
	// went. Empty for adapters that expose no working directory.
	Workspace  string    `json:"workspace,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	Model      string    `json:"model"`
	Input      int       `json:"input"`
	Output     int       `json:"output"`
	CacheWrite int       `json:"cacheWrite"`
	CacheRead  int       `json:"cacheRead"`
	// CostUSD is the estimated-equivalent cost of this turn, or 0 when the
	// model isn't in the pricing table (CostKnown false). A zero cost with
	// CostKnown true is a genuinely free/zero-token turn. For activity-tier
	// records it is the point estimate (the midpoint of the range below).
	CostUSD   float64 `json:"costUsd"`
	CostKnown bool    `json:"costKnown"`

	// Confidence is ConfidenceMeasured for token-accurate adapters (Claude Code,
	// OpenClaw)
	// and ConfidenceActivity for estimate-only adapters (Cursor, Antigravity).
	Confidence string `json:"confidence"`
	// TokensLow/TokensHigh bound the token estimate. For measured records both
	// equal the exact token count; for activity records they are the honest
	// range (A floor .. B-inflated ceiling). CostLowUSD/CostHighUSD are the same
	// bounds valued in dollars.
	TokensLow   int     `json:"tokensLow"`
	TokensHigh  int     `json:"tokensHigh"`
	CostLowUSD  float64 `json:"costLowUsd"`
	CostHighUSD float64 `json:"costHighUsd"`
}

// TotalTokens is the sum of all four token buckets for this turn. Activity-tier
// records carry no per-bucket split (only an estimate range), so this is 0 for
// them — use PointTokens for a confidence-agnostic headline figure.
func (r Record) TotalTokens() int {
	return r.Input + r.Output + r.CacheWrite + r.CacheRead
}

// PointTokens is the single headline token figure regardless of tier: the exact
// bucket sum for measured records, the midpoint of the range for activity ones.
func (r Record) PointTokens() int {
	if r.Confidence == ConfidenceActivity {
		return (r.TokensLow + r.TokensHigh) / 2
	}
	return r.TotalTokens()
}

// openClawWorkspaceSuffix identifies sessions that Claude Code ran on behalf of
// an OpenClaw agent. OpenClaw drives this same CLI, so its turns are written
// into Claude Code's transcripts exactly like a human-driven session's — and
// without this they all get billed to Claude Code. That misattribution is not
// marginal: on a machine where most of the work is dispatched through the
// gateway it accounts for the majority of the reported figure. The gateway
// always runs from its workspace, and an interactive session never does, so the
// working directory separates them cleanly.
const openClawWorkspaceSuffix = "/.openclaw/workspace"

// agentForCWD attributes a Claude Code session to whoever actually ran it.
func agentForCWD(cwd string) string {
	if strings.HasSuffix(cwd, openClawWorkspaceSuffix) {
		return AgentOpenClaw
	}
	return AgentClaudeCode
}

// FromClaudeCode converts Claude Code entries into normalized records, computing
// each one's estimated-equivalent cost via the claudecode adapter's pricing
// hook. The agent is decided per session by its working directory (see
// agentForCWD), not assumed to be Claude Code.
func FromClaudeCode(entries []claudecode.Entry) []Record {
	out := make([]Record, 0, len(entries))
	for _, e := range entries {
		cost, known := claudecode.EstimateCostUSD(e)
		rec := Record{
			Agent:        agentForCWD(e.CWD),
			Mode:         ModeInteractive,
			SessionID:    e.SessionID,
			SessionLabel: e.SessionLabel,
			ThreadID:     e.ThreadID,
			Workspace:    e.CWD,
			Timestamp:    e.Timestamp,
			Model:        e.Model,
			Input:        e.InputTokens,
			Output:       e.OutputTokens,
			CacheWrite:   e.CacheCreationInputTokens,
			CacheRead:    e.CacheReadInputTokens,
			CostUSD:      cost,
			CostKnown:    known,
		}
		out = append(out, asMeasured(rec))
	}
	return out
}

// asMeasured stamps a token-accurate record with the measured confidence tier
// and a zero-width range (low == high == the exact figure), so measured and
// activity records share one shape downstream.
func asMeasured(r Record) Record {
	r.Confidence = ConfidenceMeasured
	tokens := r.TotalTokens()
	r.TokensLow, r.TokensHigh = tokens, tokens
	r.CostLowUSD, r.CostHighUSD = r.CostUSD, r.CostUSD
	return r
}

// FromOpenClaw converts OpenClaw interactive-session entries (the JSONL scan)
// into normalized records, computing each one's estimated-equivalent cost via
// the openclaw adapter's pricing hook. Cron/heartbeat runs come from a separate
// source and converter (FromOpenClawCron); the JSONL sessions verified on this
// machine are all interactive, so they're tagged ModeInteractive.
func FromOpenClaw(entries []openclaw.Entry) []Record {
	return fromOpenClaw(entries, ModeInteractive)
}

// FromOpenClawCron converts OpenClaw cron/heartbeat entries (read from the state
// DB) into normalized records tagged ModeCron. Together with
// FromOpenClaw this closes the coverage gap where scheduled runs were invisible
// in the dashboard (docs/architecture.md §3.3).
func FromOpenClawCron(entries []openclaw.Entry) []Record {
	return fromOpenClaw(entries, ModeCron)
}

func fromOpenClaw(entries []openclaw.Entry, mode string) []Record {
	out := make([]Record, 0, len(entries))
	for _, e := range entries {
		cost, known := openclaw.EstimateCostUSD(e)
		rec := Record{
			Agent:      AgentOpenClaw,
			Mode:       mode,
			SessionID:  e.SessionID,
			Timestamp:  e.Timestamp,
			Model:      e.Model,
			Input:      e.InputTokens,
			Output:     e.OutputTokens,
			CacheWrite: e.CacheCreationInputTokens,
			CacheRead:  e.CacheReadInputTokens,
			CostUSD:    cost,
			CostKnown:  known,
		}
		out = append(out, asMeasured(rec))
	}
	return out
}

// FromCursor converts Cursor activity estimates into activity-tier records.
// Unlike the measured adapters these carry no token buckets — only a range
// (TokensLow/High, CostLow/High) — and CostUSD is the range midpoint (the
// point figure). Confidence is ConfidenceActivity so downstream never presents
// them with the authority of measured data (T12).
func FromCursor(entries []cursor.Entry) []Record {
	out := make([]Record, 0, len(entries))
	for _, e := range entries {
		costLow, costHigh, known := cursor.EstimateCostRange(e)
		out = append(out, Record{
			Agent:       AgentCursor,
			Mode:        ModeEditor,
			Confidence:  ConfidenceActivity,
			SessionID:   e.ConversationID,
			Timestamp:   e.LastActivity,
			Model:       e.Model,
			CostUSD:     (costLow + costHigh) / 2,
			CostKnown:   known,
			TokensLow:   e.TokensLow,
			TokensHigh:  e.TokensHigh,
			CostLowUSD:  costLow,
			CostHighUSD: costHigh,
		})
	}
	return out
}

// FromAntigravity converts Antigravity conversation estimates into
// activity-tier records. Confidence is ConfidenceActivity.
//
// Since T11 the adapter decodes the per-conversation model, so Claude-backed
// conversations carry a priced range. Conversations served by Antigravity's
// Gemini aliases stay CostKnown=false — those ids aren't Google API model names,
// so pricing them would be a guess — and render as tokens only.
//
// ⚠️ The dollar figure is relative weight / opportunity cost, NOT billed spend:
// Antigravity runs free under the shared subscription.
func FromAntigravity(entries []antigravity.Entry) []Record {
	out := make([]Record, 0, len(entries))
	for _, e := range entries {
		costLow, costHigh, known := antigravity.EstimateCostRange(e)
		out = append(out, Record{
			Agent:       AgentAntigravity,
			Mode:        ModeEditor,
			Confidence:  ConfidenceActivity,
			SessionID:   e.ConversationID,
			Timestamp:   e.LastActivity,
			Model:       e.Model,
			CostUSD:     (costLow + costHigh) / 2,
			CostKnown:   known,
			TokensLow:   e.TokensLow,
			TokensHigh:  e.TokensHigh,
			CostLowUSD:  costLow,
			CostHighUSD: costHigh,
		})
	}
	return out
}

// Window is a time range to aggregate over.
type Window string

const (
	WindowToday Window = "today" // since local midnight
	WindowWeek  Window = "week"  // since Monday 00:00 local (ISO week start)
	WindowAll   Window = "all"   // everything, no time filter
)

// StartOfWindow returns the inclusive lower bound for a window relative to now,
// in now's location. WindowAll returns the zero time (matches everything).
func StartOfWindow(w Window, now time.Time) time.Time {
	loc := now.Location()
	switch w {
	case WindowToday:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	case WindowWeek:
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		// Monday as the first day of the week; Go's Weekday has Sunday=0.
		offset := (int(midnight.Weekday()) + 6) % 7
		return midnight.AddDate(0, 0, -offset)
	default:
		return time.Time{}
	}
}

// FilterWindow keeps records whose timestamp falls in the window relative to
// now. WindowAll (or any unknown window) returns the records unchanged.
func FilterWindow(records []Record, w Window, now time.Time) []Record {
	start := StartOfWindow(w, now)
	if start.IsZero() {
		return records
	}
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if !r.Timestamp.Before(start) {
			out = append(out, r)
		}
	}
	return out
}

// Totals is a rolled-up sum over a set of records (per agent, per agent-day, or
// grand total). UnpricedTurns counts turns whose model wasn't in the pricing
// table, so a caller can flag that CostUSD understates the true equivalent.
type Totals struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheWrite  int `json:"cacheWrite"`
	CacheRead   int `json:"cacheRead"`
	TotalTokens int `json:"totalTokens"` // point figure (exact for measured, midpoint for activity)
	// TokensLow/TokensHigh and CostLowUSD/CostHighUSD carry the range so a caller
	// can show "rango no punto" for activity-tier data (T12).
	TokensLow   int     `json:"tokensLow"`
	TokensHigh  int     `json:"tokensHigh"`
	CostUSD     float64 `json:"costUsd"`
	CostLowUSD  float64 `json:"costLowUsd"`
	CostHighUSD float64 `json:"costHighUsd"`
	Turns       int     `json:"turns"`
	// ActivityTurns counts turns whose figures are activity estimates (not
	// measured), so a mixed bucket can be flagged as partly estimated.
	ActivityTurns int `json:"activityTurns"`
	UnpricedTurns int `json:"unpricedTurns"`
}

func (t *Totals) add(r Record) {
	t.Input += r.Input
	t.Output += r.Output
	t.CacheWrite += r.CacheWrite
	t.CacheRead += r.CacheRead
	t.TotalTokens += r.PointTokens()
	t.TokensLow += r.TokensLow
	t.TokensHigh += r.TokensHigh
	t.CostLowUSD += r.CostLowUSD
	t.CostHighUSD += r.CostHighUSD
	t.Turns++
	if r.Confidence == ConfidenceActivity {
		t.ActivityTurns++
	}
	if r.CostKnown {
		t.CostUSD += r.CostUSD
	} else {
		t.UnpricedTurns++
	}
}

// Add folds one record into a totals value and returns the result, for callers
// rolling records up along a dimension this package doesn't provide. It returns
// a new value rather than mutating so a caller can't accidentally share one
// accumulator between two buckets.
func Add(t Totals, r Record) Totals {
	t.add(r)
	return t
}

// AgentTotals pairs an agent name with its rolled-up totals.
type AgentTotals struct {
	Agent  string `json:"agent"`
	Totals Totals `json:"totals"`
}

// ByAgent sums records per agent, sorted by agent name for stable output.
func ByAgent(records []Record) []AgentTotals {
	byAgent := map[string]*Totals{}
	for _, r := range records {
		t := byAgent[r.Agent]
		if t == nil {
			t = &Totals{}
			byAgent[r.Agent] = t
		}
		t.add(r)
	}

	out := make([]AgentTotals, 0, len(byAgent))
	for agent, t := range byAgent {
		out = append(out, AgentTotals{Agent: agent, Totals: *t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// ModeTotals pairs an execution mode with its rolled-up totals.
type ModeTotals struct {
	Mode   string `json:"mode"`
	Totals Totals `json:"totals"`
}

// ByMode sums records per execution mode (interactive vs cron/heartbeat),
// sorted by mode name for stable output. This is the §6.4 breakdown that shows
// how much of an agent's usage is scheduled work versus live chat.
func ByMode(records []Record) []ModeTotals {
	byMode := map[string]*Totals{}
	for _, r := range records {
		t := byMode[r.Mode]
		if t == nil {
			t = &Totals{}
			byMode[r.Mode] = t
		}
		t.add(r)
	}

	out := make([]ModeTotals, 0, len(byMode))
	for mode, t := range byMode {
		out = append(out, ModeTotals{Mode: mode, Totals: *t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mode < out[j].Mode })
	return out
}

// ModelTotals pairs a model name with its rolled-up totals.
type ModelTotals struct {
	Model  string `json:"model"`
	Totals Totals `json:"totals"`
}

// ByModel sums records per model, heaviest first (by point tokens). The order is
// deliberate and differs from ByAgent/ByMode: this table exists to answer "which
// model is eating the quota", and that answer is the first row.
func ByModel(records []Record) []ModelTotals {
	byModel := map[string]*Totals{}
	for _, r := range records {
		t := byModel[r.Model]
		if t == nil {
			t = &Totals{}
			byModel[r.Model] = t
		}
		t.add(r)
	}

	out := make([]ModelTotals, 0, len(byModel))
	for model, t := range byModel {
		out = append(out, ModelTotals{Model: model, Totals: *t})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Totals.TotalTokens != out[j].Totals.TotalTokens {
			return out[i].Totals.TotalTokens > out[j].Totals.TotalTokens
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// WorkspaceTotals pairs a workspace (the directory a session ran in) with its
// rolled-up totals, and keeps the agent that owns it so a row can say whose
// workspace it is.
type WorkspaceTotals struct {
	Workspace string `json:"workspace"`
	Agent     string `json:"agent"`
	Totals    Totals `json:"totals"`
}

// ByWorkspace sums records per workspace, heaviest first. Records with no
// workspace (adapters that expose no working directory) are skipped rather than
// pooled into an "" bucket that would read like a real place.
func ByWorkspace(records []Record) []WorkspaceTotals {
	totals := map[string]*Totals{}
	agents := map[string]string{}
	for _, r := range records {
		if r.Workspace == "" {
			continue
		}
		t := totals[r.Workspace]
		if t == nil {
			t = &Totals{}
			totals[r.Workspace] = t
			agents[r.Workspace] = r.Agent
		}
		t.add(r)
	}

	out := make([]WorkspaceTotals, 0, len(totals))
	for workspace, t := range totals {
		out = append(out, WorkspaceTotals{Workspace: workspace, Agent: agents[workspace], Totals: *t})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Totals.TotalTokens != out[j].Totals.TotalTokens {
			return out[i].Totals.TotalTokens > out[j].Totals.TotalTokens
		}
		return out[i].Workspace < out[j].Workspace
	})
	return out
}

// AgentDayTotals is one agent's totals for one local calendar day. Day is the
// ISO date (YYYY-MM-DD) in the aggregation's location.
type AgentDayTotals struct {
	Agent  string `json:"agent"`
	Day    string `json:"day"`
	Totals Totals `json:"totals"`
}

// ByAgentDay sums records per (agent, local day), sorted by day descending then
// agent ascending — newest day first, which is what the dashboard table wants.
// The day bucket uses loc so "today" lines up with the CLI's window logic.
func ByAgentDay(records []Record, loc *time.Location) []AgentDayTotals {
	type key struct{ agent, day string }
	buckets := map[key]*Totals{}
	for _, r := range records {
		day := r.Timestamp.In(loc).Format("2006-01-02")
		k := key{r.Agent, day}
		t := buckets[k]
		if t == nil {
			t = &Totals{}
			buckets[k] = t
		}
		t.add(r)
	}

	out := make([]AgentDayTotals, 0, len(buckets))
	for k, t := range buckets {
		out = append(out, AgentDayTotals{Agent: k.agent, Day: k.day, Totals: *t})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day > out[j].Day
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// Grand sums every record into a single Totals (the whole fleet).
func Grand(records []Record) Totals {
	var t Totals
	for _, r := range records {
		t.add(r)
	}
	return t
}
