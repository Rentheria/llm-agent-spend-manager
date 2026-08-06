package enforce

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/sqliteutil"
)

// upsertWindowSQL is to SQLite what the Lua script is to Redis: the whole
// read-modify-write in ONE statement, so two concurrent requests near the cap can
// never both read a stale total and overshoot it.
//
// The CASE arms are `advance` (window.go) expressed in SQL — same three cases,
// same order: same bucket accumulates, one bucket on shifts current into
// previous, two or more starts clean.
//
//	?1 = key   ?2 = amount   ?3 = current bucket index
const upsertWindowSQL = `
INSERT INTO cap_window (key, bucket, current, previous)
VALUES (?1, ?3, ?2, 0)
ON CONFLICT(key) DO UPDATE SET
    previous = CASE ?3 - cap_window.bucket
                   WHEN 0 THEN cap_window.previous
                   WHEN 1 THEN cap_window.current
                   ELSE 0 END,
    current  = CASE ?3 - cap_window.bucket
                   WHEN 0 THEN cap_window.current + ?2
                   ELSE ?2 END,
    bucket   = ?3
RETURNING current, previous`

const createWindowTableSQL = `
CREATE TABLE IF NOT EXISTS cap_window (
    key      TEXT    PRIMARY KEY,
    bucket   INTEGER NOT NULL,
    current  INTEGER NOT NULL,
    previous INTEGER NOT NULL
)`

// SQLiteCounter tallies in a file this tool owns. It is the default because it
// costs nothing to install — the driver (modernc.org/sqlite, pure Go) is already
// linked into the binary for the adapters — and unlike MemoryCounter the window
// survives a restart. That matters once the proxy runs as a service: every
// upgrade, crash or reboot would otherwise hand the fleet a fresh budget.
//
// SQLite's write lock is held ACROSS processes, so two proxies pointed at the
// same file share one cap correctly. What it cannot do is span machines; that is
// the one case left for RedisCounter.
type SQLiteCounter struct {
	db *sql.DB
	// now is injectable so window rollover can be tested without sleeping.
	now func() time.Time
}

// NewSQLiteCounter opens (creating if needed) the counter at dbPath.
func NewSQLiteCounter(dbPath string) (*SQLiteCounter, error) {
	db, err := sqliteutil.OpenOwned(dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(createWindowTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("no pude preparar la tabla del tope en %s: %w", dbPath, err)
	}
	return &SQLiteCounter{db: db, now: time.Now}, nil
}

// Add implements Counter.
func (c *SQLiteCounter) Add(ctx context.Context, key string, amount int64, window time.Duration) (int64, error) {
	now := c.now()
	bucket := bucketIndex(now, window)

	// Rows more than one bucket behind are dropped here rather than by a
	// background sweep: per-agent keys are unbounded in principle, and a file
	// that only grows is a slow leak on a process meant to run for months.
	if _, err := c.db.ExecContext(ctx, `DELETE FROM cap_window WHERE ?1 - bucket > 1`, bucket); err != nil {
		return 0, fmt.Errorf("no pude limpiar ventanas vencidas: %w", err)
	}

	st := windowState{bucket: bucket}
	if err := c.db.QueryRowContext(ctx, upsertWindowSQL, key, amount, bucket).
		Scan(&st.current, &st.previous); err != nil {
		return 0, fmt.Errorf("no pude contabilizar en el tope: %w", err)
	}
	return estimate(st, now, window), nil
}

// Close releases the database handle.
func (c *SQLiteCounter) Close() error { return c.db.Close() }

// DefaultStatePath returns where the cap window lives by default, following the
// XDG state convention (state = data that should persist but is not precious
// enough for the backup-worthy data dir; losing it costs at most one window).
func DefaultStatePath() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "llm-agent-spend-manager", "cap.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no pude ubicar el home para el archivo del tope: %w", err)
	}
	return filepath.Join(home, ".local", "state", "llm-agent-spend-manager", "cap.db"), nil
}
