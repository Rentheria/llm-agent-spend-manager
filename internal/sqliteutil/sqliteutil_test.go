package sqliteutil

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenReadOnly_MissingFileReturnsNilNil(t *testing.T) {
	db, err := OpenReadOnly(filepath.Join(t.TempDir(), "nope.sqlite"))
	if err != nil {
		t.Fatalf("err = %v, want nil for a missing file", err)
	}
	if db != nil {
		t.Fatalf("db = %v, want nil for a missing file", db)
	}
}

func TestOpenReadOnly_RefusesWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.sqlite")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`CREATE TABLE t (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if db == nil {
		t.Fatal("db = nil for an existing file")
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO t (n) VALUES (1)`); err == nil {
		t.Error("write succeeded on a read-only handle, want an error")
	}
	// Reads still work.
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(new(int)); err != nil {
		t.Errorf("read failed on read-only handle: %v", err)
	}
}
