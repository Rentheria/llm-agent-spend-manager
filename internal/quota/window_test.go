package quota

import (
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

func at(hour, minute int) time.Time {
	return time.Date(2026, 7, 27, hour, minute, 0, 0, time.UTC)
}

func turnAt(t time.Time) aggregate.Record {
	return aggregate.Record{
		Agent:      aggregate.AgentClaudeCode,
		Confidence: aggregate.ConfidenceMeasured,
		Timestamp:  t,
		Output:     1000,
	}
}

func TestSessionWindows_OpensOnTheFirstTurnAndRunsFiveHours(t *testing.T) {
	records := []aggregate.Record{
		turnAt(at(7, 30)),
		turnAt(at(10, 40)),
		// Still inside the first window at 12:29; the 12:31 turn opens the next.
		turnAt(at(12, 29)),
		turnAt(at(12, 31)),
	}

	windows := SessionWindows(records, claudecode.WindowLength)
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}
	if !windows[0].Start.Equal(at(7, 30)) || !windows[0].Reset.Equal(at(12, 30)) {
		t.Errorf("first window = %s → %s, want 07:30 → 12:30", windows[0].Start, windows[0].Reset)
	}
	if windows[0].Turns != 3 {
		t.Errorf("first window turns = %d, want 3", windows[0].Turns)
	}
	if !windows[1].Start.Equal(at(12, 31)) {
		t.Errorf("second window opened at %s, want 12:31", windows[1].Start)
	}
}

func TestSessionWindows_IsOrderIndependent(t *testing.T) {
	forward := SessionWindows([]aggregate.Record{turnAt(at(7, 30)), turnAt(at(9, 0))}, claudecode.WindowLength)
	backward := SessionWindows([]aggregate.Record{turnAt(at(9, 0)), turnAt(at(7, 30))}, claudecode.WindowLength)
	if len(backward) != 1 || !backward[0].Start.Equal(forward[0].Start) {
		t.Errorf("out-of-order records rebuilt a different window: %+v vs %+v", backward, forward)
	}
}

// The reconstruction is only defensible because it predicts the reset Anthropic
// itself announced. This pins the case measured on 2026-07-27, where it landed
// exactly (see the doc comment on SessionWindows).
func TestResetDrift_MatchesTheAnnouncedReset(t *testing.T) {
	windows := SessionWindows([]aggregate.Record{turnAt(at(7, 30)), turnAt(at(10, 40))}, claudecode.WindowLength)
	event := claudecode.LimitEvent{Timestamp: at(10, 40), ResetAt: at(12, 30)}

	if drift := ResetDrift(windows[0], event); drift != 0 {
		t.Errorf("drift = %s, want 0 for the exactly-predicted window", drift)
	}
	late := claudecode.LimitEvent{Timestamp: at(10, 40), ResetAt: at(13, 16)}
	if drift := ResetDrift(windows[0], late); drift != 46*time.Minute {
		t.Errorf("drift = %s, want 46m", drift)
	}
}

func TestCurrentWindow_IsAbsentOnceTheLastOneRefilled(t *testing.T) {
	windows := SessionWindows([]aggregate.Record{turnAt(at(7, 30))}, claudecode.WindowLength)

	if _, ok := CurrentWindow(windows, at(10, 0)); !ok {
		t.Error("no current window while one is still live")
	}
	if _, ok := CurrentWindow(windows, at(12, 30)); ok {
		t.Error("reported a current window at the exact reset: it has already refilled")
	}
	if _, ok := CurrentWindow(nil, at(10, 0)); ok {
		t.Error("reported a current window with no records at all")
	}
}

func TestWindowOf_PinsAnInstantToItsWindow(t *testing.T) {
	windows := SessionWindows([]aggregate.Record{turnAt(at(7, 30)), turnAt(at(13, 0))}, claudecode.WindowLength)

	got, ok := WindowOf(windows, at(10, 40))
	if !ok || !got.Start.Equal(at(7, 30)) {
		t.Errorf("WindowOf(10:40) = %+v, %v; want the 07:30 window", got, ok)
	}
	if _, ok := WindowOf(windows, at(12, 45)); ok {
		t.Error("pinned an instant that falls in the gap between windows")
	}
}

func TestRecordsIn_IsHalfOpen(t *testing.T) {
	records := []aggregate.Record{turnAt(at(7, 30)), turnAt(at(10, 0)), turnAt(at(12, 30))}

	got := RecordsIn(records, at(7, 30), at(12, 30))
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2 (start inclusive, end exclusive)", len(got))
	}
}
