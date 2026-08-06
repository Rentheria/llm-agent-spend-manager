package outcome

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sources a marked change can come from.
const (
	SourceGit = "git"
	SourceLog = "log"
)

// Change is one change that was really made to the fleet, at a known time. It is
// the event a metric's movement gets contrasted against, so it is never derived
// from the metric itself and never inferred: it comes from a commit that exists
// in a repository or from a line somebody wrote in the fleet log.
type Change struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"`
	Repo   string    `json:"repo,omitempty"`
	Ref    string    `json:"ref"`
	Actor  string    `json:"actor"`
	Note   string    `json:"note"`
}

// ChangeLedger is every marked change plus an account of what could not be read.
//
// The skipped counts are part of the payload on purpose. A ledger that quietly
// dropped the lines it failed to parse would make its own coverage look better
// than it is, and the whole value of this layer is that a reader can tell the
// difference between "nothing happened then" and "we couldn't see it".
type ChangeLedger struct {
	Changes []Change `json:"changes"`
	Repos   []string `json:"repos"`
	Commits int      `json:"commits"`
	// LogEntries is how many log records parsed, LogUnreadable how many lines did
	// not (the fleet log has a handful of notes with raw newlines inside a JSON
	// string, which is not valid JSON), and LogNotAChange how many parsed fine but
	// carry an event that records talk rather than a change (see changeEvents).
	LogEntries    int `json:"logEntries"`
	LogUnreadable int `json:"logUnreadable"`
	LogNotAChange int `json:"logNotAChange"`
}

// changeEvents are the fleet-log event kinds that mark a change TO THE FLEET, as
// opposed to a message about it. A commit, a finished task and a fix each changed
// something that could move a metric; a note, a handoff, a work-in-progress
// marker or an alert did not.
//
// It is an explicit allowlist, not a heuristic over the note text, because that is
// what makes the ledger auditable: nothing is counted as a change because it
// "sounded like" one, and adding a kind is a visible edit here.
var changeEvents = map[string]bool{
	"commit": true,
	"done":   true,
	"fix":    true,
}

// gitLogTimeout bounds each `git log` call. The repositories on this machine
// answer in milliseconds; the deadline is there so a corrupt or network-backed
// repository cannot hang the report instead of failing it.
const gitLogTimeout = 20 * time.Second

// CollectChanges reads both sources of marked changes: the commits of every git
// repository under reposRoot and the entries of the fleet log at logPath.
//
// A missing source is not an error — it contributes nothing, and the ledger's
// counts say so — but a real read failure propagates, the same rule
// aggregate.Collect follows. Changes come back oldest first.
func CollectChanges(ctx context.Context, reposRoot, logPath string) (ChangeLedger, error) {
	ledger, err := collectGitChanges(ctx, reposRoot)
	if err != nil {
		return ChangeLedger{}, err
	}
	logChanges, counts, err := collectLogChanges(logPath)
	if err != nil {
		return ChangeLedger{}, err
	}
	ledger.Changes = append(ledger.Changes, logChanges...)
	ledger.LogEntries = counts.entries
	ledger.LogUnreadable = counts.unreadable
	ledger.LogNotAChange = counts.notAChange

	sort.Slice(ledger.Changes, func(i, j int) bool {
		if !ledger.Changes[i].At.Equal(ledger.Changes[j].At) {
			return ledger.Changes[i].At.Before(ledger.Changes[j].At)
		}
		return ledger.Changes[i].Ref < ledger.Changes[j].Ref
	})
	return ledger, nil
}

// collectGitChanges runs git log over every repository it finds under root.
func collectGitChanges(ctx context.Context, root string) (ChangeLedger, error) {
	repos, err := findRepos(root)
	if err != nil {
		return ChangeLedger{}, err
	}
	ledger := ChangeLedger{Repos: repos}
	for _, repo := range repos {
		commits, err := gitChanges(ctx, root, repo)
		if err != nil {
			return ChangeLedger{}, err
		}
		ledger.Changes = append(ledger.Changes, commits...)
		ledger.Commits += len(commits)
	}
	return ledger, nil
}

// findRepos lists the git repositories under root, checking root's own children
// and one level below them — the shape the fleet's checkouts actually have (a repo
// per directory, plus the multi-repo projects that keep a back and a front side by
// side). A missing root yields no repositories rather than an error: a machine
// without that directory simply has no commits to mark.
//
// Only a `.git` DIRECTORY counts. A worktree carries a `.git` file pointing at its
// parent, and counting one would put the same commits in the ledger twice.
func findRepos(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot list repositories under %s: %w", root, err)
	}

	repos := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if isRepo(filepath.Join(root, entry.Name())) {
			repos = append(repos, entry.Name())
			continue
		}
		nested, err := os.ReadDir(filepath.Join(root, entry.Name()))
		if err != nil {
			continue // unreadable directory: no repositories we can see, not a failure
		}
		for _, child := range nested {
			if child.IsDir() && isRepo(filepath.Join(root, entry.Name(), child.Name())) {
				repos = append(repos, filepath.Join(entry.Name(), child.Name()))
			}
		}
	}
	sort.Strings(repos)
	return repos, nil
}

func isRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

// gitLogFormat asks git for exactly the four fields a Change carries, separated by
// a unit-separator byte. A commit subject can contain any printable character —
// tabs, pipes, quotes — so the separator has to be one that cannot appear in it.
const gitLogFormat = "%H%x1f%aI%x1f%an%x1f%s"

// gitChanges reads one repository's commits as marked changes.
//
// Merge commits are excluded: a merge introduces no change of its own, its content
// is the commits it brings, and those are already in the ledger. Counting both
// would list the same work twice at two different times and make every attribution
// window look inseparable.
func gitChanges(ctx context.Context, root, repo string) ([]Change, error) {
	ctx, cancel := context.WithTimeout(ctx, gitLogTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", filepath.Join(root, repo),
		"log", "--no-merges", "--pretty=format:"+gitLogFormat)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log failed in %s: %w", repo, err)
	}
	return parseGitLog(repo, string(out))
}

// parseGitLog turns git's output into changes. A line that doesn't have the four
// fields is a bug in the format string, not bad data, so it fails loudly instead
// of being skipped.
func parseGitLog(repo, out string) ([]Change, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	changes := make([]Change, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x1f")
		if len(fields) != 4 {
			return nil, fmt.Errorf("git log line in %s has %d fields, want 4: %q", repo, len(fields), line)
		}
		at, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			return nil, fmt.Errorf("git log commit %.12s in %s has an unparseable date %q: %w",
				fields[0], repo, fields[1], err)
		}
		changes = append(changes, Change{
			At:     at,
			Source: SourceGit,
			Repo:   repo,
			Ref:    fields[0][:min(len(fields[0]), shortRefLen)],
			Actor:  fields[2],
			Note:   fields[3],
		})
	}
	return changes, nil
}

// shortRefLen is how much of a commit hash the ledger carries: enough to look the
// commit up by hand, short enough to fit a table cell.
const shortRefLen = 12

// logEntry is the fleet log's record shape: {"ts","by","ev","ref","note"}.
type logEntry struct {
	TS   string `json:"ts"`
	By   string `json:"by"`
	Ev   string `json:"ev"`
	Ref  string `json:"ref"`
	Note string `json:"note"`
}

// logCounts is the account of a fleet-log read: how much of it was usable.
type logCounts struct {
	entries    int
	unreadable int
	notAChange int
}

// collectLogChanges reads the fleet log. A missing file yields no changes and no
// error: the log is a source the machine may not have, not a requirement.
func collectLogChanges(path string) ([]Change, logCounts, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, logCounts{}, nil
	}
	if err != nil {
		return nil, logCounts{}, fmt.Errorf("cannot read the fleet log %s: %w", path, err)
	}
	defer file.Close()

	changes, counts := readLogChanges(file)
	return changes, counts, nil
}

// logLineLimit is the longest fleet-log line the reader accepts. The notes are
// prose and some run long; a line past this is treated as unreadable and counted,
// never silently truncated into a different record.
const logLineLimit = 1 << 20

// readLogChanges parses the fleet log line by line and keeps the entries whose
// event marks a real change.
//
// Line by line, rather than as one JSON stream, because a few of the notes were
// written with a raw newline inside the JSON string: those lines are invalid JSON
// and a stream decoder stops dead at the first one, taking the rest of the file
// with it. Skipping the unreadable line and counting it keeps the other ~800
// entries usable while still admitting what was lost.
func readLogChanges(r io.Reader) ([]Change, logCounts) {
	var counts logCounts
	var changes []Change

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), logLineLimit)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry logEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			counts.unreadable++
			continue
		}
		at, err := time.Parse(time.RFC3339, entry.TS)
		if err != nil {
			counts.unreadable++
			continue
		}
		counts.entries++
		if !changeEvents[entry.Ev] {
			counts.notAChange++
			continue
		}
		changes = append(changes, Change{
			At:     at,
			Source: SourceLog,
			Ref:    entry.Ref,
			Actor:  entry.By,
			Note:   entry.Note,
		})
	}
	if scanner.Err() != nil {
		// A line past logLineLimit stops the scan. Counting the rest of the file as
		// unreadable is not possible without reading it, so what IS knowable — that
		// the read ended early — is recorded as one unreadable line.
		counts.unreadable++
	}
	return changes, counts
}
