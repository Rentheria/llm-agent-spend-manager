package cursor

// Estimation constants for Cursor's activity-only data. Cursor exposes no token
// counts (docs/architecture.md §3.3), so these turn its two activity
// signals — the real text stored per conversation (Camino A) and the count of
// AI-authored code hashes (Camino B) — into a token *range*, never a point, and
// always labeled "actividad estimada", never real spend.
//
// The approved approach (proposal §5) for Cursor is B-as-base with an A graft:
// Cursor's raw store is stable JSON, so counting its real text is a cheap, solid
// FLOOR; B fills conversations whose raw store is absent and, with the headroom
// factor, accounts for the invisible cost (per-turn context re-reads / cache)
// that stored text alone doesn't show. The two signals are independent: either
// can be missing for a given conversation, and A alone is enough to estimate
// one (see the package comment in cursor.go).
//
// Calibration status (full method, sample and evidence: docs/calibracion.md):
//   - The A floor is no longer a ratio at all: it is a real BPE tokenizer over
//     the text the store PROVES went to the model (internal/tokenize, blob.go).
//     The bytes/4 constant it replaced is gone.
//   - defaultTokensPerCodeHash — CALIBRATED against this machine's real data,
//     and RE-derived on 2026-07-30 because the A floor it is measured from
//     changed.
//   - invisibleHeadroom — still provisional 🏠, and still un-calibratable from
//     local data: Cursor records no total cost to measure real headroom against.

const (
	// invisibleHeadroom sets the high end of the range relative to the visible
	// floor: high = floor × (1 + invisibleHeadroom). It stands in for context
	// re-reads and cache that stored blobs count only once. UNCALIBRATED — a
	// deliberately wide, honest ceiling; un-calibratable from local data (Cursor
	// records no total cost to measure the real headroom against).
	invisibleHeadroom = 2.0

	// defaultTokensPerCodeHash is the Camino B factor used when a conversation has
	// code hashes but no readable store.db to self-calibrate from. CALIBRATED
	// base: the pooled measured ratio (Σ visible-tokens / Σ code-hashes) across
	// the 14 conversations that have both signals — 1,174,539 / 11,588 ≈ 101.
	//
	// It dropped from 1,745 because the floor it is measured against changed (A2:
	// the floor now tokenizes only the text the store proves was sent, instead of
	// dividing every byte on disk by 4). Not re-deriving it would have left this
	// fallback ~11× inflated over the path it is supposed to approximate.
	//
	// Read it knowing the dispersion: per-conversation ratios run 42.6 to 3,768 —
	// ~90×. With 14 conversations instead of 2, what more data showed is that a
	// code-hash is NOT a stable unit of work. Hence: range, never a point, and
	// deriveTokensPerCodeHash still supersedes this per run. See
	// docs/calibracion.md §Cursor.
	defaultTokensPerCodeHash = 101
)

// The A floor now uses a real tokenizer (A2, 2026-07-30). What is still open,
// and why it is not a TODO anyone can just close:
//
//   - The tokenizer is OpenAI's o200k_base, because Anthropic publishes none.
//     Closing that gap needs an Anthropic tokenizer or provider-side counts, not
//     more local data (internal/tokenize explains what was tried).
//   - invisibleHeadroom stays uncalibrated for the same reason as before: there
//     is no total-cost signal on disk to measure it against. Probe sessions
//     are the only path, and they are not implemented.

// deriveTokensPerCodeHash computes a Camino B factor (visible tokens per code
// hash) from the conversations that have BOTH signals, so the factor reflects
// this machine's real usage instead of a guessed constant. Falls back to
// defaultTokensPerCodeHash when there's nothing to calibrate from.
func deriveTokensPerCodeHash(convs []conversation) int {
	var sumVisibleTokens, sumHashes int
	for _, c := range convs {
		if c.visibleTokens > 0 && c.codeHashes > 0 {
			sumVisibleTokens += c.visibleTokens
			sumHashes += c.codeHashes
		}
	}
	if sumHashes == 0 {
		return defaultTokensPerCodeHash
	}
	return sumVisibleTokens / sumHashes
}

// estimateRange turns a conversation's signals into a token range [low, high].
// low is the visible-text floor (Camino A) when available, else the Camino B
// estimate (code hashes × factor). high adds the invisible-cost headroom.
//
// A conversation can carry either signal alone. With no code hashes (an
// ask-mode agent, which writes none) the floor is the stored text and
// tokensPerCodeHash never enters the arithmetic — the ceiling is still the same
// invisibleHeadroom, so the A-only range is the A-with-B range minus the code
// contribution, not a differently-shaped number.
func estimateRange(c conversation, tokensPerCodeHash int) (low, high int) {
	low = c.visibleTokens
	if low == 0 {
		low = c.codeHashes * tokensPerCodeHash
	}
	high = int(float64(low) * (1 + invisibleHeadroom))
	return low, high
}
