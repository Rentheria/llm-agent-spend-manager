// This file covers OpenClaw's cron/heartbeat usage, which the JSONL session
// scan in openclaw.go does NOT see (docs/architecture.md §3.3, coverage gap).
//
// Cron runs write their usage to ~/.openclaw/state/openclaw.sqlite in the table
// cron_run_logs, not to agents/<id>/sessions/*.jsonl. Verified on this machine:
// the cron session ids have zero overlap with the active JSONL files, so reading
// both sources adds no double-count — it closes a real gap (~1.79M tokens that
// were invisible in the dashboard).
//
// The state DB is a live SQLite file the OpenClaw gateway writes to, so it is
// opened strictly read-only with a busy timeout; the pure-Go driver
// (modernc.org/sqlite) keeps the single-binary, CGO-free cross-compile intact.
package openclaw

import (
	"path/filepath"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/sqliteutil"
)

// cronToken bucketing: cron_run_logs exposes only total_tokens, with no
// input/output/cache split. Empirically these runs report input_tokens≈2 and
// output_tokens≈2 while total_tokens is tens of thousands — i.e. the total is
// overwhelmingly cache reads (each scheduled run re-reads the accumulated
// context). So the whole total is booked as cache-read tokens: that is both the
// empirically correct bucket and the conservative pricing floor (cache-read is
// the cheapest per-token rate). It must be labeled activity, never real spend,
// like every other figure here (see internal/pricing).

// StateDBPath returns the OpenClaw state database path
// (~/.openclaw/state/openclaw.sqlite), given a home directory.
func StateDBPath(homeDir string) string {
	return filepath.Join(homeDir, ".openclaw", "state", "openclaw.sqlite")
}

// cronUsageQuery selects only the cron runs that actually invoked a model.
// Rows with a null model / null total_tokens are lightweight reminders that
// sent text without an LLM call (verified: 81 of 99 rows) and carry no cost.
const cronUsageQuery = `
	SELECT ts, model, total_tokens, session_key
	FROM cron_run_logs
	WHERE model IS NOT NULL AND total_tokens > 0
	ORDER BY ts`

// CollectCronUsage reads OpenClaw's cron/heartbeat token usage from the
// OpenClaw state DB and returns one Entry per model-invoking run.
//
// A missing DB file is not an error (OpenClaw simply isn't installed / has no
// cron history) — it yields no entries, mirroring CollectUsage's treatment of a
// missing agents directory.
func CollectCronUsage(dbPath string) ([]Entry, error) {
	db, err := sqliteutil.OpenReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil
	}
	defer db.Close()

	rows, err := db.Query(cronUsageQuery)
	if err != nil {
		// A read failure against a live DB (locked, mid-write, table absent on an
		// older schema) shouldn't blank out the whole dashboard; report no cron
		// entries rather than aborting the fleet-wide collection.
		return nil, nil
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var (
			tsMillis    int64
			model       string
			totalTokens int
			sessionKey  string
		)
		if err := rows.Scan(&tsMillis, &model, &totalTokens, &sessionKey); err != nil {
			continue
		}
		entries = append(entries, Entry{
			SessionID:            sessionKey,
			Timestamp:            time.UnixMilli(tsMillis),
			Model:                model,
			CacheReadInputTokens: totalTokens,
		})
	}
	return entries, rows.Err()
}
