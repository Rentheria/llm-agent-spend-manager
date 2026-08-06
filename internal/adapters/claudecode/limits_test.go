package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// realRefusal is a quota refusal exactly as Claude Code wrote it on this machine
// on 2026-07-27. Keeping the verbatim shape is the point of the test: this
// parser exists only because Anthropic exposes the cap nowhere else.
const realRefusal = `{"type":"user","isApiErrorMessage":true,"error":"rate_limit",` +
	`"timestamp":"2026-07-27T16:40:12.000Z",` +
	`"message":{"role":"user","content":"You've hit your session limit · resets 12:30pm (America/Mexico_City)"}}`

func TestLimitEventFrom_ParsesTheAnnouncedReset(t *testing.T) {
	var line transcriptLine
	if err := json.Unmarshal([]byte(realRefusal), &line); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}

	event, ok := limitEventFrom(line)
	if !ok {
		t.Fatal("a rate_limit line was not recognized as a limit event")
	}
	mx, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		t.Skipf("no tzdata on this machine: %v", err)
	}
	// 16:40 UTC is 10:40 in Mexico City, so the announced 12:30pm is later the
	// same day — not the next one.
	want := time.Date(2026, 7, 27, 12, 30, 0, 0, mx)
	if !event.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %s, want %s", event.ResetAt, want)
	}
	if event.Raw == "" {
		t.Error("Raw is empty: the evidence must survive parsing so a report can quote it")
	}
}

func TestLimitEventFrom_RollsAResetPastMidnightToTheNextDay(t *testing.T) {
	mx, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		t.Skipf("no tzdata on this machine: %v", err)
	}
	refusedAt := time.Date(2026, 7, 27, 23, 10, 0, 0, mx)

	reset, ok := resolveReset("resets 1:30am (America/Mexico_City)", refusedAt)
	if !ok {
		t.Fatal("reset clock not resolved")
	}
	if want := time.Date(2026, 7, 28, 1, 30, 0, 0, mx); !reset.Equal(want) {
		t.Errorf("reset = %s, want %s (next day)", reset, want)
	}
}

func TestLimitEventFrom_IgnoresNonQuotaErrors(t *testing.T) {
	lines := []string{
		`{"isApiErrorMessage":true,"error":"overloaded","message":{"content":"resets 1:00pm (UTC)"}}`,
		`{"isApiErrorMessage":false,"error":"rate_limit","message":{"content":"resets 1:00pm (UTC)"}}`,
		// A refusal with no readable reset is unusable: without it there is no
		// window to pin the exhaustion to.
		`{"isApiErrorMessage":true,"error":"rate_limit","message":{"content":"You've hit your session limit"}}`,
	}
	for _, raw := range lines {
		var line transcriptLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("fixture does not parse: %v", err)
		}
		if _, ok := limitEventFrom(line); ok {
			t.Errorf("accepted a line it should have skipped: %s", raw)
		}
	}
}

func TestErrorText_ReadsBothContentShapes(t *testing.T) {
	if got := errorText(json.RawMessage(`"plain string"`)); got != "plain string" {
		t.Errorf("string content = %q", got)
	}
	blocks := json.RawMessage(`[{"type":"text","text":"hit your limit"},{"type":"text","text":"resets 9:00am (UTC)"}]`)
	if got := errorText(blocks); got != "hit your limit resets 9:00am (UTC)" {
		t.Errorf("block content = %q", got)
	}
}

func TestCollect_ReturnsTurnsAndLimitsFromOneScan(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "-home-user")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"assistant","sessionId":"s1","cwd":"/home/user","timestamp":"2026-07-27T16:00:00.000Z",` +
		`"message":{"model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n" + realRefusal + "\n"
	if err := os.WriteFile(filepath.Join(project, "s1.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := Collect(dir)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snapshot.Turns) != 1 {
		t.Errorf("turns = %d, want 1", len(snapshot.Turns))
	}
	if len(snapshot.SessionLimits) != 1 {
		t.Fatalf("session limits = %d, want 1", len(snapshot.SessionLimits))
	}

	// The refusal is not a turn: counting it as one would inflate the very
	// consumption figure the window is measured with.
	turns, err := CollectUsage(dir)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(turns) != len(snapshot.Turns) {
		t.Errorf("CollectUsage returned %d turns, Collect returned %d", len(turns), len(snapshot.Turns))
	}
}
