package openclaw

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// writeCronFixtureDB creates a temp SQLite DB with the real cron_run_logs schema
// (a subset of the columns this adapter reads) and the given rows, returning its
// path. Rows are {ts, model, totalTokens, sessionKey}; a nil model or zero
// totalTokens mirrors the lightweight-reminder rows that carry no LLM usage.
func writeCronFixtureDB(t *testing.T, rows []cronRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openclaw.sqlite")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE cron_run_logs (
		ts INTEGER NOT NULL,
		model TEXT,
		total_tokens INTEGER,
		session_key TEXT
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO cron_run_logs (ts, model, total_tokens, session_key) VALUES (?, ?, ?, ?)`,
			r.ts, r.model, r.totalTokens, r.sessionKey,
		); err != nil {
			t.Fatalf("insert row: %v", err)
		}
	}
	return path
}

type cronRow struct {
	ts          int64
	model       any // string or nil
	totalTokens any // int or nil
	sessionKey  string
}

func TestCollectCronUsage_ReadsModelInvokingRunsOnly(t *testing.T) {
	path := writeCronFixtureDB(t, []cronRow{
		{ts: 1_782_695_950_889, model: "claude-opus-4-8", totalTokens: 65943, sessionKey: "agent:main:cron:job-a:run:1"},
		{ts: 1_782_696_850_860, model: "claude-sonnet-5", totalTokens: 88191, sessionKey: "agent:main:cron:job-b:run:2"},
		// Lightweight reminder: no model, no tokens — must be excluded.
		{ts: 1_782_697_750_858, model: nil, totalTokens: nil, sessionKey: "agent:main:cron:job-c:run:3"},
		// Model present but zero tokens — nothing to cost, excluded.
		{ts: 1_782_698_650_849, model: "claude-opus-4-8", totalTokens: 0, sessionKey: "agent:main:cron:job-d:run:4"},
	})

	entries, err := CollectCronUsage(path)
	if err != nil {
		t.Fatalf("CollectCronUsage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (null-model and zero-token rows excluded)", len(entries))
	}

	opus := entries[0]
	if opus.Model != "claude-opus-4-8" {
		t.Errorf("Model = %q, want claude-opus-4-8", opus.Model)
	}
	if opus.SessionID != "agent:main:cron:job-a:run:1" {
		t.Errorf("SessionID = %q, want the session_key", opus.SessionID)
	}
	// total_tokens is booked entirely as cache-read (see cron.go rationale).
	if opus.CacheReadInputTokens != 65943 {
		t.Errorf("CacheReadInputTokens = %d, want 65943", opus.CacheReadInputTokens)
	}
	if opus.InputTokens != 0 || opus.OutputTokens != 0 || opus.CacheCreationInputTokens != 0 {
		t.Errorf("non-cache-read buckets = (%d in, %d out, %d write), want all 0",
			opus.InputTokens, opus.OutputTokens, opus.CacheCreationInputTokens)
	}
	wantTime := time.UnixMilli(1_782_695_950_889)
	if !opus.Timestamp.Equal(wantTime) {
		t.Errorf("Timestamp = %v, want %v (ts is epoch millis)", opus.Timestamp, wantTime)
	}
}

func TestCollectCronUsage_CostsCronTokensAtCacheReadFloor(t *testing.T) {
	path := writeCronFixtureDB(t, []cronRow{
		{ts: 1, model: "claude-opus-4-8", totalTokens: 1_000_000, sessionKey: "agent:main:cron:job:run:1"},
	})

	entries, err := CollectCronUsage(path)
	if err != nil {
		t.Fatalf("CollectCronUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}

	cost, known := EstimateCostUSD(entries[0])
	if !known {
		t.Fatal("known = false, want true (claude-opus-4-8 is priced)")
	}
	// 1M cache-read tokens × opus cache-read rate ($0.50/M) = $0.50, the
	// conservative floor — not the $5/M input rate.
	if cost < 0.4999 || cost > 0.5001 {
		t.Errorf("cost = %v, want ~0.50 (cache-read floor, not input rate)", cost)
	}
}

func TestCollectCronUsage_MissingDBReturnsEmptyNotError(t *testing.T) {
	entries, err := CollectCronUsage(filepath.Join(t.TempDir(), "does-not-exist.sqlite"))
	if err != nil {
		t.Fatalf("CollectCronUsage: %v, want nil error for a missing DB", err)
	}
	if entries != nil {
		t.Fatalf("entries = %v, want nil", entries)
	}
}

func TestStateDBPath(t *testing.T) {
	got := StateDBPath("/home/user")
	want := filepath.Join("/home/user", ".openclaw", "state", "openclaw.sqlite")
	if got != want {
		t.Errorf("StateDBPath = %q, want %q", got, want)
	}
}
