package bootfiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The rest of the tool is stateless on purpose: every number it reports is
// recomputed from the usage records, so there is no stored memory to drift out
// of sync with reality (see internal/advise/escalation.go).
//
// A file size is the one thing that cannot follow that rule. It is not in the
// records, and "how much did it grow" is not answerable from a single os.Stat.
// So this package keeps the smallest possible exception: one JSON file holding
// the last DISTINCT size seen per watched path. Nothing derived, nothing
// aggregated — if it is deleted, the next run simply reports no delta and says
// so, and one run later the history is back.
const (
	snapshotDirName  = "llm-agent-spend-manager"
	snapshotFileName = "bootfiles.json"
	// snapshotFileMode / snapshotDirMode: owner-only. The paths being watched
	// are a description of this user's private workspace.
	snapshotFileMode fs.FileMode = 0o600
	snapshotDirMode  fs.FileMode = 0o700
)

// SizePoint is the last distinct size observed for a path, and when it was
// first observed at that size. "First observed at that size" — not "last
// checked" — is what makes the delta legible: the dashboard re-runs this check
// on every poll, and if each run overwrote the timestamp the answer would
// forever be "no change since a minute ago".
type SizePoint struct {
	Bytes int64     `json:"bytes"`
	At    time.Time `json:"at"`
}

// Snapshot is the previous run's view, keyed by path.
type Snapshot struct {
	Files map[string]SizePoint `json:"files"`
}

// SnapshotPath is where the snapshot lives: the same state dir the enforcement
// counter already uses (internal/enforce/sqlitecounter.go), so there is one
// place to look for "state this tool keeps on disk" instead of two.
func SnapshotPath(homeDir string) string {
	return filepath.Join(homeDir, ".local", "state", snapshotDirName, snapshotFileName)
}

// LoadSnapshot reads the previous run's sizes. A missing file is not an error:
// the first run of a new install has no history, and the report says so rather
// than pretending the delta is zero.
//
// A CORRUPT file, on the other hand, is an error. Silently starting over would
// throw away the history that makes the delta worth reporting, and would do it
// quietly — exactly the failure mode this whole ticket exists to catch.
func LoadSnapshot(path string) (Snapshot, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Snapshot{Files: map[string]SizePoint{}}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("leyendo el snapshot de archivos de arranque %s: %w", path, err)
	}

	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot de archivos de arranque corrupto en %s: %w", path, err)
	}
	if snapshot.Files == nil {
		snapshot.Files = map[string]SizePoint{}
	}
	return snapshot, nil
}

// SaveSnapshot writes the sizes this run observed, creating the state dir if it
// is not there yet.
//
// Only the CLI calls this. The dashboard reads the snapshot but never writes it:
// its systemd unit runs with ProtectHome=read-only (docs/servicios-permanentes.md),
// and a metric surface has no business mutating state just because someone
// refreshed a browser tab.
func SaveSnapshot(path string, snapshot Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), snapshotDirMode); err != nil {
		return fmt.Errorf("creando el directorio de estado %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("serializando el snapshot de archivos de arranque: %w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), snapshotFileMode); err != nil {
		return fmt.Errorf("escribiendo el snapshot de archivos de arranque %s: %w", path, err)
	}
	return nil
}
