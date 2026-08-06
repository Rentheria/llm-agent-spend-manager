package claudecode

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LimitEvent is one moment the Anthropic account ran out of its rolling session
// window, exactly as Claude Code recorded it in the transcript. It is the only
// ground truth this project has about the quota: Anthropic publishes no cap, no
// counter and no remaining-budget header for subscription plans, so the single
// observable fact is "at instant T the account was already empty, and it refills
// at instant R". Everything in internal/quota is derived from these two stamps.
//
// The quota is per ACCOUNT, not per agent or per model: every agent driving the
// same account shares it, and when it runs out every model fails at once — a
// multi-model fallback chain dies together rather than degrading.
type LimitEvent struct {
	// Timestamp is when the call was refused — an upper bound on the moment the
	// window filled up.
	Timestamp time.Time
	// ResetAt is when the window refills, resolved to an absolute instant from
	// the wall clock and IANA zone the message carries.
	ResetAt time.Time
	// Raw is the message verbatim, so a report can quote the evidence instead of
	// asking the reader to trust a parsed field.
	Raw string
}

// WindowLength is the span Anthropic's session quota covers. It is the one
// number here that is not measured: it is the documented length of the rolling
// window for subscription plans. The window's PHASE is measured, not assumed —
// see internal/quota.SessionWindows.
const WindowLength = 5 * time.Hour

// rateLimitErrorCode is the stable code Claude Code stamps on a turn it could
// not run because the account is out of quota. Matching the code rather than the
// prose keeps this working when Anthropic rewords the message; the prose is only
// parsed afterwards, for the reset clock.
const rateLimitErrorCode = "rate_limit"

// resetClock pulls the refill moment out of the message, which reads e.g.
// "You've hit your session limit · resets 12:30pm (America/Mexico_City)". The
// zone is part of the message, so the instant is resolvable without assuming the
// reader's own timezone.
var resetClock = regexp.MustCompile(`resets (\d{1,2}):(\d{2})(am|pm) \(([^)]+)\)`)

// limitEventFrom builds a LimitEvent from an error line, reporting false for any
// line that is not a quota refusal or whose reset clock can't be resolved — a
// refusal with no readable reset is unusable here, since the whole point is to
// reconstruct the window it belongs to.
func limitEventFrom(line transcriptLine) (LimitEvent, bool) {
	if !line.IsAPIErrorMessage || line.Error != rateLimitErrorCode {
		return LimitEvent{}, false
	}
	text := errorText(line.Message.Content)
	resetAt, ok := resolveReset(text, line.Timestamp)
	if !ok {
		return LimitEvent{}, false
	}
	return LimitEvent{Timestamp: line.Timestamp, ResetAt: resetAt, Raw: text}, true
}

// errorText flattens the error message's content blocks into one line. Claude
// Code writes an error either as a plain string or as text blocks, same as a
// normal message.
func errorText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, " ")
}

// resolveReset turns the message's wall clock ("12:30pm", "America/Mexico_City")
// into the next such instant at or after the refusal. Anthropic reports only the
// time of day, so the date comes from the refusal itself: a reset that reads
// earlier than the refusal belongs to the following day.
func resolveReset(text string, refusedAt time.Time) (time.Time, bool) {
	m := resetClock.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(m[1])
	if err != nil || hour < 1 || hour > 12 {
		return time.Time{}, false
	}
	minute, err := strconv.Atoi(m[2])
	if err != nil || minute > 59 {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(m[4])
	if err != nil {
		return time.Time{}, false
	}

	local := refusedAt.In(loc)
	reset := time.Date(local.Year(), local.Month(), local.Day(), hourOfDay(hour, m[3]), minute, 0, 0, loc)
	if reset.Before(local) {
		reset = reset.AddDate(0, 0, 1)
	}
	return reset, true
}

// hourOfDay converts a 12-hour clock reading into a 24-hour one.
func hourOfDay(hour12 int, meridiem string) int {
	hour := hour12 % 12
	if meridiem == "pm" {
		hour += 12
	}
	return hour
}
