package antigravity

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/sqliteutil"
	"github.com/Rentheria/llm-agent-spend-manager/internal/tokenize"
)

// maxBlobBytes bounds one gen_metadata blob read out of a conversation DB.
// The largest real blob on this machine is 1.1 MB, so this is ~15× headroom and
// only fires on a corrupt or hostile DB — where losing that conversation's floor
// is the safe outcome, since it drops to the generations fallback.
const maxBlobBytes = 16 << 20

// conversation is the internal accumulator for one conversation DB.
type conversation struct {
	id           string
	lastActivity time.Time
	steps        int
	generations  int
	// floorTokens is the Camino A measurement: the tokenized text the store
	// proves went to the model. 0 when the blob could not be decoded, which
	// sends the estimate back to the Camino B count (estimate.go).
	floorTokens int
	// model is empty when no row named one, which leaves the cost unknown
	// rather than guessed.
	model string
}

// collectConversations reads every <uuid>.db under dir and returns one
// conversation each, newest-activity first. A missing dir yields no
// conversations (Antigravity not installed), not an error.
func collectConversations(dir string) ([]conversation, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return nil, err
	}

	convs := make([]conversation, 0, len(matches))
	for _, path := range matches {
		conv, ok := readConversation(path)
		if !ok {
			continue
		}
		conv.id = strings.TrimSuffix(filepath.Base(path), ".db")
		conv.lastActivity = modTime(path)
		convs = append(convs, conv)
	}

	sort.Slice(convs, func(i, j int) bool {
		if !convs[i].lastActivity.Equal(convs[j].lastActivity) {
			return convs[i].lastActivity.After(convs[j].lastActivity)
		}
		return convs[i].id < convs[j].id
	})
	return convs, nil
}

// readConversation reads every signal one conversation DB offers: its
// trajectory steps, its model generations, the measured token floor, and the
// model that served it. ok is false when the file can't be opened or lacks a
// steps table (a partial or foreign DB), so the caller skips it rather than
// counting a phantom.
//
// Everything comes from one read-only open so it all describes the same snapshot
// of a live file.
func readConversation(path string) (conv conversation, ok bool) {
	db, err := sqliteutil.OpenReadOnly(path)
	if err != nil || db == nil {
		return conversation{}, false
	}
	defer db.Close()

	if err := db.QueryRow(`SELECT COUNT(*) FROM steps`).Scan(&conv.steps); err != nil {
		return conversation{}, false
	}
	conv.generations = generationCount(db)
	conv.floorTokens, conv.model = decodeGenMetadata(db)
	return conv, true
}

// generationCount counts the rows of gen_metadata, one per real model
// generation: each row is one request to the backing model (in Claude-backed
// conversations each carries its own req_vrtx_… Vertex request ID).
//
// A DB without the table (older or foreign format) yields 0, not an error: the
// conversation still counts, estimated from its steps instead.
func generationCount(db *sql.DB) int {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM gen_metadata`).Scan(&count); err != nil {
		return 0
	}
	return count
}

// decodeGenMetadata is the Camino A read: it decodes the stored protobuf blobs
// and returns the conversation's measured token floor and its model. Both are
// zero-valued when nothing decodes — a DB with no gen_metadata table, blobs in a
// format this decoder no longer recognizes, or fixtures that store no blob at
// all — which is the signal to fall back to Camino B rather than report a zero.
//
// It takes the LARGEST row's text, not the sum. Only one row per conversation
// carries the message history (12/12 conversations); the rest are ~1 KB stubs
// that decode to "". That one row holds the FINAL history, which already
// contains every earlier turn, so summing rows would re-count the whole
// conversation once per generation.
func decodeGenMetadata(db *sql.DB) (floorTokens int, model string) {
	rows, err := db.Query(`SELECT data FROM gen_metadata WHERE LENGTH(data) <= ?`, maxBlobBytes)
	if err != nil {
		return 0, ""
	}
	defer rows.Close()

	votes := map[string]int{}
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			continue
		}
		if id := blobModelID(blob); id != "" {
			votes[id]++
		}
		if tokens := tokenize.Count(modelVisibleText(blob)); tokens > floorTokens {
			floorTokens = tokens
		}
	}
	if rows.Err() != nil {
		return 0, ""
	}
	return floorTokens, dominantModel(votes)
}

// dominantModel returns the most-voted model, breaking ties by name for stable
// output. Empty when no row named one, which leaves the cost unknown.
//
// The vote exists because the row carrying the conversation payload does not
// always carry the model id — one of the 12 conversations had none on that row —
// so reading the model off the payload row alone would silently lose it.
func dominantModel(votes map[string]int) string {
	best, bestCount := "", 0
	for model, count := range votes {
		if count > bestCount || (count == bestCount && model < best) {
			best, bestCount = model, count
		}
	}
	return best
}

// modTime is the file's last-modified time, used as a proxy for the
// conversation's last activity. The steps carry their own timestamps inside the
// undocumented protobuf, but a field number recovered by wire-format recon is a
// weaker guarantee than the filesystem's own mtime, so the mtime stays
// authoritative here.
func modTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
