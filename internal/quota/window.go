package quota

import (
	"sort"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// SessionWindow is one rolling Anthropic quota window: it opens with the first
// turn sent while no window was live, and refills exactly WindowLength later.
type SessionWindow struct {
	Start time.Time `json:"start"`
	Reset time.Time `json:"reset"`
	Turns int       `json:"turns"`
}

// Contains reports whether an instant falls inside this window.
func (w SessionWindow) Contains(t time.Time) bool {
	return !t.Before(w.Start) && t.Before(w.Reset)
}

// SessionWindows reconstructs the account's quota windows from the turn stream.
//
// Anthropic exposes no window boundary anywhere — no header, no counter, no API.
// What it does expose, on refusal, is the reset clock ("resets 12:30pm"), and
// that is enough to CHECK the reconstruction against reality. Rebuilding the
// windows as "the first turn with no live window opens a new one, which lasts 5
// hours" and comparing the predicted resets against the ones actually observed
// on this machine (measured 2026-07-28 over 5 exhaustions, timestamps in UTC):
// 6 s off on 2026-07-27, then 1.1 min on 2026-06-26, 3.8 min on 2026-07-04 and
// 7.8 min on 2026-06-28. The remaining observation (2026-07-17) landed 45.7
// minutes off, which is why the calibration treats every window as an
// observation with error rather than as a fact.
//
// The turns of every agent on the account go into the same window: the quota is
// per account, and Claude Code and OpenClaw share it.
func SessionWindows(records []aggregate.Record, length time.Duration) []SessionWindow {
	stamps := make([]time.Time, 0, len(records))
	for _, r := range records {
		if !r.Timestamp.IsZero() {
			stamps = append(stamps, r.Timestamp)
		}
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })

	var windows []SessionWindow
	for _, t := range stamps {
		if n := len(windows); n > 0 && windows[n-1].Contains(t) {
			windows[n-1].Turns++
			continue
		}
		windows = append(windows, SessionWindow{Start: t, Reset: t.Add(length), Turns: 1})
	}
	return windows
}

// CurrentWindow returns the window in flight at now. ok is false when the last
// window already refilled, meaning the account is starting fresh: there is no
// consumption to report and the next turn opens a new window.
func CurrentWindow(windows []SessionWindow, now time.Time) (SessionWindow, bool) {
	if len(windows) == 0 {
		return SessionWindow{}, false
	}
	last := windows[len(windows)-1]
	if !last.Contains(now) {
		return SessionWindow{}, false
	}
	return last, true
}

// WindowOf finds the window an instant belongs to. It is what pins an observed
// exhaustion to the consumption that caused it.
func WindowOf(windows []SessionWindow, t time.Time) (SessionWindow, bool) {
	for _, w := range windows {
		if w.Contains(t) {
			return w, true
		}
	}
	return SessionWindow{}, false
}

// RecordsIn keeps the turns that fall inside a half-open time range.
func RecordsIn(records []aggregate.Record, start, end time.Time) []aggregate.Record {
	out := make([]aggregate.Record, 0, len(records))
	for _, r := range records {
		if !r.Timestamp.Before(start) && r.Timestamp.Before(end) {
			out = append(out, r)
		}
	}
	return out
}

// ResetDrift is how far a reconstructed window's reset lands from the reset the
// provider actually announced in a refusal. It is the reconstruction's own error
// bar, reported instead of assumed away.
func ResetDrift(window SessionWindow, event claudecode.LimitEvent) time.Duration {
	drift := window.Reset.Sub(event.ResetAt)
	if drift < 0 {
		return -drift
	}
	return drift
}
