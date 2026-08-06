// Package openclaw reads OpenClaw's local session logs to extract real
// per-message token usage, without depending on OpenClaw internals beyond its
// on-disk JSONL format (~/.openclaw/agents/<agentId>/sessions/*.jsonl).
//
// The format differs from Claude Code's (see package claudecode): each line is an
// event {type, id, parentId, timestamp, message}; a model turn is a line with
// type "message" and message.role "assistant", carrying message.usage with
// short token keys {input, output, cacheRead, cacheWrite}. There is no
// per-line session id — the file name (a UUID) is the session id.
//
// Three OpenClaw specifics are handled here:
//   - usage.cost is always 0 (it runs on subscription/CLI models with no
//     per-token billing), so cost must be recomputed from tokens via the
//     shared pricing table (see pricing.go), never read from the log.
//   - model "delivery-mirror" is an internal delivery echo, not a real LLM
//     call (always zero usage); it is skipped.
//   - the SAME turn is written to several transcripts, so the scan has to
//     deduplicate by the event id or it counts one call many times (H-01, see
//     dedupeRepeatedTurns).
package openclaw

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxEntriesPerFile caps how many usage entries a single session transcript can
// contribute, bounding memory against a pathologically large or corrupt file
// (L-02). It's a var, not a const, so tests can lower it.
var maxEntriesPerFile = 500_000

// deliveryMirrorModel is OpenClaw's internal delivery-echo pseudo-model; its
// lines are not real LLM calls and always carry zero usage.
const deliveryMirrorModel = "delivery-mirror"

// gatewayFallbackPrefix names the transcripts OpenClaw writes when a turn had to
// leave through its fallback gateway. Such a file holds a side-record of a call
// that also landed in the real conversation's transcript, so when both record
// the same turn the real conversation is the better owner (see ownsTurn).
const gatewayFallbackPrefix = "gateway-fallback-"

// Entry is one assistant turn with real token usage, as recorded by OpenClaw.
// It mirrors claudecode.Entry so aggregation (T5) can treat both adapters uniformly.
type Entry struct {
	SessionID                string
	Timestamp                time.Time
	Model                    string
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// usageTurn is one parsed assistant turn plus the event id OpenClaw stamped on
// it. The id is what identifies the same call across the several transcripts
// that record it; Entry deliberately doesn't carry it, because by the time
// CollectUsage returns every turn appears exactly once and the id has no
// meaning left downstream.
type usageTurn struct {
	entry   Entry
	eventID string
}

// sessionLine mirrors only the fields this package needs. OpenClaw's JSONL has
// many more (content, api, provider, stopReason, ...); json.Unmarshal ignores
// anything not listed here, so this stays forward-compatible with additions.
//
// ID is the event id, and it sits at the TOP level of the line — not inside
// message. (H-01 was first written up as "message.id"; there is no such field.)
type sessionLine struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Role  string `json:"role"`
		Model string `json:"model"`
		Usage struct {
			Input      int `json:"input"`
			Output     int `json:"output"`
			CacheRead  int `json:"cacheRead"`
			CacheWrite int `json:"cacheWrite"`
		} `json:"usage"`
	} `json:"message"`
}

// AgentsDir returns the default OpenClaw agents directory
// (~/.openclaw/agents), given a home directory.
func AgentsDir(homeDir string) string {
	return filepath.Join(homeDir, ".openclaw", "agents")
}

// CollectUsage walks every active session transcript under agentsDir
// (<agentId>/sessions/*.jsonl) and returns one Entry per assistant turn that
// reports token usage.
//
// Only active *.jsonl files are scanned. Deliberately excluded:
//   - *.trajectory.jsonl — runtime event traces, carry no usage; they share
//     the .jsonl suffix so they must be filtered by name, not extension.
//   - *.jsonl.reset.<date> / *.jsonl.deleted.<date> — sessions the user reset
//     or deleted. Their content is rotated out of the active file (the base
//     UUID never appears as both active and archived), so including them would
//     count spend the user chose to clear and diverges from the claudecode adapter's
//     active-only scan. Revisit if a full historical ledger is ever needed.
//
// The same turn is recorded in several of those files, so the scan ends by
// deduplicating on the event id (see dedupeRepeatedTurns) — without it, one
// call is counted once per transcript that happens to hold it.
//
// Transcripts or lines that fail to parse are skipped rather than aborting the
// whole scan — a single malformed/partial line (e.g. a session still being
// written to) should not hide every other session's data.
func CollectUsage(agentsDir string) ([]Entry, error) {
	var files []string

	err := filepath.WalkDir(agentsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && isActiveSessionFile(path) {
			files = append(files, path)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var turns []usageTurn
	for _, path := range files {
		fileTurns, err := collectFromFile(path)
		if err != nil {
			continue
		}
		turns = append(turns, fileTurns...)
	}

	return dedupeRepeatedTurns(turns), nil
}

// dedupeRepeatedTurns keeps one copy of every turn OpenClaw wrote to more than
// one transcript (finding H-01). It is not an edge case: OpenClaw snapshots a
// live conversation into a new `<ISO-8601>Z_<uuid>.jsonl` as it grows, so the
// files of one conversation form a chain of nested prefixes and every turn
// reappears in each later snapshot. Measured on this machine 2026-08-06: 463
// records for 300 real turns, +78.2% in tokens.
//
// A turn with no event id is never dropped. That only happens in transcripts
// older than the field (none on this machine today), and silently deleting real
// usage would be a worse failure than counting it twice.
func dedupeRepeatedTurns(turns []usageTurn) []Entry {
	owner := canonicalSessions(turns)

	var entries []Entry
	kept := make(map[string]bool, len(owner))
	for _, t := range turns {
		if t.eventID != "" {
			if kept[t.eventID] || owner[t.eventID] != t.entry.SessionID {
				continue
			}
			kept[t.eventID] = true
		}
		entries = append(entries, t.entry)
	}
	return entries
}

// canonicalSessions decides which transcript owns each turn, so a repeated turn
// keeps ONE session id instead of inventing a session per copy. That choice is
// not cosmetic: everything that reads a context stream per (session, thread)
// —internal/contextcurve, internal/quota— sees the conversation this names.
func canonicalSessions(turns []usageTurn) map[string]string {
	turnsPerSession := make(map[string]int)
	for _, t := range turns {
		turnsPerSession[t.entry.SessionID]++
	}

	owner := make(map[string]string, len(turns))
	for _, t := range turns {
		if t.eventID == "" {
			continue
		}
		current, seen := owner[t.eventID]
		if !seen || ownsTurn(t.entry.SessionID, current, turnsPerSession) {
			owner[t.eventID] = t.entry.SessionID
		}
	}
	return owner
}

// ownsTurn reports whether candidate is a better owner than current for a turn
// both transcripts recorded, applying three tie-breaks in order:
//
//  1. the transcript with MORE usage turns wins. Snapshots of one conversation
//     nest, so this is the file holding the whole thread — the only one whose
//     context curve is complete. Picking an older snapshot would truncate it.
//  2. a real conversation beats a gateway-fallback side-record.
//  3. the greater name wins. For `<ISO-8601>Z_<uuid>` snapshots that is the
//     newest one, since the format sorts chronologically; for anything else it
//     is an arbitrary but stable rule, which is what matters — the same disk
//     must always produce the same answer.
func ownsTurn(candidate, current string, turnsPerSession map[string]int) bool {
	if theirs, ours := turnsPerSession[candidate], turnsPerSession[current]; theirs != ours {
		return theirs > ours
	}
	if theirs, ours := isGatewayFallback(candidate), isGatewayFallback(current); theirs != ours {
		return !theirs
	}
	return candidate > current
}

func isGatewayFallback(sessionID string) bool {
	return strings.HasPrefix(sessionID, gatewayFallbackPrefix)
}

// isActiveSessionFile reports whether path is a live session transcript
// (foo.jsonl) and not a trajectory trace (foo.trajectory.jsonl). Reset/deleted
// archives don't end in .jsonl, so the suffix check already excludes them.
func isActiveSessionFile(path string) bool {
	name := filepath.Base(path)
	return strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".trajectory.jsonl")
}

func collectFromFile(path string) ([]usageTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	var turns []usageTurn
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // lines can be long (full assistant replies)

	for scanner.Scan() {
		if len(turns) >= maxEntriesPerFile {
			fmt.Fprintf(os.Stderr, "openclaw: %s excede %d entradas; se trunca (¿archivo enorme o corrupto?)\n", path, maxEntriesPerFile)
			break
		}
		var line sessionLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "message" || line.Message.Role != "assistant" {
			continue
		}
		if line.Message.Model == deliveryMirrorModel {
			continue
		}
		usage := line.Message.Usage
		if usage.Input == 0 && usage.Output == 0 && usage.CacheRead == 0 && usage.CacheWrite == 0 {
			continue
		}

		turns = append(turns, usageTurn{
			eventID: line.ID,
			entry: Entry{
				SessionID:                sessionID,
				Timestamp:                line.Timestamp,
				Model:                    line.Message.Model,
				InputTokens:              usage.Input,
				OutputTokens:             usage.Output,
				CacheCreationInputTokens: usage.CacheWrite,
				CacheReadInputTokens:     usage.CacheRead,
			},
		})
	}

	return turns, scanner.Err()
}
