// Package sessionreset answers the one question the combined cap cannot: when
// does the PROVIDER's own quota window actually refill?
//
// The cap in internal/enforce runs on a window anchored to the epoch. That is
// deliberate — it is what lets every process and every backend agree on the same
// boundaries without coordinating — but it is not the phase of Anthropic's real
// 5 h session, which opens with the account's first turn at whatever minute that
// happened to be. So a 429 from the cap says nothing true about when work can
// resume: on 2026-08-06 the proxy rejected the fleet while Anthropic's own screen
// read "58% used, resets in 1h34min" (T139).
//
// internal/quota already reconstructs that real phase from every agent's turns
// and is calibrated against observed refusals. This package does not recompute
// it: it reuses SessionWindows/CurrentWindow and puts the answer somewhere the
// rejection path can reach.
//
// Reading it is expensive — one full scan of the machine measured 11.5 s wall and
// ~110 MB peak RSS on this fleet's own box (2026-08-06). That is the whole reason
// for Resolver: nothing here ever runs inside the request that needs the answer.
package sessionreset

import (
	"fmt"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/humanize"
	"github.com/Rentheria/llm-agent-spend-manager/internal/quota"
)

// Status is what is known about the provider's window. The three cases are
// mutually exclusive by construction rather than by convention: "no window is
// live" and "we could not find out" are different answers, and a pair of
// booleans would let a caller state both at once. Unknown is the zero value, so
// a State nobody has filled in cannot read as good news.
type Status int

const (
	// StatusUnknown means the phase has not been read, could not be read, or was
	// read long enough ago that the window it described has already refilled.
	StatusUnknown Status = iota
	// StatusIdle means the reading found no window in flight: the account's quota
	// already refilled, so a rejection at that moment is the self-imposed cap's
	// alone.
	StatusIdle
	// StatusLive means a window was open and Reset is when it refills.
	StatusLive
)

func (s Status) String() string {
	switch s {
	case StatusIdle:
		return "idle"
	case StatusLive:
		return "live"
	default:
		return "unknown"
	}
}

// State is one reading of the provider's window.
type State struct {
	Status Status
	// Start and Reset bound the window. They are meaningful with StatusLive, and
	// kept with StatusUnknown when the unknown came from a reading that expired —
	// the window that just refilled is still worth naming.
	Start time.Time
	Reset time.Time
	// Err is why the phase could not be read. It travels with the state instead of
	// being logged and dropped: an ETA missing because the scan failed and an ETA
	// missing because nothing was ever read are different problems.
	Err error
	// ReadAt is the instant the snapshot behind this state was taken.
	ReadAt time.Time
}

// Read derives the window state from one snapshot of the machine. It is the pure
// half of this package: same snapshot and same instant, same answer.
func Read(snapshot aggregate.Snapshot, now time.Time) State {
	windows := quota.SessionWindows(anthropicRecords(snapshot.Records), claudecode.WindowLength)
	window, ok := quota.CurrentWindow(windows, now)
	if !ok {
		return State{Status: StatusIdle, ReadAt: now}
	}
	return State{Status: StatusLive, Start: window.Start, Reset: window.Reset, ReadAt: now}
}

// anthropicRecords keeps the turns that drain the Anthropic account. Every other
// agent's turns would open windows on a quota they do not touch, which is the
// same plan boundary internal/quota already draws.
func anthropicRecords(records []aggregate.Record) []aggregate.Record {
	plan := quota.AnthropicMax{}
	out := make([]aggregate.Record, 0, len(records))
	for _, r := range records {
		if plan.Covers(r) {
			out = append(out, r)
		}
	}
	return out
}

// at returns the state as it reads at now. A live window whose reset has passed
// is no longer live, and what replaces it is not idle but unknown: the window
// refilled, and whether a new one opened since is exactly what a stale reading
// cannot say.
func (s State) at(now time.Time) State {
	if s.Status == StatusLive && !now.Before(s.Reset) {
		return State{Status: StatusUnknown, Start: s.Start, Reset: s.Reset, ReadAt: s.ReadAt}
	}
	return s
}

// needsRead reports whether this state is worth spending a full scan to replace.
//
// A live window's Reset does not move once the window is open, so re-reading it
// would burn 11.5 s to learn the same instant; it becomes worth re-reading only
// when that instant has passed. An idle reading is the one that ages: any turn
// after it opens a new window this state knows nothing about.
func (s State) needsRead(now time.Time) bool {
	switch s.Status {
	case StatusLive:
		return !now.Before(s.Reset)
	case StatusIdle:
		return now.Sub(s.ReadAt) >= idleStaleAfter
	default:
		return true
	}
}

// idleStaleAfter is how long "no window was open" is served before it is worth
// another scan. Minutes rather than seconds because the scan is not cheap, and
// because being a few minutes late to notice a window opened costs the reader
// nothing they can act on.
const idleStaleAfter = 5 * time.Minute

// clockLayout prints the wall-clock time of a reset next to the relative wait.
// The relative figure is the one that answers the question; the clock is there
// so it can be matched against the provider's own screen.
const clockLayout = "15:04"

// Note is the sentence this state contributes to a rejection, in the second
// person of everything else this tool prints. now is passed in rather than read
// from the clock so the same state always renders the same way.
func (s State) Note(now time.Time) string {
	state := s.at(now)
	switch state.Status {
	case StatusLive:
		return fmt.Sprintf("la ventana real de 5 h de Anthropic se libera en %s (~%s)",
			humanize.Duration(state.Reset.Sub(now)), state.Reset.In(time.Local).Format(clockLayout))
	case StatusIdle:
		return fmt.Sprintf("a las %s no había ventana de 5 h de Anthropic en vuelo: este 429 es del tope propio, no del plan",
			state.ReadAt.In(time.Local).Format(clockLayout))
	default:
		return state.unknownNote()
	}
}

// unknownNote says why there is no ETA. Not knowing is reported as not knowing:
// the failure this whole ticket is about was a message that sounded certain.
func (s State) unknownNote() string {
	if s.Err != nil {
		return fmt.Sprintf("no pude leer la fase real de la ventana de 5 h de Anthropic: %v", s.Err)
	}
	if !s.Reset.IsZero() {
		return fmt.Sprintf("la última ventana de 5 h de Anthropic que leímos se liberó a las %s; estoy releyendo la fase actual",
			s.Reset.In(time.Local).Format(clockLayout))
	}
	return "la fase real de la ventana de 5 h de Anthropic se está leyendo en segundo plano"
}
