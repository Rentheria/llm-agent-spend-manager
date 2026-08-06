package quota

import (
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

func breakdownFrom(records []aggregate.Record) Breakdown {
	return breakdownOf(records, time.Time{})
}

// The counterfactual is capped at a rate the fleet already runs at — never at
// zero and never at an invented target (see docs/workload-classes.md §5.1).
func TestTokensPerTurnLever_CapsTheSavingAtTheObservedMedian(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var records []aggregate.Record
	records = append(records, usage(aggregate.AgentOpenClaw, openClawWorkspace, "chat", "claude-opus-5", 100, 300_000, now)...)
	records = append(records, usage(aggregate.AgentClaudeCode, ccWorkspace, "a", "claude-opus-5", 100, 100_000, now)...)
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "b", "claude-opus-5", 100, 100_000, now)...)

	lever, ok := tokensPerTurnLever(breakdownFrom(records), Burn{PerHour: 1_000_000})
	if !ok {
		t.Fatal("a workspace at 3x the median did not trigger the lever")
	}
	// 100 turns × (300k − 100k median) = 20M, not 30M: the target is the observed
	// median, not zero consumption.
	if lever.TokensSaved != 20_000_000 {
		t.Errorf("tokens saved = %v, want 20,000,000 (capped at the median)", lever.TokensSaved)
	}
	if lever.WindowExtension != 20*time.Hour {
		t.Errorf("window extension = %s, want 20h at 1M/h", lever.WindowExtension)
	}
}

func TestTokensPerTurnLever_StaysQuietWhenNobodyIsAnOutlier(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var records []aggregate.Record
	records = append(records, usage(aggregate.AgentClaudeCode, ccWorkspace, "a", "claude-opus-5", 100, 110_000, now)...)
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "b", "claude-opus-5", 100, 100_000, now)...)

	if _, ok := tokensPerTurnLever(breakdownFrom(records), Burn{PerHour: 1000}); ok {
		t.Error("fired on a 1.1x spread; a lever nobody should pull is noise")
	}
}

func TestConcentrationLever_IgnoresAWorkspaceWithTooLittleBehindIt(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var records []aggregate.Record
	// One enormous turn is a majority share and an anecdote at the same time.
	records = append(records, usage(aggregate.AgentClaudeCode, ccWorkspace, "one", "claude-opus-5", 1, 90_000_000, now)...)
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "b", "claude-opus-5", 100, 100_000, now)...)

	if _, ok := concentrationLever(breakdownFrom(records), Burn{}); ok {
		t.Error("fired on a single turn")
	}
}

// The second-biggest model by volume is not necessarily the cheaper one. On this
// fleet sonnet-5 carries long-context work and costs MORE per turn than opus-5,
// so recommending it would make the window shorter.
func TestModelMixLever_NeverRecommendsAModelThatCostsMorePerTurn(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var records []aggregate.Record
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "a", "claude-opus-5", 500, 160_000, now)...)
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "b", "claude-sonnet-5", 150, 400_000, now)...)

	lever, ok := modelMixLever(breakdownFrom(records), Burn{})
	if ok {
		t.Errorf("recommended %q when the only alternative is heavier per turn", lever.Action)
	}
}

func TestModelMixLever_NeedsTheAlternativeToCarryComparableWork(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var records []aggregate.Record
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "a", "claude-opus-5", 500, 160_000, now)...)
	// 25 turns of a small model always looks cheap and proves nothing.
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "b", "claude-fable-5", 25, 50_000, now)...)

	if _, ok := modelMixLever(breakdownFrom(records), Burn{}); ok {
		t.Error("fired on a model with 5% of the dominant model's turns")
	}
}

func TestModelMixLever_FiresOnAGenuinelyLighterAlternativeAndClaimsNoSaving(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var records []aggregate.Record
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "a", "claude-opus-5", 500, 160_000, now)...)
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "b", "claude-opus-4-8", 200, 110_000, now)...)

	lever, ok := modelMixLever(breakdownFrom(records), Burn{PerHour: 1000})
	if !ok {
		t.Fatal("did not fire on a dominant model with a lighter, well-used alternative")
	}
	if !strings.Contains(lever.Action, "claude-opus-4-8") {
		t.Errorf("action does not name the alternative: %q", lever.Action)
	}
	// Anthropic publishes no per-model weighting and this machine could not derive
	// one, so a saving figure here would be fabricated.
	if lever.TokensSaved != 0 || lever.WindowExtension != 0 {
		t.Errorf("claimed a saving of %v tokens for an underivable weighting", lever.TokensSaved)
	}
	if !strings.Contains(lever.Action, "no se inventa") {
		t.Errorf("action does not say the number is unavailable: %q", lever.Action)
	}
}

func TestLevers_AreOrderedByHowMuchQuotaTheyTouch(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var records []aggregate.Record
	records = append(records, usage(aggregate.AgentOpenClaw, openClawWorkspace, "chat", "claude-opus-5", 500, 300_000, now)...)
	records = append(records, usage(aggregate.AgentClaudeCode, ccWorkspace, "a", "claude-opus-4-8", 300, 100_000, now)...)
	records = append(records, usage(aggregate.AgentClaudeCode, repoWorkspace, "b", "claude-opus-4-8", 200, 100_000, now)...)

	got := levers(breakdownFrom(records), nil)
	if len(got) < 2 {
		t.Fatalf("levers = %d, want at least 2 on this fleet", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].QuotaShare < got[i].QuotaShare {
			t.Fatalf("levers are not ordered by share: %+v", got)
		}
	}
}

func TestExtension_IsZeroWithoutAMeasuredRate(t *testing.T) {
	if got := extension(1_000_000, Burn{}); got != 0 {
		t.Errorf("extension = %s, want 0: there is no rate to convert with", got)
	}
}

func TestWorkspaceName_ShortensWithoutLosingWhatDistinguishes(t *testing.T) {
	cases := map[string]string{
		"/home/user/.openclaw/workspace":             ".openclaw/workspace",
		"/home/user/Develop/llm-agent-spend-manager": "Develop/llm-agent-spend-manager",
		"/home/user": "/home/user",
		"":           "(sin directorio)",
	}
	for in, want := range cases {
		if got := WorkspaceName(in); got != want {
			t.Errorf("WorkspaceName(%q) = %q, want %q", in, got, want)
		}
	}
}
