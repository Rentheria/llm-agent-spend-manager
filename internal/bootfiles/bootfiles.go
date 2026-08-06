// Package bootfiles measures the size of the shared files every agent reads at
// the start of every session — the fleet's boot tax.
//
// This is not spend from a provider: it is bytes on this machine, read with
// os.Stat, no API involved. It belongs next to the cost metrics because it IS a
// cost with the same shape as the ones in internal/advise: a file that every
// agent loads on every boot is paid again on every session, for every agent,
// forever, whether or not today's task touches anything inside it. A shared file
// that grows without a ceiling is a permanent tax nobody is billed for and
// nobody notices.
//
// The trigger was real (2026-07-31): ~/.openclaw/workspace/AGENTS.md documents an
// index+detail split for closed tasks (state.json keeps id+title, the full note
// lives in tasks-cerradas.json), but 31 closed tasks had kept their full note
// glued to the boot index. Migrating them took state.json from 69KB to 33KB. The
// pattern existed; nothing was watching it, so it broke in silence.
//
// Scope, on purpose: SIZE IN BYTES against a threshold. Whether a given file
// *should* hold that content is a judgement call this package does not make.
package bootfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvWatchedFiles overrides the watched list. Format: "path=bytes" pairs
// separated by commas, e.g. "/home/x/state.json=67584,/home/x/SYNC.md=131072".
// It is configuration for the same reason a plan price is (internal/quota):
// which files a fleet boots from is a property of that fleet, not of the binary.
const EnvWatchedFiles = "SPEND_BOOT_FILES"

// Baselines and thresholds for this fleet's boot files, as measured on
// 2026-07-31. Read docs/archivos-arranque.md before changing them.
//
// The threshold rule is "twice the documented baseline", and it has exactly one
// real anchor: the erosion that was actually caught had state.json at 69KB
// against a 33KB cleaned index — 2.1x. So doubling is the size at which drift
// has already been visible and expensive once on this machine, rather than a
// number picked because it looked round.
//
// This is a STARTING POINT, not a validated limit. One data point is one data
// point; the honest thing to do is write it down, say what it came from, and
// adjust when there are more runs to calibrate against.
const (
	// stateJSONBaselineBytes is state.json right after the 2026-07-31 cleanup:
	// 33KB, the size of the index when it holds only what AGENTS.md says it
	// should (id + title per closed task, detail in tasks-cerradas.json).
	stateJSONBaselineBytes = 33 * 1024
	// syncBaselineBytes is SYNC.md as first measured, 2026-07-31: 62.9KB, taken
	// as 64KB. Unlike state.json this is NOT a known-good size — SYNC.md has
	// never been cleaned up, so this baseline bakes in whatever drift it already
	// carries. It is a floor to detect further growth from, nothing more.
	syncBaselineBytes = 64 * 1024

	// thresholdMultiplier turns a baseline into the size at which the file gets
	// reported. See the reasoning above.
	thresholdMultiplier = 2
)

// WatchedFile is one shared boot file and the size at which it stops being
// cheap enough to ignore.
type WatchedFile struct {
	Path           string `json:"path"`
	ThresholdBytes int64  `json:"thresholdBytes"`
}

// DefaultWatched is the list this fleet boots from. Deliberately short: each
// entry is here because a real session reads it at startup, not because it
// looked like it belonged on a list of files worth watching.
func DefaultWatched(homeDir string) []WatchedFile {
	workspace := filepath.Join(homeDir, ".openclaw", "workspace")
	return []WatchedFile{
		{
			Path:           filepath.Join(workspace, "state.json"),
			ThresholdBytes: stateJSONBaselineBytes * thresholdMultiplier,
		},
		{
			Path:           filepath.Join(workspace, "SYNC.md"),
			ThresholdBytes: syncBaselineBytes * thresholdMultiplier,
		},
	}
}

// LoadWatched reads the watched list from the environment, falling back to this
// fleet's defaults. A variable that is set but unparseable is an error rather
// than a silent fallback to the defaults: a typo in a path would otherwise leave
// the checker quietly watching a file nobody asked about.
func LoadWatched(getenv func(string) string, homeDir string) ([]WatchedFile, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	raw := strings.TrimSpace(getenv(EnvWatchedFiles))
	if raw == "" {
		return DefaultWatched(homeDir), nil
	}

	entries := strings.Split(raw, ",")
	watched := make([]WatchedFile, 0, len(entries))
	for _, entry := range entries {
		file, err := parseWatchedEntry(entry)
		if err != nil {
			return nil, err
		}
		watched = append(watched, file)
	}
	return watched, nil
}

// parseWatchedEntry reads one "path=bytes" pair.
func parseWatchedEntry(entry string) (WatchedFile, error) {
	path, rawThreshold, hasThreshold := strings.Cut(strings.TrimSpace(entry), "=")
	path = strings.TrimSpace(path)
	if !hasThreshold || path == "" {
		return WatchedFile{}, fmt.Errorf("%s=%q: se espera una lista de pares ruta=bytes separados por comas",
			EnvWatchedFiles, entry)
	}
	threshold, err := strconv.ParseInt(strings.TrimSpace(rawThreshold), 10, 64)
	if err != nil || threshold <= 0 {
		return WatchedFile{}, fmt.Errorf("%s: umbral %q para %s: se espera un número de bytes positivo",
			EnvWatchedFiles, rawThreshold, path)
	}
	return WatchedFile{Path: path, ThresholdBytes: threshold}, nil
}

// FileSize is one watched file as of this run: what it weighs now, how that
// compares to the last run that saw a different size, and whether it crossed its
// threshold.
type FileSize struct {
	Path           string `json:"path"`
	Bytes          int64  `json:"bytes"`
	ThresholdBytes int64  `json:"thresholdBytes"`
	// OverThreshold is the alert condition: strictly greater, so a file sitting
	// exactly at its threshold is still within it.
	OverThreshold bool `json:"overThreshold"`
	// DeltaBytes is the change since PreviousAt, and is only meaningful when
	// HasPrevious is true. Without an earlier measurement there is no delta to
	// report, and reporting 0 would read as "it didn't grow".
	HasPrevious bool      `json:"hasPrevious"`
	DeltaBytes  int64     `json:"deltaBytes"`
	PreviousAt  time.Time `json:"previousAt,omitempty"`
	// Unreadable carries the reason this file could not be measured (missing,
	// permission denied, a directory). A file that can't be read is reported as
	// such, never as 0 bytes — see docs/calibracion.md: nothing is invented.
	Unreadable string `json:"unreadable,omitempty"`
}

// Measured is true when this run got a real size for the file.
func (f FileSize) Measured() bool { return f.Unreadable == "" }

// Report is the whole boot-file check for one run.
type Report struct {
	CheckedAt time.Time  `json:"checkedAt"`
	Files     []FileSize `json:"files"`
}

// Oversized returns the files that crossed their threshold, in the order they
// were checked.
func (r Report) Oversized() []FileSize {
	out := make([]FileSize, 0, len(r.Files))
	for _, f := range r.Files {
		if f.OverThreshold {
			out = append(out, f)
		}
	}
	return out
}

// MeasuredCount is how many watched files this run could actually read. It is
// the denominator for any share over this report: files that could not be read
// are neither over nor under their threshold, and counting them either way would
// make the number say something the data doesn't.
func (r Report) MeasuredCount() int {
	n := 0
	for _, f := range r.Files {
		if f.Measured() {
			n++
		}
	}
	return n
}

// Collect is the entry point both surfaces share: read the watched list, read
// the previous snapshot, measure. It deliberately does NOT persist the new
// snapshot — the caller decides that, because only one of the two surfaces may
// write (see SaveSnapshot).
func Collect(getenv func(string) string, homeDir string, now time.Time) (Report, Snapshot, error) {
	watched, err := LoadWatched(getenv, homeDir)
	if err != nil {
		return Report{}, Snapshot{}, err
	}
	previous, err := LoadSnapshot(SnapshotPath(homeDir))
	if err != nil {
		return Report{}, Snapshot{}, err
	}
	report, next := Check(watched, previous, now)
	return report, next, nil
}

// Check measures every watched file and compares it against the previous
// snapshot. It is pure with respect to state: the caller owns the snapshot and
// decides whether to persist the returned one (the dashboard runs read-only and
// does not — see docs/servicios-permanentes.md).
//
// The returned snapshot only advances a file's entry when its size actually
// CHANGED. That is what makes the delta readable under a surface that runs the
// check often: "grew 4KB since 3 days ago" survives a dashboard poll every
// minute, while "vs the last run" would report 0 forever.
func Check(watched []WatchedFile, previous Snapshot, now time.Time) (Report, Snapshot) {
	report := Report{CheckedAt: now, Files: make([]FileSize, 0, len(watched))}
	next := Snapshot{Files: make(map[string]SizePoint, len(watched))}

	for _, file := range watched {
		size := measure(file)
		last, seenBefore := previous.Files[file.Path]

		switch {
		case !size.Measured():
			// Keep whatever we knew: a file that is unreadable today should not
			// erase the history that lets tomorrow's run report a real delta.
			if seenBefore {
				next.Files[file.Path] = last
			}
		case seenBefore && last.Bytes == size.Bytes:
			size.HasPrevious = true
			size.PreviousAt = last.At
			next.Files[file.Path] = last
		case seenBefore:
			size.HasPrevious = true
			size.PreviousAt = last.At
			size.DeltaBytes = size.Bytes - last.Bytes
			next.Files[file.Path] = SizePoint{Bytes: size.Bytes, At: now}
		default:
			next.Files[file.Path] = SizePoint{Bytes: size.Bytes, At: now}
		}
		report.Files = append(report.Files, size)
	}
	return report, next
}

// measure stats one file. Every failure mode gets a reason a human can act on
// instead of a zero that looks like a measurement.
func measure(file WatchedFile) FileSize {
	size := FileSize{Path: file.Path, ThresholdBytes: file.ThresholdBytes}

	info, err := os.Stat(file.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		size.Unreadable = "no derivable: el archivo no existe en esta máquina"
		return size
	case errors.Is(err, fs.ErrPermission):
		size.Unreadable = "no derivable: sin permiso de lectura"
		return size
	case err != nil:
		size.Unreadable = "no derivable: " + err.Error()
		return size
	case info.IsDir():
		size.Unreadable = "no derivable: la ruta es un directorio, no un archivo"
		return size
	}

	size.Bytes = info.Size()
	size.OverThreshold = size.Bytes > file.ThresholdBytes
	return size
}
