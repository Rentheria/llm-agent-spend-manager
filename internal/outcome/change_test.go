package outcome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadLogChanges_KeepsOnlyTheEntriesThatMarkAChange(t *testing.T) {
	// A note and a work-in-progress marker are messages ABOUT the work; a commit and
	// a finished task are the work landing. Only the second kind can move a metric.
	log := strings.Join([]string{
		`{"ts":"2026-07-10T10:00:00Z","by":"claude-code","ev":"note","ref":"T1","note":"pensando"}`,
		`{"ts":"2026-07-10T11:00:00Z","by":"claude-code","ev":"commit","ref":"T1","note":"cache off en el lanzador"}`,
		`{"ts":"2026-07-10T12:00:00Z","by":"openclaw","ev":"wip","ref":"T2","note":"a medias"}`,
		`{"ts":"2026-07-10T13:00:00Z","by":"openclaw","ev":"done","ref":"T2","note":"tope de contexto cableado"}`,
	}, "\n")

	changes, counts := readLogChanges(strings.NewReader(log))

	if len(changes) != 2 {
		t.Fatalf("kept %d changes, want 2 (the commit and the done): %+v", len(changes), changes)
	}
	if counts.entries != 4 || counts.notAChange != 2 {
		t.Errorf("counts = %d entries / %d not-a-change, want 4 / 2", counts.entries, counts.notAChange)
	}
	if changes[0].Ref != "T1" || changes[0].Source != SourceLog || changes[0].Actor != "claude-code" {
		t.Errorf("first change = %+v, want the T1 commit by claude-code from the log", changes[0])
	}
}

func TestReadLogChanges_CountsTheLinesItCannotRead(t *testing.T) {
	// The real fleet log has notes written with a raw newline inside the JSON string,
	// which makes those lines invalid JSON. Skipping them silently would make the
	// ledger's coverage look better than it is, so they are counted.
	log := strings.Join([]string{
		`{"ts":"2026-07-10T11:00:00Z","by":"claude-code","ev":"commit","ref":"T1","note":"ok"}`,
		`{"ts":"2026-07-10T12:00:00Z","by":"claude-code","ev":"done","ref":"T2","note":"esto se corta`,
		`y sigue en otra línea"}`,
		`{"ts":"nunca","by":"claude-code","ev":"done","ref":"T3","note":"fecha ilegible"}`,
	}, "\n")

	changes, counts := readLogChanges(strings.NewReader(log))

	if counts.unreadable != 3 {
		t.Errorf("unreadable = %d, want 3 (two halves of the broken note plus the bad timestamp)", counts.unreadable)
	}
	if len(changes) != 1 {
		t.Errorf("kept %d changes, want only the one readable commit: %+v", len(changes), changes)
	}
}

func TestReadLogChanges_ReadsTheTimestampInItsOwnZone(t *testing.T) {
	// The fleet log mixes UTC and local offsets on purpose (whoever wrote the line
	// wrote its own clock). Both have to land on the same instant, or a change will
	// fall in the wrong attribution window by six hours.
	log := strings.Join([]string{
		`{"ts":"2026-07-10T18:00:00Z","by":"claude-code","ev":"done","ref":"utc","note":"a"}`,
		`{"ts":"2026-07-10T12:00:00-06:00","by":"claude-code","ev":"done","ref":"local","note":"b"}`,
	}, "\n")

	changes, _ := readLogChanges(strings.NewReader(log))

	if len(changes) != 2 {
		t.Fatalf("kept %d changes, want 2", len(changes))
	}
	if !changes[0].At.Equal(changes[1].At) {
		t.Errorf("the same instant written two ways parsed differently: %s vs %s", changes[0].At, changes[1].At)
	}
}

func TestParseGitLog_ReadsCommitsWithAwkwardSubjects(t *testing.T) {
	// A commit subject can hold anything printable, tabs and pipes included, which
	// is why the fields are separated by a unit-separator byte and not by a
	// character somebody might type.
	out := strings.Join([]string{
		"a1b2c3d4e5f60718\x1f2026-07-10T11:00:00-06:00\x1fdev\x1ffix(cache): quitar\tcache | del lanzador",
		"00112233445566778\x1f2026-07-11T09:30:00Z\x1fjr\x1ffeat: tope de contexto",
	}, "\n")

	changes, err := parseGitLog("llm-agent-spend-manager", out)

	if err != nil {
		t.Fatalf("parseGitLog failed on well-formed output: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("parsed %d commits, want 2", len(changes))
	}
	if changes[0].Ref != "a1b2c3d4e5f6" {
		t.Errorf("ref = %q, want the first %d chars of the hash", changes[0].Ref, shortRefLen)
	}
	if changes[0].Note != "fix(cache): quitar\tcache | del lanzador" {
		t.Errorf("note = %q, want the subject intact including its tab and pipe", changes[0].Note)
	}
	if changes[0].Source != SourceGit || changes[0].Repo != "llm-agent-spend-manager" {
		t.Errorf("change = %+v, want it marked as a git change in the repo it came from", changes[0])
	}
}

func TestParseGitLog_FailsLoudlyOnOutputItDoesNotUnderstand(t *testing.T) {
	// Output that doesn't match the format string means the format string changed,
	// not that the data is dirty. Skipping the line would silently shrink the ledger.
	_, err := parseGitLog("repo", "a1b2c3\x1f2026-07-10T11:00:00Z\x1fonly three fields")

	if err == nil {
		t.Error("parseGitLog accepted a line with 3 fields; a malformed format must fail, not be skipped")
	}
}

func TestFindRepos_SkipsWorktreesAndDescendsOneLevel(t *testing.T) {
	// The fleet keeps single repos directly under the root and the two-sided projects
	// one level down. Worktrees carry a .git FILE pointing at their parent, and
	// counting one would put the same commits in the ledger twice.
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "spend-manager", ".git"))
	mustMkdirAll(t, filepath.Join(root, "MiProducto", "backend", ".git"))
	mustMkdirAll(t, filepath.Join(root, "MiProducto", "frontend", ".git"))
	mustMkdirAll(t, filepath.Join(root, "_wt", "spend-T77"))
	mustWriteFile(t, filepath.Join(root, "_wt", "spend-T77", ".git"), "gitdir: /elsewhere")

	repos, err := findRepos(root)

	if err != nil {
		t.Fatalf("findRepos failed: %v", err)
	}
	want := []string{"MiProducto/backend", "MiProducto/frontend", "spend-manager"}
	if strings.Join(repos, ",") != strings.Join(want, ",") {
		t.Errorf("repos = %v, want %v", repos, want)
	}
}

func TestFindRepos_TreatsAMissingRootAsNoRepositories(t *testing.T) {
	// A machine without that directory has no commits to mark. That is not a failure
	// of the tool, and turning it into one would make the command unusable elsewhere.
	repos, err := findRepos(filepath.Join(t.TempDir(), "no-such-dir"))

	if err != nil {
		t.Errorf("findRepos on a missing root returned an error: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("repos = %v, want none", repos)
	}
}

func TestCollectChanges_ToleratesSourcesThatAreNotThere(t *testing.T) {
	// Neither source present: an empty ledger, no error, and counts that say so.
	root := t.TempDir()

	ledger, err := CollectChanges(t.Context(), filepath.Join(root, "nope"), filepath.Join(root, "nope.ndjson"))

	if err != nil {
		t.Fatalf("CollectChanges failed with both sources missing: %v", err)
	}
	if len(ledger.Changes) != 0 || ledger.Commits != 0 || ledger.LogEntries != 0 {
		t.Errorf("ledger = %+v, want it empty", ledger)
	}
}

func TestCollectChanges_ReturnsBothSourcesOldestFirst(t *testing.T) {
	// The attribution window walks the list in time order, so the ledger owes it a
	// single merged series and not two sources side by side.
	root := t.TempDir()
	logPath := filepath.Join(root, "log.ndjson")
	mustWriteFile(t, logPath, strings.Join([]string{
		`{"ts":"2026-07-12T10:00:00Z","by":"openclaw","ev":"done","ref":"late","note":"b"}`,
		`{"ts":"2026-07-10T10:00:00Z","by":"openclaw","ev":"done","ref":"early","note":"a"}`,
	}, "\n"))

	ledger, err := CollectChanges(t.Context(), filepath.Join(root, "no-repos"), logPath)

	if err != nil {
		t.Fatalf("CollectChanges failed: %v", err)
	}
	if len(ledger.Changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(ledger.Changes))
	}
	if ledger.Changes[0].Ref != "early" || ledger.Changes[1].Ref != "late" {
		t.Errorf("changes are not oldest first: %q then %q", ledger.Changes[0].Ref, ledger.Changes[1].Ref)
	}
	if !ledger.Changes[0].At.Before(ledger.Changes[1].At.Add(time.Nanosecond)) {
		t.Error("timestamps did not survive the merge")
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("cannot create %s: %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("cannot write %s: %v", path, err)
	}
}
