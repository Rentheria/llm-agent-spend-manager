package aggregate

import (
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/antigravity"
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/cursor"
	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/openclaw"
)

// Collect runs every MVP adapter against the given home directory and returns
// all turns as normalized records. A missing agent directory is not an error
// (the agent simply isn't installed / has no sessions) — it contributes no
// records. Only a real read failure propagates.
//
// homeDir is taken as a parameter rather than read from the environment so the
// caller (CLI, server, tests) controls it and can point at a fixture tree.
func Collect(homeDir string) ([]Record, error) {
	snapshot, err := CollectSnapshot(homeDir)
	return snapshot.Records, err
}

// Snapshot is everything one read of the machine yields: the normalized turns,
// plus the signals that don't fit the per-turn shape. Today that's the Anthropic
// session-limit events — the only observable evidence of where the subscription's
// quota ceiling actually sits (see internal/quota).
type Snapshot struct {
	Records []Record `json:"records"`
	// SessionLimits are the account's observed quota exhaustions, newest last.
	// Empty means the quota was never hit in the data on disk, which is a real
	// answer and not a missing one.
	SessionLimits []claudecode.LimitEvent `json:"sessionLimits"`
}

// CollectSnapshot is Collect plus the quota signals, read in the same pass over
// the transcripts. Callers that only need usage should use Collect.
func CollectSnapshot(homeDir string) (Snapshot, error) {
	ccSnapshot, err := claudecode.Collect(claudecode.ProjectsDir(homeDir))
	if err != nil {
		return Snapshot{}, err
	}
	records, err := collectRecords(homeDir, ccSnapshot.Turns)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Records: records, SessionLimits: ccSnapshot.SessionLimits}, nil
}

// collectRecords runs every adapter but claudecode and merges everything into
// one normalized stream. Claude Code's turns arrive already read, so the scan
// happens once per snapshot.
func collectRecords(homeDir string, ccEntries []claudecode.Entry) ([]Record, error) {
	ocEntries, err := openclaw.CollectUsage(openclaw.AgentsDir(homeDir))
	if err != nil {
		return nil, err
	}
	// Cron/heartbeat usage lives in the OpenClaw state DB, not the JSONL
	// sessions, and is disjoint from them — reading both closes the
	// coverage gap without double-counting.
	cronEntries, err := openclaw.CollectCronUsage(openclaw.StateDBPath(homeDir))
	if err != nil {
		return nil, err
	}
	// Cursor exposes no tokens/cost — these are activity-tier estimates (T10),
	// marked ConfidenceActivity so they never read as measured spend.
	cursorEntries, err := cursor.CollectUsage(homeDir)
	if err != nil {
		return nil, err
	}
	// Antigravity: measured token floor decoded from the stored protobuf, with
	// activity counts as the fallback; activity-tier (T11).
	antigravityEntries, err := antigravity.CollectUsage(homeDir)
	if err != nil {
		return nil, err
	}

	records := make([]Record, 0,
		len(ccEntries)+len(ocEntries)+len(cronEntries)+len(cursorEntries)+len(antigravityEntries))
	ccRecords := FromClaudeCode(ccEntries)
	records = append(records, ccRecords...)
	// OpenClaw records some of its own turns AND drives the Claude CLI, whose
	// transcripts the claudecode adapter already read: the same call lands in
	// both sources. The overlap is small in practice, but a measurement tool
	// cannot ship a known double count.
	records = append(records, dedupeAgainst(FromOpenClaw(ocEntries), ccRecords)...)
	records = append(records, FromOpenClawCron(cronEntries)...)
	records = append(records, FromCursor(cursorEntries)...)
	records = append(records, FromAntigravity(antigravityEntries)...)
	return records, nil
}

// turnFingerprint identifies one LLM call by its four token counts. Two adapters
// reading the same call from different files produce identical counts, while two
// genuinely different calls essentially never do (they'd need the same input,
// output, cache-read AND cache-write). Timestamps are deliberately NOT part of
// the key: the two sources record them at different moments of the same call.
type turnFingerprint struct{ input, output, cacheWrite, cacheRead int }

func fingerprint(r Record) turnFingerprint {
	return turnFingerprint{r.Input, r.Output, r.CacheWrite, r.CacheRead}
}

// dedupeAgainst drops records whose call was already counted in seen. A turn
// with no token counts at all is never deduped: those carry no signal and
// collapsing them would silently delete unrelated records.
func dedupeAgainst(records, seen []Record) []Record {
	known := make(map[turnFingerprint]bool, len(seen))
	for _, r := range seen {
		if r.TotalTokens() > 0 {
			known[fingerprint(r)] = true
		}
	}

	// Slice nuevo, no records[:0]: reusar el array de entrada muta al que llama,
	// y ahorrar una asignación no vale una sorpresa a distancia.
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if r.TotalTokens() > 0 && known[fingerprint(r)] {
			continue
		}
		out = append(out, r)
	}
	return out
}
