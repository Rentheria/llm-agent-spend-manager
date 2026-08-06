// Package antigravity reads Antigravity CLI's on-disk data to produce
// activity-tier usage estimates for the Antigravity agent.
// Antigravity reports no tokens and no cost anywhere on disk, so every figure
// here is an estimated range labeled "actividad estimada", never real spend.
//
// Source: ~/.gemini/antigravity-cli/conversations/<uuid>.db — table gen_metadata
// (one row per real model generation) and table steps (one row per trajectory
// step). As of 2026-07-31 (T11) the adapter reads two things from those tables:
//
//   - Camino A, the measured floor: gen_metadata's protobuf blob is decoded by
//     wire format (blob.go) and the text it proves went to the model is
//     tokenized for real. Antigravity publishes no .proto, so those field
//     numbers are recon findings that can break on any release — when they do,
//     the decode yields nothing and the estimate degrades to the counts below
//     rather than reporting a zero (docs/calibracion.md §Antigravity).
//   - Camino B, the fallback: plain row counts of generations, then steps.
//
// The same decode recovers the per-conversation model, so records now carry a
// model and can be priced. That dollar figure is RELATIVE WEIGHT, not a charge:
// Antigravity runs free under the shared subscription. Models absent from the pricing
// table (Antigravity's Gemini aliases) stay unpriced — tokens only.
//
// DBs are opened read-only (internal/sqliteutil): this tool never writes to
// Antigravity's live files.
package antigravity

import (
	"path/filepath"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
)

// Entry is one Antigravity conversation's activity estimate. Steps and
// Generations are both reported: Generations is what the fallback estimate is
// built from, Steps is kept because it is the last-resort unit and what earlier
// reports showed.
type Entry struct {
	ConversationID string
	LastActivity   time.Time
	Steps          int
	Generations    int
	TokensLow      int
	TokensHigh     int
	// Model is empty when no gen_metadata row named one, which leaves the cost
	// unknown rather than guessed.
	Model string
	// FloorMeasured reports whether TokensLow is a Camino A measurement of real
	// stored text (true) or a Camino B activity count × band (false). No surface
	// renders it yet — it is exposed because the difference is the whole point of
	// T11 and a caller that shows a range must be able to say which of the two it
	// is showing.
	FloorMeasured bool
}

// EstimateCostRange converts an entry's token range into a dollar range using
// the model's published per-token rates.
//
// known is false — and the caller must show tokens without a dollar figure —
// whenever the model is unknown or absent from the pricing table. That is the
// normal case for Antigravity's Gemini aliases: names like "gemini-3-flash-a"
// are Antigravity's own internal labels, not Google API model ids, so mapping
// one to a published price would be a guess. Claude-backed conversations, whose
// ids do map to Anthropic's published rates, do get priced.
//
// ⚠️ The result is RELATIVE WEIGHT / opportunity cost, not billed spend: Antigravity runs
// free under the shared subscription. Surfaces must say so.
func EstimateCostRange(e Entry) (low, high float64, known bool) {
	lowRate, highRate, known := pricing.PerTokenBounds(e.Model)
	if !known {
		return 0, 0, false
	}
	return float64(e.TokensLow) * lowRate, float64(e.TokensHigh) * highRate, true
}

// ConversationsDir returns the Antigravity per-conversation directory
// (~/.gemini/antigravity-cli/conversations).
func ConversationsDir(homeDir string) string {
	return filepath.Join(homeDir, ".gemini", "antigravity-cli", "conversations")
}

// CollectUsage returns one activity estimate per Antigravity conversation, or no
// entries when Antigravity isn't installed / has no conversations (not an
// error). homeDir is a parameter so tests can point at a fixture tree.
func CollectUsage(homeDir string) ([]Entry, error) {
	convs, err := collectConversations(ConversationsDir(homeDir))
	if err != nil {
		return nil, err
	}

	// Derived from this machine's own conversations, so a machine unlike the
	// calibration sample gets its own factor (see deriveTokensPerGeneration).
	tokensPerGeneration := deriveTokensPerGeneration(convs)

	entries := make([]Entry, 0, len(convs))
	for _, c := range convs {
		if c.steps == 0 && c.generations == 0 {
			continue
		}
		low, high := estimateRange(c, tokensPerGeneration)
		entries = append(entries, Entry{
			ConversationID: c.id,
			LastActivity:   c.lastActivity,
			Steps:          c.steps,
			Generations:    c.generations,
			TokensLow:      low,
			TokensHigh:     high,
			Model:          c.model,
			FloorMeasured:  c.floorTokens > 0,
		})
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return entries, nil
}
