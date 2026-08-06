package antigravity

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/tokenize"
	_ "modernc.org/sqlite"
)

// conversationSpec describes a fixture conversation DB to write to disk.
// omitGenTable reproduces an older/foreign DB that has no gen_metadata at all,
// which is the case the steps fallback exists for. blob, when set, goes in the
// LAST gen_metadata row, matching the real store where only one row carries the
// conversation payload and the rest are stubs.
type conversationSpec struct {
	id           string
	steps        int
	generations  int
	omitGenTable bool
	blob         []byte
	mtime        time.Time
}

// writeConversationDB creates a conversation <id>.db shaped like Antigravity's:
// a steps table with one row per trajectory step, and (unless omitted) a
// gen_metadata table with one row per real model generation. It stamps the
// file's mtime so ordering is deterministic.
func writeConversationDB(t *testing.T, dir string, spec conversationSpec) {
	t.Helper()
	path := filepath.Join(dir, spec.id+".db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	fillTable(t, db, `CREATE TABLE steps (idx INTEGER)`, `INSERT INTO steps (idx) VALUES (?)`, spec.steps)
	if !spec.omitGenTable {
		fillGenMetadata(t, db, spec)
	}
	db.Close()
	if err := os.Chtimes(path, spec.mtime, spec.mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func fillGenMetadata(t *testing.T, db *sql.DB, spec conversationSpec) {
	t.Helper()
	const create = `CREATE TABLE gen_metadata (idx INTEGER, data BLOB, size INTEGER NOT NULL DEFAULT 0)`
	if _, err := db.Exec(create); err != nil {
		t.Fatalf("%s: %v", create, err)
	}
	for i := 0; i < spec.generations; i++ {
		var blob []byte
		if i == spec.generations-1 {
			blob = spec.blob // only the last row carries the payload
		}
		if _, err := db.Exec(`INSERT INTO gen_metadata (idx, data, size) VALUES (?, ?, 0)`, i, blob); err != nil {
			t.Fatalf("insert gen_metadata: %v", err)
		}
	}
}

func fillTable(t *testing.T, db *sql.DB, createSQL, insertSQL string, rows int) {
	t.Helper()
	if _, err := db.Exec(createSQL); err != nil {
		t.Fatalf("%s: %v", createSQL, err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(insertSQL, i); err != nil {
			t.Fatalf("%s: %v", insertSQL, err)
		}
	}
}

func convDir(t *testing.T, home string) string {
	t.Helper()
	dir := ConversationsDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// --- Camino A: the measured floor -------------------------------------------

func TestCollectUsage_MeasuresFloorFromStoredText(t *testing.T) {
	home := t.TempDir()
	dir := convDir(t, home)
	const (
		systemPrompt = "You are a coding agent. Follow the user's instructions."
		userText     = "Add a test for the estimator."
		model        = "claude-opus-4-6-thinking"
	)
	writeConversationDB(t, dir, conversationSpec{
		id: "conv", steps: 12, generations: 4, mtime: time.Now(),
		blob: genMetadataBlob(model, systemPrompt, userText),
	})

	entries, err := CollectUsage(home)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	got := entries[0]

	if !got.FloorMeasured {
		t.Error("FloorMeasured = false, want true — the blob decodes, so this is Camino A")
	}
	if got.Model != model {
		t.Errorf("Model = %q, want %q", got.Model, model)
	}
	wantLow := tokenize.Count(systemPrompt + userText)
	if got.TokensLow != wantLow {
		t.Errorf("TokensLow = %d, want %d (tokenized stored text)", got.TokensLow, wantLow)
	}
	if got.TokensHigh != int(float64(wantLow)*(1+invisibleHeadroom)) {
		t.Errorf("TokensHigh = %d, want floor × %.0f", got.TokensHigh, 1+invisibleHeadroom)
	}
}

// The floor must come from the ONE row that holds the payload, not the sum of
// all rows: that row already contains every earlier turn, so summing would
// re-count the whole conversation once per generation.
func TestCollectUsage_FloorTakesLargestRowNotSum(t *testing.T) {
	home := t.TempDir()
	dir := convDir(t, home)
	const text = "one shared conversation history"
	writeConversationDB(t, dir, conversationSpec{
		id: "conv", steps: 9, generations: 5, mtime: time.Now(),
		blob: genMetadataBlob("claude-opus-4-6", "", text),
	})

	entries, err := CollectUsage(home)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	want := tokenize.Count(text)
	if entries[0].TokensLow != want {
		t.Errorf("TokensLow = %d, want %d — one row's text, not 5 rows summed",
			entries[0].TokensLow, want)
	}
}

// The model is voted across ALL rows because the payload row does not always
// name one (one of the 12 real conversations had none on that row).
func TestCollectUsage_ResolvesModelFromStubRowWhenPayloadRowHasNone(t *testing.T) {
	home := t.TempDir()
	dir := convDir(t, home)
	path := filepath.Join(dir, "conv.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	fillTable(t, db, `CREATE TABLE steps (idx INTEGER)`, `INSERT INTO steps (idx) VALUES (?)`, 3)
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER, data BLOB)`); err != nil {
		t.Fatal(err)
	}
	// Row 0: a stub naming the model but carrying no messages.
	// Row 1: the payload, with no model id at all.
	stub := field(fieldRequest, concat(field(fieldModelID, []byte("claude-opus-5"))))
	payload := field(fieldRequest, concat(
		field(fieldSystemPrompt, []byte("system")),
		field(fieldMessage, field(fieldMessageText, []byte("hello"))),
	))
	for i, blob := range [][]byte{stub, payload} {
		if _, err := db.Exec(`INSERT INTO gen_metadata (idx, data) VALUES (?, ?)`, i, blob); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	entries, err := CollectUsage(home)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if entries[0].Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5 voted from the stub row", entries[0].Model)
	}
	if entries[0].TokensLow != tokenize.Count("systemhello") {
		t.Errorf("TokensLow = %d, want the payload row's text", entries[0].TokensLow)
	}
}

// --- Camino B: the fallbacks ------------------------------------------------

// A blob that doesn't decode (here: none stored at all) must fall back to the
// generation count, never to a zero that would erase real activity.
func TestCollectUsage_FallsBackToGenerationsWhenBlobUndecodable(t *testing.T) {
	home := t.TempDir()
	dir := convDir(t, home)
	older := time.Date(2026, 6, 23, 17, 25, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 9, 18, 39, 0, 0, time.UTC)
	writeConversationDB(t, dir, conversationSpec{id: "conv-old", steps: 4, generations: 1, mtime: older})
	writeConversationDB(t, dir, conversationSpec{id: "conv-new", steps: 38, generations: 10, mtime: newer})

	entries, err := CollectUsage(home)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}

	first := entries[0] // newest activity first
	if first.ConversationID != "conv-new" {
		t.Errorf("first = %q, want conv-new (most recent mtime)", first.ConversationID)
	}
	if first.FloorMeasured {
		t.Error("FloorMeasured = true, want false — nothing decoded, so this is Camino B")
	}
	if first.Steps != 38 || first.Generations != 10 {
		t.Errorf("(Steps, Generations) = (%d, %d), want (38, 10)", first.Steps, first.Generations)
	}
	wantLow := 10 * defaultTokensPerGeneration
	if first.TokensLow != wantLow {
		t.Errorf("TokensLow = %d, want %d — generations × the default factor", first.TokensLow, wantLow)
	}
	if !first.LastActivity.Equal(newer) {
		t.Errorf("LastActivity = %v, want %v (file mtime)", first.LastActivity, newer)
	}
}

// A DB with no gen_metadata table falls back to the steps floor — never to zero.
func TestCollectUsage_FallsBackToStepsWithoutGenMetadata(t *testing.T) {
	home := t.TempDir()
	dir := convDir(t, home)
	const steps = 7
	writeConversationDB(t, dir, conversationSpec{
		id: "legacy", steps: steps, omitGenTable: true, mtime: time.Now(),
	})

	entries, err := CollectUsage(home)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1 (a DB without gen_metadata still counts)", len(entries))
	}
	got := entries[0]
	if got.Generations != 0 {
		t.Errorf("Generations = %d, want 0 (no gen_metadata table)", got.Generations)
	}
	if got.TokensLow != steps*tokensPerStepFloor {
		t.Errorf("TokensLow = %d, want the steps fallback %d", got.TokensLow, steps*tokensPerStepFloor)
	}
}

func TestCollectUsage_SkipsDBWithoutStepsTable(t *testing.T) {
	home := t.TempDir()
	dir := convDir(t, home)
	// A foreign/partial DB with no steps table must be skipped, not counted.
	other, err := sql.Open("sqlite", filepath.Join(dir, "foreign.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`CREATE TABLE something_else (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	other.Close()
	writeConversationDB(t, dir, conversationSpec{id: "real", steps: 3, generations: 1, mtime: time.Now()})

	entries, err := CollectUsage(home)
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if len(entries) != 1 || entries[0].ConversationID != "real" {
		t.Fatalf("entries = %+v, want only the real conversation", entries)
	}
}

func TestCollectUsage_MissingDirReturnsEmpty(t *testing.T) {
	entries, err := CollectUsage(t.TempDir())
	if err != nil {
		t.Fatalf("CollectUsage: %v, want nil error when Antigravity is absent", err)
	}
	if entries != nil {
		t.Fatalf("entries = %v, want nil", entries)
	}
}

// --- estimateRange and calibration ------------------------------------------

func TestEstimateRange_PrefersMeasuredFloorOverActivityCount(t *testing.T) {
	c := conversation{steps: 20, generations: 5, floorTokens: 9_000}
	low, high := estimateRange(c, defaultTokensPerGeneration)
	if low != 9_000 {
		t.Errorf("low = %d, want the measured floor 9000, not the generation count", low)
	}
	if high != 27_000 {
		t.Errorf("high = %d, want floor × 3", high)
	}
}

func TestEstimateRange_DegradesInDocumentedOrder(t *testing.T) {
	tests := []struct {
		name    string
		conv    conversation
		wantLow int
	}{
		{"no floor uses generations", conversation{steps: 20, generations: 5}, 5 * defaultTokensPerGeneration},
		{"no generations uses steps", conversation{steps: 5}, 5 * tokensPerStepFloor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			low, high := estimateRange(tt.conv, defaultTokensPerGeneration)
			if low != tt.wantLow {
				t.Errorf("low = %d, want %d", low, tt.wantLow)
			}
			if low >= high {
				t.Errorf("not a range: low=%d high=%d", low, high)
			}
		})
	}
}

// The factor is pooled Σ/Σ, not an average of per-conversation ratios: a
// 30-generation conversation must weigh 30× a 1-generation one.
func TestDeriveTokensPerGeneration_PoolsAcrossConversations(t *testing.T) {
	convs := []conversation{
		{floorTokens: 90_000, generations: 30},
		{floorTokens: 10_000, generations: 1},
	}
	got := deriveTokensPerGeneration(convs)
	if want := 100_000 / 31; got != want {
		t.Errorf("deriveTokensPerGeneration = %d, want %d (Σ tokens / Σ generations)", got, want)
	}
}

func TestDeriveTokensPerGeneration_FallsBackWhenNothingDecoded(t *testing.T) {
	convs := []conversation{{generations: 10}, {steps: 4}}
	if got := deriveTokensPerGeneration(convs); got != defaultTokensPerGeneration {
		t.Errorf("deriveTokensPerGeneration = %d, want the measured default %d",
			got, defaultTokensPerGeneration)
	}
}

// Pins the calibration constants to the pool measured on 2026-07-31 (T11), so a
// future edit has to be a deliberate re-measurement rather than a silent drift.
func TestCalibrationConstantsMatchMeasuredPool(t *testing.T) {
	const (
		pooledFloorTokens = 467_307
		pooledGenerations = 112
		pooledSteps       = 322
	)
	if want := pooledFloorTokens / pooledGenerations; defaultTokensPerGeneration != want {
		t.Errorf("defaultTokensPerGeneration = %d, want %d (Σ%d / Σ%d)",
			defaultTokensPerGeneration, want, pooledFloorTokens, pooledGenerations)
	}
	if want := pooledFloorTokens / pooledSteps; tokensPerStepFloor != want {
		t.Errorf("tokensPerStepFloor = %d, want %d (Σ%d / Σ%d)",
			tokensPerStepFloor, want, pooledFloorTokens, pooledSteps)
	}
	// The headline finding of T11: the band this replaced had a LOW of 2300,
	// which sat below the measured floor — so it was not a floor at all.
	if defaultTokensPerGeneration <= 2300 {
		t.Errorf("measured floor %d no longer exceeds the old invented low (2300); re-check the calibration",
			defaultTokensPerGeneration)
	}
}

// --- cost -------------------------------------------------------------------

func TestEstimateCostRange(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantKnown bool
	}{
		{"claude model is priced", "claude-opus-4-6-thinking", true},
		{"gemini alias is not a Google API model id, so it stays unpriced", "gemini-3-flash-a", false},
		{"no model at all", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			low, high, known := EstimateCostRange(Entry{
				Model: tt.model, TokensLow: 10_000, TokensHigh: 30_000,
			})
			if known != tt.wantKnown {
				t.Fatalf("known = %v, want %v", known, tt.wantKnown)
			}
			if !known {
				if low != 0 || high != 0 {
					t.Errorf("costs = (%v, %v), want zeros when unknown", low, high)
				}
				return
			}
			if low <= 0 || high <= low {
				t.Errorf("cost range = (%v, %v), want a positive widening range", low, high)
			}
		})
	}
}

func TestConversationsDir(t *testing.T) {
	want := filepath.Join("/home/user", ".gemini", "antigravity-cli", "conversations")
	if got := ConversationsDir("/home/user"); got != want {
		t.Errorf("ConversationsDir = %q, want %q", got, want)
	}
}
