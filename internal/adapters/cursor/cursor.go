// Package cursor reads Cursor CLI's on-disk data to produce activity-tier usage
// estimates for the Cursor agent. Cursor records no tokens and no
// cost, so unlike the claudecode/openclaw adapters
// this one cannot measure — it estimates, and every figure it emits is a range
// labeled "actividad estimada", a confidence tier below the measured agents.
//
// Two sources, joined by conversationId — but neither one owns the list of
// conversations, because either can be missing:
//   - ~/.cursor/ai-tracking/ai-code-tracking.db, table ai_code_hashes: one row
//     per AI-authored code hash, with the model and timestamp. This is the
//     Camino B activity signal (how much code the agent produced). Cursor
//     creates this DB only once it writes code, so an agent running --mode ask
//     (reads and answers, never edits) leaves none of it behind.
//   - ~/.cursor/chats/<workspace>/<conversationId>/store.db, table blobs: the
//     real conversation content (messages, tool results, system prompt) as
//     stable JSON/text. Counting it is the Camino A floor (the text that truly
//     passed through the model).
//
// Camino A is the floor under Camino B where both exist, and stands on its own
// where Camino B doesn't: a conversation that has only a store.db still yields
// an estimate, with zero code hashes and an unknown model. Anchoring the join
// on the code hashes made the adapter report nothing at all for an ask-mode
// agent whose conversations were all sitting on disk (T100).
//
// All DBs are opened read-only (see internal/sqliteutil): this tool never writes
// to Cursor's live files.
package cursor

import (
	"path/filepath"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/pricing"
)

// Entry is one Cursor conversation's activity estimate. It is the adapter's unit
// of output: a token range plus the dominant model, ready to be normalized into
// an activity-tier record.
type Entry struct {
	ConversationID string
	Model          string
	LastActivity   time.Time
	CodeHashes     int
	TokensLow      int
	TokensHigh     int
}

// conversation is the internal accumulator while joining the two sources.
type conversation struct {
	id            string
	model         string
	lastActivity  time.Time
	codeHashes    int
	visibleTokens int // Camino A floor, 0 until a store.db is read
}

// TrackingDBPath returns the Cursor code-tracking DB path
// (~/.cursor/ai-tracking/ai-code-tracking.db).
func TrackingDBPath(homeDir string) string {
	return filepath.Join(homeDir, ".cursor", "ai-tracking", "ai-code-tracking.db")
}

// ChatsDir returns the Cursor per-conversation chats directory
// (~/.cursor/chats).
func ChatsDir(homeDir string) string {
	return filepath.Join(homeDir, ".cursor", "chats")
}

// CollectUsage returns one activity estimate per Cursor conversation, or no
// entries if Cursor isn't installed / has no data (not an error, mirroring the
// other adapters). homeDir is taken as a parameter so tests can point at a
// fixture tree.
func CollectUsage(homeDir string) ([]Entry, error) {
	// Camino B first, when it exists: only the code hashes carry a model.
	convs, err := collectActivity(TrackingDBPath(homeDir))
	if err != nil {
		return nil, err
	}
	if convs == nil {
		convs = map[string]*conversation{}
	}
	// Camino A completes the set: a conversation with a store.db of its own is
	// real activity whether or not it ever produced code.
	addConversationsFromChats(convs, ChatsDir(homeDir))
	if len(convs) == 0 {
		return nil, nil
	}

	addVisibleTokens(convs, ChatsDir(homeDir))

	ordered := sortedConversations(convs)
	tokensPerCodeHash := deriveTokensPerCodeHash(ordered)

	entries := make([]Entry, 0, len(ordered))
	for _, c := range ordered {
		low, high := estimateRange(c, tokensPerCodeHash)
		if low == 0 && high == 0 {
			continue // no signal at all for this conversation
		}
		entries = append(entries, Entry{
			ConversationID: c.id,
			Model:          c.model,
			LastActivity:   c.lastActivity,
			CodeHashes:     c.codeHashes,
			TokensLow:      low,
			TokensHigh:     high,
		})
	}
	return entries, nil
}

// EstimateCostRange values a Cursor estimate: the low token bound at the model's
// input rate and the high bound at its output rate (Cursor gives no
// input/output split, so this brackets the cost honestly). known is false for
// models absent from the pricing table (e.g. Cursor's own composer-*), where the
// UI should show tokens without a dollar figure.
func EstimateCostRange(e Entry) (low, high float64, known bool) {
	lowRate, highRate, known := pricing.PerTokenBounds(e.Model)
	if !known {
		return 0, 0, false
	}
	return float64(e.TokensLow) * lowRate, float64(e.TokensHigh) * highRate, true
}
