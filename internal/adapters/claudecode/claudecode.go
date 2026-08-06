// Package claudecode reads Claude Code's local session transcripts to extract
// real per-message token usage, without depending on any Node.js tooling or
// Claude Code internals beyond its on-disk JSONL format
// (~/.claude/projects/*/*.jsonl), the same format community tools like
// claude-usage and Claude-Code-Usage-Monitor already rely on.
package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxEntriesPerFile caps how many usage entries a single transcript can
// contribute, bounding memory against a pathologically large or corrupt file
// (L-02). It's a var, not a const, so tests can lower it. The ceiling sits far
// above any real session (hundreds of thousands of assistant turns).
var maxEntriesPerFile = 500_000

// Entry is one assistant turn with real token usage, as recorded by Claude
// Code itself.
type Entry struct {
	SessionID string
	// SessionLabel is the first thing the user asked in this session, verbatim
	// and truncated. It's what turns "session 8f3a…" into "which TASK cost this",
	// which is the question you actually want answered when a bill looks big.
	// Empty when the transcript has no usable user message.
	SessionLabel string
	// CWD is the working directory the session ran in. It's the only reliable
	// signal of WHO ran it: an OpenClaw agent drives this same CLI through its
	// gateway, so its turns land in Claude Code's transcripts too and would
	// otherwise all be counted as Claude Code. See aggregate.FromClaudeCode.
	CWD string
	// ThreadID identifies the context stream this turn belongs to inside the
	// session. Claude Code writes a subagent's turns under the SAME sessionId as
	// the main thread (they live in <session>/subagents/*.jsonl and carry an
	// "agentId"), but each subagent carries its own independent context — so a
	// context curve computed over the mixed stream is meaningless. Empty means the
	// main thread. Measured on this machine 2026-07-27: the priciest session had
	// 4,737 main-thread turns and 7,476 subagent turns under one session id.
	ThreadID                 string
	Timestamp                time.Time
	Model                    string
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// transcriptLine mirrors only the fields this package needs. Claude Code's
// JSONL has many more fields (thinking blocks, tool calls, cwd, git branch,
// ...); json.Unmarshal ignores anything not listed here, so this stays
// forward-compatible with format additions.
type transcriptLine struct {
	Type      string    `json:"type"`
	SessionID string    `json:"sessionId"`
	AgentID   string    `json:"agentId"`
	CWD       string    `json:"cwd"`
	Timestamp time.Time `json:"timestamp"`
	// IsAPIErrorMessage/Error mark a turn the API refused. They are what makes
	// the quota measurable: a refusal with Error == "rate_limit" is the only
	// place the account's usage cap becomes observable (see limits.go).
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Error             string `json:"error"`
	Message           struct {
		Model string `json:"model"`
		// Content is raw because Claude Code writes it either as a plain string
		// or as an array of typed blocks; sessionLabel handles both.
		Content json.RawMessage `json:"content"`
		Usage   struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ProjectsDir returns the default Claude Code transcript directory
// (~/.claude/projects), given a home directory.
func ProjectsDir(homeDir string) string {
	return filepath.Join(homeDir, ".claude", "projects")
}

// Snapshot is one read of the Claude Code transcripts. The turns and the quota
// refusals live in the same files and are gathered in the same pass: the scan
// parses several GB of JSONL, and reading it twice to get two views of it would
// double the cost of every report that needs both.
type Snapshot struct {
	Turns []Entry
	// SessionLimits are the moments the account ran out of session quota. Empty
	// is a legitimate answer (the quota was never hit) and callers must treat it
	// as "nothing measured", not as "nothing consumed".
	SessionLimits []LimitEvent
}

// CollectUsage walks every *.jsonl transcript under projectsDir and returns
// one Entry per assistant turn that reports token usage.
func CollectUsage(projectsDir string) ([]Entry, error) {
	snapshot, err := Collect(projectsDir)
	return snapshot.Turns, err
}

// Collect walks every *.jsonl transcript under projectsDir and returns both the
// priced turns and the quota refusals. Transcripts or lines that fail to parse
// are skipped rather than aborting the whole scan — a single malformed/partial
// line (e.g. a session still being written to) should not hide every other
// session's data.
func Collect(projectsDir string) (Snapshot, error) {
	var files []string

	err := filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".jsonl" {
			files = append(files, path)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}

	var snapshot Snapshot
	for _, path := range files {
		fileSnapshot, err := collectFromFile(path)
		if err != nil {
			continue
		}
		snapshot.Turns = append(snapshot.Turns, fileSnapshot.Turns...)
		snapshot.SessionLimits = append(snapshot.SessionLimits, fileSnapshot.SessionLimits...)
	}

	return snapshot, nil
}

func collectFromFile(path string) (Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()

	var snapshot Snapshot
	var label, cwd string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // transcripts can have long lines (thinking blocks)

	for scanner.Scan() {
		if len(snapshot.Turns) >= maxEntriesPerFile {
			fmt.Fprintf(os.Stderr, "claudecode: %s excede %d entradas; se trunca (¿archivo enorme o corrupto?)\n", path, maxEntriesPerFile)
			break
		}
		var line transcriptLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if cwd == "" && line.CWD != "" {
			cwd = line.CWD
		}
		if event, ok := limitEventFrom(line); ok {
			snapshot.SessionLimits = append(snapshot.SessionLimits, event)
			continue
		}
		if line.Type == "user" && label == "" {
			label = sessionLabel(line.Message.Content)
			continue
		}
		if line.Type != "assistant" {
			continue
		}
		if line.Message.Usage.InputTokens == 0 && line.Message.Usage.OutputTokens == 0 {
			continue
		}

		snapshot.Turns = append(snapshot.Turns, Entry{
			SessionID:                line.SessionID,
			SessionLabel:             label,
			CWD:                      cwd,
			ThreadID:                 line.AgentID,
			Timestamp:                line.Timestamp,
			Model:                    line.Message.Model,
			InputTokens:              line.Message.Usage.InputTokens,
			OutputTokens:             line.Message.Usage.OutputTokens,
			CacheCreationInputTokens: line.Message.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     line.Message.Usage.CacheReadInputTokens,
		})
	}

	return snapshot, scanner.Err()
}

// maxLabelChars keeps a session label short enough to scan in a table. The label
// is only there to recognize the task, not to reproduce the prompt.
const maxLabelChars = 120

// sessionLabel extracts a readable one-liner from a user message's content,
// which Claude Code writes either as a plain string or as an array of typed
// blocks. It returns "" for content that identifies no task — tool results and
// the harness's own <command…>/<system-reminder> blocks — so the caller keeps
// looking at later messages instead of labelling a session "<command-name>".
func sessionLabel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return ""
		}
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = b.Text
				break
			}
		}
	}

	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "<") {
		return ""
	}
	// Collapse newlines so a multi-line prompt still renders as one row.
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxLabelChars {
		return strings.TrimSpace(text[:maxLabelChars]) + "…"
	}
	return text
}
