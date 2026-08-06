package cursor

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// codeHash is one ai_code_hashes fixture row.
type codeHash struct {
	conversationID string
	model          any // string or nil
	timestampMs    int64
}

func writeTrackingDB(t *testing.T, home string, hashes []codeHash) {
	t.Helper()
	dir := filepath.Join(home, ".cursor", "ai-tracking")
	mustMkdirAll(t, dir)
	db := openWritable(t, filepath.Join(dir, "ai-code-tracking.db"))
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE ai_code_hashes (
		conversationId TEXT, model TEXT, timestamp INTEGER
	)`); err != nil {
		t.Fatalf("create ai_code_hashes: %v", err)
	}
	for _, h := range hashes {
		if _, err := db.Exec(
			`INSERT INTO ai_code_hashes (conversationId, model, timestamp) VALUES (?, ?, ?)`,
			h.conversationID, h.model, h.timestampMs,
		); err != nil {
			t.Fatalf("insert hash: %v", err)
		}
	}
}

// writeStoreDB creates a conversation's store.db under a workspace hash with the
// given blob byte payloads.
func writeStoreDB(t *testing.T, home, workspaceHash, conversationID string, blobs [][]byte) {
	t.Helper()
	dir := filepath.Join(home, ".cursor", "chats", workspaceHash, conversationID)
	mustMkdirAll(t, dir)
	db := openWritable(t, filepath.Join(dir, "store.db"))
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE blobs (id TEXT, data BLOB)`); err != nil {
		t.Fatalf("create blobs: %v", err)
	}
	for i, b := range blobs {
		if _, err := db.Exec(`INSERT INTO blobs (id, data) VALUES (?, ?)`, i, b); err != nil {
			t.Fatalf("insert blob: %v", err)
		}
	}
}

func TestCollectUsage_FloorFromBlobsBFallbackOtherwise(t *testing.T) {
	home := t.TempDir()
	writeTrackingDB(t, home, []codeHash{
		{conversationID: "conv-a", model: "claude-opus-4-8", timestampMs: 2000},
		{conversationID: "conv-a", model: "claude-opus-4-8", timestampMs: 3000},
		{conversationID: "conv-b", model: "composer-2.5", timestampMs: 1000},
		// null model must not win the dominant-model vote.
		{conversationID: "conv-a", model: nil, timestampMs: 1500},
	})
	// conv-a has a store.db → Camino A floor; conv-b has none → Camino B fallback.
	// The blobs mirror a real store: two message blobs plus the protobuf editor
	// state that makes up ~92% of the bytes on disk and is NOT prompt text.
	writeStoreDB(t, home, "ws1", "conv-a", [][]byte{
		[]byte(`{"role":"user","content":"cuenta hasta tres"}`),
		[]byte(`{"role":"assistant","content":[{"type":"text","text":"uno dos tres"}]}`),
		append([]byte{0x0a, 0x4e}, make([]byte, 4000)...), // protobuf editor state
	})

	entries, err := CollectUsage(home)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}

	byConv := map[string]Entry{}
	for _, e := range entries {
		byConv[e.ConversationID] = e
	}

	a := byConv["conv-a"]
	if a.Model != "claude-opus-4-8" {
		t.Errorf("conv-a model = %q, want claude-opus-4-8 (null vote ignored)", a.Model)
	}
	// The floor is the tokenized message text — and NOT the 4 KB of editor
	// state, which is why it stays small. A dozen tokens of Spanish is the right
	// order of magnitude; the exact count belongs to the tokenizer, not to this
	// test, so it asserts the band a wrong denominator would blow through.
	if a.TokensLow < 5 || a.TokensLow > 40 {
		t.Errorf("conv-a TokensLow = %d, want el piso del texto real (5..40); "+
			"un valor de miles significa que se está contando el estado del editor", a.TokensLow)
	}
	if a.TokensHigh <= a.TokensLow {
		t.Errorf("conv-a range not widened: low=%d high=%d", a.TokensLow, a.TokensHigh)
	}

	b := byConv["conv-b"]
	if b.Model != "composer-2.5" {
		t.Errorf("conv-b model = %q, want composer-2.5", b.Model)
	}
	if b.TokensLow == 0 {
		t.Error("conv-b TokensLow = 0, want a Camino B fallback estimate from code hashes")
	}

	// Newest activity first: conv-a (ts 3000) before conv-b (ts 1000).
	if entries[0].ConversationID != "conv-a" {
		t.Errorf("first entry = %q, want conv-a (most recent)", entries[0].ConversationID)
	}
}

// An agent in --mode ask answers without editing files, so Cursor never writes
// ai-code-tracking.db and every one of its conversations lives only as a
// store.db. Anchoring on the code hashes made the adapter report nothing at all
// for such an agent (T100), so both the missing DB and the empty one have to
// leave Camino A standing.
func TestCollectUsage_StoreWithoutCodeHashesStillCounts(t *testing.T) {
	cases := []struct {
		name           string
		writeTrackingA bool
	}{
		{name: "sin ai-code-tracking.db"},
		{name: "con ai-code-tracking.db vacia", writeTrackingA: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.writeTrackingA {
				writeTrackingDB(t, home, nil)
			}
			writeStoreDB(t, home, "ws1", "conv-ask", [][]byte{
				[]byte(`{"role":"user","content":"explica el repo sin tocar nada"}`),
				[]byte(`{"role":"assistant","content":[{"type":"text","text":"es un medidor de gasto"}]}`),
				append([]byte{0x0a, 0x4e}, make([]byte, 4000)...), // protobuf editor state
			})

			entries, err := CollectUsage(home)
			if err != nil {
				t.Fatalf("CollectUsage: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("len(entries) = %d, want 1: la conversación existe en disco "+
					"aunque no haya code hashes", len(entries))
			}

			e := entries[0]
			if e.ConversationID != "conv-ask" {
				t.Errorf("ConversationID = %q, want conv-ask", e.ConversationID)
			}
			if e.CodeHashes != 0 {
				t.Errorf("CodeHashes = %d, want 0", e.CodeHashes)
			}
			if e.Model != "" {
				t.Errorf("Model = %q, want vacío: store.db no registra qué modelo corrió", e.Model)
			}
			// Same floor as any Camino A conversation: the message text, not the
			// 4 KB of editor state, and never tokensPerCodeHash × 0 hashes.
			if e.TokensLow < 5 || e.TokensLow > 40 {
				t.Errorf("TokensLow = %d, want el piso del texto real (5..40)", e.TokensLow)
			}
			if e.TokensHigh <= e.TokensLow {
				t.Errorf("rango sin techo: low=%d high=%d", e.TokensLow, e.TokensHigh)
			}
			if e.LastActivity.IsZero() {
				t.Error("LastActivity en cero: la entrada se caería de toda ventana de tiempo")
			}
		})
	}
}

// The conversation set is the union of both sources, so one conversation having
// code hashes doesn't hide another that only exists as a store.
func TestCollectUsage_UnionOfCodeHashesAndStores(t *testing.T) {
	home := t.TempDir()
	writeTrackingDB(t, home, []codeHash{
		{conversationID: "conv-code", model: "claude-opus-4-8", timestampMs: 2000},
	})
	writeStoreDB(t, home, "ws1", "conv-code", [][]byte{
		[]byte(`{"role":"user","content":"arregla el parser"}`),
	})
	writeStoreDB(t, home, "ws2", "conv-ask", [][]byte{
		[]byte(`{"role":"user","content":"nada más explícame el parser"}`),
	})

	entries, err := CollectUsage(home)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	byConv := map[string]Entry{}
	for _, e := range entries {
		byConv[e.ConversationID] = e
	}
	if len(byConv) != 2 {
		t.Fatalf("conversaciones = %v, want conv-code y conv-ask", byConv)
	}
	if got := byConv["conv-code"]; got.Model != "claude-opus-4-8" || got.CodeHashes != 1 {
		t.Errorf("conv-code = %+v, want el modelo y el hash de Camino B intactos", got)
	}
	if got := byConv["conv-ask"]; got.CodeHashes != 0 || got.TokensLow == 0 {
		t.Errorf("conv-ask = %+v, want piso de Camino A con cero code hashes", got)
	}
}

func TestEstimateCostRange_KnownModelBracketsInputToOutput(t *testing.T) {
	e := Entry{Model: "claude-opus-4-8", TokensLow: 1_000_000, TokensHigh: 2_000_000}
	low, high, known := EstimateCostRange(e)
	if !known {
		t.Fatal("known = false, want true for a priced model")
	}
	// low = 1M × input ($5/M) = $5; high = 2M × output ($25/M) = $50.
	if low < 4.99 || low > 5.01 {
		t.Errorf("low = %v, want ~5 (input rate on low tokens)", low)
	}
	if high < 49.9 || high > 50.1 {
		t.Errorf("high = %v, want ~50 (output rate on high tokens)", high)
	}
}

func TestEstimateCostRange_UnknownModelHasNoCost(t *testing.T) {
	_, _, known := EstimateCostRange(Entry{Model: "composer-2.5", TokensLow: 100, TokensHigh: 200})
	if known {
		t.Error("known = true for composer-2.5, want false (Cursor's own model isn't priced)")
	}
}

func TestCollectUsage_MissingCursorReturnsEmpty(t *testing.T) {
	entries, err := CollectUsage(t.TempDir())
	if err != nil {
		t.Fatalf("CollectUsage: %v, want nil error when Cursor is absent", err)
	}
	if entries != nil {
		t.Fatalf("entries = %v, want nil", entries)
	}
}

func TestDeriveTokensPerCodeHash(t *testing.T) {
	// Two conversations with both signals: (3000 tokens / 2 hashes) and
	// (1000 / 2) → 4000 tokens / 4 hashes = 1000 per hash.
	convs := []conversation{
		{visibleTokens: 3000, codeHashes: 2},
		{visibleTokens: 1000, codeHashes: 2},
		{visibleTokens: 0, codeHashes: 5}, // no A data — excluded from calibration
	}
	if got := deriveTokensPerCodeHash(convs); got != 1000 {
		t.Errorf("deriveTokensPerCodeHash = %d, want 1000", got)
	}

	if got := deriveTokensPerCodeHash(nil); got != defaultTokensPerCodeHash {
		t.Errorf("empty input = %d, want default %d", got, defaultTokensPerCodeHash)
	}
}

func TestPaths(t *testing.T) {
	home := "/home/user"
	if got := TrackingDBPath(home); got != filepath.Join(home, ".cursor", "ai-tracking", "ai-code-tracking.db") {
		t.Errorf("TrackingDBPath = %q", got)
	}
	if got := ChatsDir(home); got != filepath.Join(home, ".cursor", "chats") {
		t.Errorf("ChatsDir = %q", got)
	}
}

func openWritable(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return db
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCollectActivity_CapsRowsPerDB(t *testing.T) {
	old := maxRowsPerDB
	maxRowsPerDB = 3
	defer func() { maxRowsPerDB = old }()

	home := t.TempDir()
	writeTrackingDB(t, home, []codeHash{
		{conversationID: "c1", model: "claude-opus-4-8", timestampMs: 1000},
		{conversationID: "c2", model: "claude-opus-4-8", timestampMs: 1000},
		{conversationID: "c3", model: "claude-opus-4-8", timestampMs: 1000},
		{conversationID: "c4", model: "claude-opus-4-8", timestampMs: 1000},
		{conversationID: "c5", model: "claude-opus-4-8", timestampMs: 1000},
		{conversationID: "c6", model: "claude-opus-4-8", timestampMs: 1000},
	})

	convs, err := collectActivity(TrackingDBPath(home))
	if err != nil {
		t.Fatalf("collectActivity: %v", err)
	}
	// Only the first maxRowsPerDB rows are scanned, so no more than that many
	// distinct conversations can materialize (bounded memory, not OOM).
	if len(convs) > 3 {
		t.Fatalf("len(convs) = %d, want <= 3 (rows capped)", len(convs))
	}
}

func TestIsValidConversationID(t *testing.T) {
	valid := []string{
		"conv-a", "conv-b",
		"9f8c2a1b-7d3e-4c5f-8a1b-2c3d4e5f6a7b", // UUID
		"composer_2.5-abc123",
	}
	for _, id := range valid {
		if !isValidConversationID(id) {
			t.Errorf("isValidConversationID(%q) = false, want true", id)
		}
	}
	invalid := []string{
		"",          // empty
		"..",        // traversal
		"../../etc", // traversal with separators
		"a/b",       // path separator
		`a\b`,       // windows separator
		"conv*",     // glob metachar
		"conv?",     // glob metachar
		"conv[a]",   // glob metachar
		"a..b",      // contains ..
		"conv id",   // space
	}
	for _, id := range invalid {
		if isValidConversationID(id) {
			t.Errorf("isValidConversationID(%q) = true, want false", id)
		}
	}
}
