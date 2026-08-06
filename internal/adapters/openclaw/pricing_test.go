package openclaw

import "testing"

func TestEstimateCostUSD_KnownModelUsesFleetTable(t *testing.T) {
	e := Entry{
		Model:        "claude-opus-4-8",
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
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

func TestEstimateCostUSD_CacheTokensMapToTheirBuckets(t *testing.T) {
	// An OpenClaw cacheWrite lands in CacheCreationInputTokens and a cacheRead
	// in CacheReadInputTokens; each is billed at its own rate.
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
