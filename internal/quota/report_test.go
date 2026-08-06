package quota

import (
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// The fleet as it was actually measured over the three days to 2026-07-28, in
// the proportions the manual analysis found. The absolute numbers are scaled
// down; the shares are the ones being reproduced, because they are what the
// report has to surface without anyone digging by hand.
const (
	openClawWorkspace = "/home/user/.openclaw/workspace"
	ccWorkspace       = "/home/user"
	repoWorkspace     = "/home/user/Develop/llm-agent-spend-manager"
)

func usage(agent, workspace, session, model string, turns, tokensPerTurn int, start time.Time) []aggregate.Record {
	out := make([]aggregate.Record, 0, turns)
	for i := 0; i < turns; i++ {
		out = append(out, aggregate.Record{
			Agent:        agent,
			Confidence:   aggregate.ConfidenceMeasured,
			SessionID:    session,
			SessionLabel: "arranque de " + session,
			Workspace:    workspace,
			Model:        model,
			Timestamp:    start.Add(time.Duration(i) * time.Second),
			Output:       tokensPerTurn,
		})
	}
	return out
}

// measuredFleet reproduces a concentrated split: the chat conversation
// carries most of the quota, in Opus, at far more tokens per turn than the code
// work does.
func measuredFleet(now time.Time) []aggregate.Record {
	start := now.Add(-48 * time.Hour)
	var records []aggregate.Record
	records = append(records, usage(aggregate.AgentOpenClaw, openClawWorkspace, "telegram", "claude-opus-5", 500, 170_000, start)...)
	records = append(records, usage(aggregate.AgentClaudeCode, ccWorkspace, "cc-term", "claude-opus-5", 300, 100_000, start)...)
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "repo", "claude-sonnet-5", 200, 60_000, start)...)
	return records
}

func TestAnalyze_PutsTheHeaviestWorkspaceFirst(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	snapshot := aggregate.Snapshot{Records: measuredFleet(now)}

	report := Analyze(snapshot, Config{ClaudeTier: "Max 5x", CursorMonthlyUSD: 200, CursorRenewalDay: 1},
		now.Add(-72*time.Hour), now)

	b := report.Breakdown
	if len(b.ByWorkspace) == 0 {
		t.Fatal("no workspace breakdown")
	}
	if b.ByWorkspace[0].Workspace != openClawWorkspace {
		t.Errorf("first workspace = %q, want the Telegram conversation", b.ByWorkspace[0].Workspace)
	}
	if b.ByAgent[0].Agent != aggregate.AgentOpenClaw {
		t.Errorf("first agent = %q, want %q", b.ByAgent[0].Agent, aggregate.AgentOpenClaw)
	}
	if b.ByModel[0].Model != "claude-opus-5" {
		t.Errorf("first model = %q, want claude-opus-5", b.ByModel[0].Model)
	}
	if len(b.TopSessions) == 0 || b.TopSessions[0].SessionID != "telegram" {
		t.Errorf("top session = %+v, want the telegram session", b.TopSessions)
	}
}

// This is the compliance bar for the whole command: the finding that 55% of the
// quota goes to the Telegram conversation, in Opus, at ~170k tokens/turn has to
// come out of the report by itself.
func TestAnalyze_ShoutsTheConcentrationWithoutAnyoneDigging(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	snapshot := aggregate.Snapshot{Records: measuredFleet(now)}

	report := Analyze(snapshot, Config{ClaudeTier: "Max 5x", CursorMonthlyUSD: 200, CursorRenewalDay: 1},
		now.Add(-72*time.Hour), now)

	var lever Lever
	for _, l := range report.Levers {
		if l.ID == LeverConcentration {
			lever = l
		}
	}
	if lever.ID == "" {
		t.Fatalf("the concentration lever did not fire; levers = %+v", report.Levers)
	}
	if lever.QuotaShare < 0.50 {
		t.Errorf("quota share = %.2f, want the majority", lever.QuotaShare)
	}
	if !strings.Contains(lever.Title, "workspace") {
		t.Errorf("title does not name the workspace: %q", lever.Title)
	}
	// The per-turn weight is the mechanism, so it has to be in the evidence.
	if !strings.Contains(lever.Evidence, "170,000 tokens/turno") {
		t.Errorf("evidence does not carry the tokens/turn: %q", lever.Evidence)
	}
	if lever.Action == "" {
		t.Error("a lever with no action is a report, which is what this replaces")
	}
}

func TestAnalyze_BoundsTheBreakdownToItsPeriod(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	records := measuredFleet(now)
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "old", "claude-opus-5", 10, 1_000_000,
		now.Add(-30*24*time.Hour))...)

	report := Analyze(aggregate.Snapshot{Records: records}, Config{}, now.Add(-72*time.Hour), now)
	for _, s := range report.Breakdown.TopSessions {
		if s.SessionID == "old" {
			t.Error("a session from a month ago leaked into a 3-day breakdown")
		}
	}
}

func TestAnalyze_CarriesTheCalibrationNoteEvenWithNoExhaustions(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	report := Analyze(aggregate.Snapshot{Records: measuredFleet(now)}, Config{}, now.Add(-72*time.Hour), now)
	if report.Calibration.Note == "" {
		t.Fatal("no calibration note: a thin calibration must announce itself, not stay silent")
	}
	if report.Calibration.Capacity.Known {
		t.Error("a ceiling was calibrated with zero observed exhaustions")
	}
	for _, c := range report.Cycles {
		if c.FillKnown && c.Cycle.Name == CycleSessionWindow {
			t.Error("a window percentage was shown against a ceiling nobody measured")
		}
	}
}

func TestTopSessions_KeepsTheHeaviestAndCapsTheTail(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var records []aggregate.Record
	for i := 0; i < topSessionCount+5; i++ {
		records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace,
			string(rune('a'+i)), "claude-opus-5", 1, (i+1)*1000, now)...)
	}

	got := topSessions(records, topSessionCount)
	if len(got) != topSessionCount {
		t.Fatalf("sessions = %d, want %d", len(got), topSessionCount)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Totals.TotalTokens < got[i].Totals.TotalTokens {
			t.Fatalf("sessions are not heaviest-first: %+v", got)
		}
	}
}
