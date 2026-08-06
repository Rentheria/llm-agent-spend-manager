// Package sqliteutil centralizes access to on-disk SQLite databases.
//
// Two kinds of file, and the difference is a hard rule:
//
//   - Databases owned by OTHER apps (OpenClaw state, Cursor tracking,
//     Antigravity summaries) are opened with OpenReadOnly, strictly read-only.
//     They belong to live third-party processes; this tool never writes to them.
//   - Databases this tool owns (the enforcement proxy's cap window) are opened
//     with OpenOwned, read-write. Only files under our own state directory
//     qualify.
//
// The driver is modernc.org/sqlite (pure Go): it keeps llm-agent-spend-manager a
// single CGO-free binary that cross-compiles across the release matrix.
package sqliteutil

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the CGO-free "sqlite" driver
)

// OpenReadOnly opens dbPath read-only. It returns (nil, nil) when the file does
// not exist so callers can treat "the app isn't installed / has no data" as an
// empty result rather than an error.
//
// mode=ro guarantees we never write to another app's live DB; busy_timeout lets
// a momentary write lock resolve instead of erroring immediately.
func OpenReadOnly(dbPath string) (*sql.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// OpenOwned opens (creating if needed) a database this tool owns, read-write.
// Unlike OpenReadOnly it never returns (nil, nil): a missing file is something
// to create, not an app that isn't installed.
//
// _txlock=immediate takes the write lock when a transaction begins rather than
// on its first write. Without it, two writers can both start, both read, and one
// then fails with SQLITE_BUSY partway through — for a counter that would mean a
// silently dropped increment. busy_timeout lets a contended lock resolve instead
// of erroring at once.
func OpenOwned(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("no pude crear el directorio de %s: %w", dbPath, err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("no pude abrir %s: %w", dbPath, err)
	}
	return db, nil
}
