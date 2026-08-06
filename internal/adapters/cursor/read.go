package cursor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/sqliteutil"
	"github.com/Rentheria/llm-agent-spend-manager/internal/tokenize"
)

// maxRowsPerDB caps how many ai_code_hashes rows are read from one tracking DB,
// bounding the convs/modelVotes maps against a pathologically large or corrupt
// DB (L-02). It's a var, not a const, so tests can lower it.
var maxRowsPerDB = 2_000_000

// maxBlobBytes bounds one blob read out of store.db. The 2026-07-26 review noted
// that reading LENGTH(data) instead of the blob kept a huge row from ever landing
// in memory; A2 has to read the bytes to tokenize them, so the bound moves into
// the query (SQLite never materializes what the WHERE excludes). The largest real
// blob on this machine is 221 KB, so this is ~75× headroom and only fires on a
// corrupt or hostile DB — where losing that conversation's floor is the safe
// outcome, since it drops to the visible code-hash path.
const maxBlobBytes = 16 << 20

// collectActivity reads ai_code_hashes and rolls it up per conversation: hash
// count, dominant model, and most-recent activity. A missing DB yields no
// conversations (Cursor not installed), not an error.
func collectActivity(dbPath string) (map[string]*conversation, error) {
	db, err := sqliteutil.OpenReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil
	}
	defer db.Close()

	rows, err := db.Query(`SELECT conversationId, model, timestamp FROM ai_code_hashes`)
	if err != nil {
		return nil, nil // older/absent schema shouldn't blank the whole fleet
	}
	defer rows.Close()

	convs := map[string]*conversation{}
	modelVotes := map[string]map[string]int{} // conversationId -> model -> count
	rowsScanned := 0
	for rows.Next() {
		if rowsScanned >= maxRowsPerDB {
			fmt.Fprintf(os.Stderr, "cursor: %s excede %d filas; se trunca (¿DB enorme o corrupta?)\n", dbPath, maxRowsPerDB)
			break
		}
		rowsScanned++
		var (
			convID string
			model  *string
			tsMs   *int64
		)
		if err := rows.Scan(&convID, &model, &tsMs); err != nil {
			continue
		}
		if convID == "" {
			continue
		}
		c := convs[convID]
		if c == nil {
			c = &conversation{id: convID}
			convs[convID] = c
			modelVotes[convID] = map[string]int{}
		}
		c.codeHashes++
		if isMeaningfulModel(model) {
			modelVotes[convID][*model]++
		}
		if tsMs != nil {
			if t := time.UnixMilli(*tsMs); t.After(c.lastActivity) {
				c.lastActivity = t
			}
		}
	}
	for id, c := range convs {
		c.model = dominantModel(modelVotes[id])
	}
	return convs, rows.Err()
}

// isMeaningfulModel rejects null and the placeholder "default" so they don't win
// the dominant-model vote over a real model name.
func isMeaningfulModel(model *string) bool {
	return model != nil && *model != "" && *model != "default"
}

// dominantModel returns the most-voted model, breaking ties by name for stable
// output. Empty when a conversation has no meaningful model (cost stays unknown).
func dominantModel(votes map[string]int) string {
	best := ""
	bestCount := 0
	for model, count := range votes {
		if count > bestCount || (count == bestCount && model < best) {
			best, bestCount = model, count
		}
	}
	return best
}

// addConversationsFromChats adds every conversation that has a store.db on disk
// and wasn't already found in the code-hash DB, so Camino A doesn't depend on
// Camino B having seen the conversation first (T100: an agent in --mode ask
// writes no code, so its conversations exist ONLY here). Conversations already
// in convs are left untouched — the code-hash rows carry a model and a
// timestamp, and those beat what can be read off the filesystem.
func addConversationsFromChats(convs map[string]*conversation, chatsDir string) {
	stores, err := filepath.Glob(filepath.Join(chatsDir, "*", "*", "store.db"))
	if err != nil {
		return
	}
	for _, storePath := range stores {
		id := filepath.Base(filepath.Dir(storePath))
		if !isValidConversationID(id) || convs[id] != nil {
			continue
		}
		// The model stays empty on purpose: store.db records the turns but not
		// which model served them, and a wrong model would be priced wrongly.
		// Unknown here means the entry renders as tokens without a dollar
		// figure, which is the honest outcome.
		convs[id] = &conversation{id: id, lastActivity: lastWriteTime(storePath)}
	}
}

// lastWriteTime dates a conversation by when Cursor last wrote its store, the
// only activity timestamp available without the code-hash rows. Zero when the
// file can't be stat'd, which drops the conversation out of time-windowed
// views rather than inventing a date for it.
func lastWriteTime(storePath string) time.Time {
	info, err := os.Stat(storePath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// addVisibleTokens fills each conversation's Camino A floor by tokenizing the
// real text stored in its store.db. Conversations without a readable store keep
// visibleTokens == 0 and fall back to the Camino B estimate.
func addVisibleTokens(convs map[string]*conversation, chatsDir string) {
	for id, c := range convs {
		c.visibleTokens = storedTextTokens(chatsDir, id)
	}
}

// storedTextTokens tokenizes the text a conversation's store.db proves went to
// the model. Returns 0 if no store.db exists or it can't be read.
func storedTextTokens(chatsDir, conversationID string) int {
	// The ID comes from Cursor's local DB, not an attacker, and store.db is
	// opened read-only — but interpolating it into a glob pattern is still a
	// path-traversal / glob-injection footgun, so validate it first (L-03).
	if !isValidConversationID(conversationID) {
		return 0
	}
	matches, err := filepath.Glob(filepath.Join(chatsDir, "*", conversationID, "store.db"))
	if err != nil || len(matches) == 0 {
		return 0
	}

	db, err := sqliteutil.OpenReadOnly(matches[0])
	if err != nil || db == nil {
		return 0
	}
	defer db.Close()

	rows, err := db.Query(`SELECT data FROM blobs WHERE LENGTH(data) <= ?`, maxBlobBytes)
	if err != nil {
		return 0
	}
	defer rows.Close()

	total := 0
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			continue
		}
		total += tokenize.Count(modelVisibleText(data))
	}
	return total
}

// isValidConversationID reports whether id is safe to interpolate into a path
// glob. Real Cursor conversation IDs are UUID-ish slugs, so we allow only
// [A-Za-z0-9._-] and reject empty IDs and any "..". This blocks path separators
// (/ and \), traversal, and glob metacharacters (* ? [ ]) in one shot — none of
// which are in the whitelist — so a malformed/hostile ID can neither escape
// ~/.cursor/chats nor make the glob match somewhere unintended.
func isValidConversationID(id string) bool {
	if id == "" || strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// sortedConversations returns the conversations newest-activity first (stable by
// id on ties), so estimates render in a predictable order.
func sortedConversations(convs map[string]*conversation) []conversation {
	out := make([]conversation, 0, len(convs))
	for _, c := range convs {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].lastActivity.Equal(out[j].lastActivity) {
			return out[i].lastActivity.After(out[j].lastActivity)
		}
		return out[i].id < out[j].id
	})
	return out
}
