package claudecode

import "testing"

func TestEstimateCostUSD_KnownModel(t *testing.T) {
	e := Entry{
		Model:                    "claude-opus-4-8",
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheCreationInputTokens: 0,
		CacheReadInputTokens:     0,
	}

	cost, known := EstimateCostUSD(e)
	if !known {
		t.Fatal("known = false, want true for claude-opus-4-8")
	}
	// 1M input @ $5/MTok + 1M output @ $25/MTok = $30 exactly.
	if got, want := cost, 30.0; got != want {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestEstimateCostUSD_StripsDatedSnapshotSuffix(t *testing.T) {
	e := Entry{Model: "claude-haiku-4-5-20251001", InputTokens: 1_000_000}

	cost, known := EstimateCostUSD(e)
	if !known {
		t.Fatal("known = false, want true — the dated suffix should fall back to the base model's rate")
	}
	if got, want := cost, 1.0; got != want {
		t.Errorf("cost = %v, want %v ($1/MTok for claude-haiku-4-5)", got, want)
	}
}

func TestEstimateCostUSD_UnknownModelReturnsNotKnown(t *testing.T) {
	e := Entry{Model: "some-future-model-not-in-the-table", InputTokens: 1000}

	cost, known := EstimateCostUSD(e)
	if known {
		t.Fatal("known = true, want false for an unmapped model")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 when the model is unknown", cost)
	}
}

func TestEstimateCostUSD_CacheTokensAreBilledAtTheirOwnRates(t *testing.T) {
	e := Entry{
		Model:                    "claude-haiku-4-5",
		CacheCreationInputTokens: 1_000_000,
		CacheReadInputTokens:     1_000_000,
	}

	cost, known := EstimateCostUSD(e)
	if !known {
		t.Fatal("known = false, want true")
	}
	// 1M cache-write(5m) @ $1.25/MTok + 1M cache-read @ $0.10/MTok = $1.35.
	if got, want := cost, 1.35; got != want {
		t.Errorf("cost = %v, want %v", got, want)
	}
}
