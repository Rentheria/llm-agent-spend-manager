// Package contextfill measures how full each live conversation's context window
// is, and what a mid-session model change does to that figure.
//
// It exists because "cuánto se gastó" and "cuánto contexto queda" are different
// questions with different units, and only the second one predicts the failure
// that actually interrupts work: a session that fills its window gets compacted
// or refused mid-task no matter how much quota the account has left. The quota
// cycle belongs to the ACCOUNT (internal/quota); this window belongs to ONE
// conversation, and the two run out independently.
//
// The measurement is a level, not an accumulation. A session's total tokens can
// be thirty times its window — every turn re-reads the same prefix — so summing
// them says nothing about how full the window is. What says it is the context the
// LAST turn carried, which the provider itself counted (input + cache-read +
// cache-write), against the window of the model that carried it.
//
// That last clause is the whole ticket (T22). The ceiling moves with the model,
// so the same context is a different percentage depending on which model is
// active: a session at 60% of a 1M window is at 300% of a 200K one without
// writing a single token. Analyze reports that as a Shift, valuing the carried
// context against BOTH windows.
//
// Nothing here is estimated. A model whose window the local sources cannot pin
// down yields Known=false with the reason (see pricing.ContextWindowOf), because
// a fabricated ceiling would produce a percentage that reads exactly as solid as
// a measured one. Method in docs/ventana-contexto.md.
package contextfill

import (
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
)

// DefaultWarnShare is how full a window gets before the report warns, as a
// fraction. It is declared policy with a measured reason, not a guess: leaving a
// fifth of the window free has to cover one heavy turn, and over the 16,952
// turn-to-turn context increases observed on this machine (2026-07-30) the p99
// turn adds 21,947 tokens and the worst adds 79,193. On the smallest window in
// the fleet's table (200,000) a fifth is 40,000 tokens — room for the p99 turn
// with margin. A threshold at 0.95 would leave 10,000 and the warning would
// arrive after the turn that broke it, which is the failure this measurement
// exists to prevent. Configurable per run (see quota.Config).
const DefaultWarnShare = 0.80

// Status is how a stream stands against its own ceiling.
const (
	// StatusOK is below the warning threshold.
	StatusOK = "ok"
	// StatusWarning is past the threshold and below the ceiling: the point of the
	// whole measurement is that this state exists before the next one.
	StatusWarning = "warning"
	// StatusCeiling is at or past 100% of the window. It is a real, observable
	// state — the runtime is already compacting or refusing — not a projection.
	StatusCeiling = "ceiling"
	// StatusUnknown is a stream whose active model has no derivable window. It is
	// NOT "ok": nobody knows where this one stands.
	StatusUnknown = "unknown"
)

// Turn is one assistant turn of a SINGLE context stream, in chronological order.
// Turns from different streams must never be mixed: a session that spawned
// subagents runs several independent contexts at once and interleaving them
// describes none of them (see aggregate.Record.ThreadID).
type Turn struct {
	Model     string
	Timestamp time.Time
	// Context is everything the turn carried in: input + cache-read + cache-write
	// tokens. That sum is the window occupancy the provider itself counted.
	Context int
}

// Occupancy is how much of one model's context window a given amount of context
// takes up. Known is false when that model's window isn't derivable, and then
// Reason carries the motive to print where the percentage would go — never a
// zero, which would read as an empty window.
type Occupancy struct {
	Model       string  `json:"model"`
	Tokens      int     `json:"tokens"`
	Limit       int     `json:"limit,omitempty"`
	LimitSource string  `json:"limitSource,omitempty"`
	Share       float64 `json:"share,omitempty"`
	Known       bool    `json:"known"`
	Reason      string  `json:"reason,omitempty"`
}

// Shift is what a mid-session model change did to a stream's occupancy: the same
// carried context, re-scored against the new model's window.
//
// This is the shape the incident that opened T22 needs in order to be sayable:
// the tokens on both sides are identical by construction (they are the context of
// the last turn before the change), so any difference between the two shares
// comes from the ceiling moving and nothing else.
type Shift struct {
	At time.Time `json:"at"`
	// CarriedTokens is the context that crossed the change without a single new
	// token being written.
	CarriedTokens int       `json:"carriedTokens"`
	Before        Occupancy `json:"before"`
	After         Occupancy `json:"after"`
}

// Rescored reports whether both sides have a derivable window, i.e. whether the
// jump can be stated as two percentages. A shift with only one side known is
// still reported — the change of ceiling happened either way — but its magnitude
// is not a number.
func (s Shift) Rescored() bool { return s.Before.Known && s.After.Known }

// SameCeiling reports that the model changed but the ceiling did not: both models
// carry the same window, so the occupancy is untouched. It is a real and common
// case on this machine (most of the fleet's models carry ~1M), and separating it
// out is what keeps the interesting shifts visible — a table full of "16% → 16%"
// rows buries the one row where the ceiling actually moved. False when either
// window is underivable: nobody can claim a ceiling held if it can't be seen.
func (s Shift) SameCeiling() bool {
	return s.Rescored() && s.Before.Limit == s.After.Limit
}

// Stream is one context stream's standing: how full it is right now, and every
// model change that re-scored it along the way.
type Stream struct {
	SessionID string `json:"sessionId"`
	ThreadID  string `json:"threadId,omitempty"`
	Turns     int    `json:"turns"`
	// LastActivity is when the last measured turn landed, which is what makes a
	// stream "live" enough to be worth warning about.
	LastActivity time.Time `json:"lastActivity"`
	// Live is the occupancy of the last turn against its own model's window. The
	// model of that turn is the session's active model, so this is the figure that
	// changes when someone switches models without writing anything.
	Live   Occupancy `json:"live"`
	Status string    `json:"status"`
	Shifts []Shift   `json:"shifts,omitempty"`
}

// reasonNoTurns is the honest answer for a stream with nothing measurable in it.
const reasonNoTurns = "no hay turnos con contexto medible en este hilo"

// Analyze measures one context stream. turns must be chronological and belong to
// a single stream; turns that carry no context should be dropped by the caller,
// since a zero would fake both a reset and a model change. warnAt is the share of
// the window at which the status turns to a warning (see DefaultWarnShare); the
// caller validates it.
func Analyze(sessionID, threadID string, turns []Turn, warnAt float64) Stream {
	stream := Stream{
		SessionID: sessionID,
		ThreadID:  threadID,
		Turns:     len(turns),
	}
	if len(turns) == 0 {
		stream.Live = Occupancy{Reason: reasonNoTurns}
		stream.Status = StatusUnknown
		return stream
	}

	last := turns[len(turns)-1]
	stream.LastActivity = last.Timestamp
	stream.Live = OccupancyOf(last.Model, last.Context)
	stream.Status = statusOf(stream.Live, warnAt)
	stream.Shifts = shiftsIn(turns)
	return stream
}

// OccupancyOf scores an amount of carried context against a model's window.
func OccupancyOf(model string, tokens int) Occupancy {
	window := pricing.ContextWindowOf(model)
	occupancy := Occupancy{Model: model, Tokens: tokens}
	if !window.Known || window.Tokens <= 0 {
		occupancy.Reason = window.Reason
		return occupancy
	}
	occupancy.Limit = window.Tokens
	occupancy.LimitSource = window.Source
	occupancy.Share = float64(tokens) / float64(window.Tokens)
	occupancy.Known = true
	return occupancy
}

// statusOf places an occupancy against the ceiling and the warning threshold. A
// stream with no derivable window is StatusUnknown rather than StatusOK: not
// knowing where it stands is not the same as standing fine.
func statusOf(occupancy Occupancy, warnAt float64) string {
	if !occupancy.Known {
		return StatusUnknown
	}
	switch {
	case occupancy.Share >= 1:
		return StatusCeiling
	case occupancy.Share >= warnAt:
		return StatusWarning
	default:
		return StatusOK
	}
}

// shiftsIn finds every model change in the stream and re-scores the context that
// crossed it. The carried context is the one the LAST turn before the change was
// already holding, so both sides of the shift describe the same tokens — that is
// what licenses the phrase "sin escribir un solo token más".
func shiftsIn(turns []Turn) []Shift {
	var shifts []Shift
	for i := 1; i < len(turns); i++ {
		previous, current := turns[i-1], turns[i]
		if previous.Model == current.Model || previous.Model == "" || current.Model == "" {
			continue
		}
		shifts = append(shifts, Shift{
			At:            current.Timestamp,
			CarriedTokens: previous.Context,
			Before:        OccupancyOf(previous.Model, previous.Context),
			After:         OccupancyOf(current.Model, previous.Context),
		})
	}
	return shifts
}
