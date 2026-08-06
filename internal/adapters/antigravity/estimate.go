package antigravity

// Estimation for Antigravity. Antigravity (Google) exposes neither tokens nor
// cost: a 2026-07-26 probe of every local conversation found NO token or usage
// count anywhere in the schema or the blobs, and that is still true. So there is
// no reported number to read — but there IS the text itself.
//
// 🏠 As of 2026-07-31 (T11) this adapter is no longer Camino B only. The raw
// store's protobuf was mapped by wire-format recon (blob.go) and the text it
// proves was sent to the model is now tokenized for real, exactly as Cursor's A2
// did. That measurement replaced an invented band with a measured floor, and it
// moved the numbers a long way: the old band's LOW (2300 tokens/generation) sat
// BELOW the measured floor (4172), i.e. the previous "low" was not a floor at
// all. Full method and evidence: docs/calibracion.md §Antigravity — Camino A.
//
// The fallbacks below are still Camino B, and deliberately so: an undocumented
// format can change without notice, and when it does this adapter must degrade
// to counting activity, never report a zero and never invent a number.
//
// Everything remains labeled "actividad estimada", never real spend — Antigravity
// runs free under the shared subscription, so any dollar figure is relative
// weight / opportunity cost, not a charge.
const (
	// invisibleHeadroom sets the range's ceiling at floor × (1 + headroom). It
	// stands in for what the store cannot decide, and for Antigravity that is a
	// LOT: the floor counts the stored history once, but every generation re-sent
	// the system prompt, the tool definitions and the whole prefix again, and the
	// steps table holds a further ~1.0 MB of text (editor file snapshots,
	// subagent turns, tool errors) that disk cannot prove was ever put in the
	// model's context.
	//
	// Same value as Cursor's, for the same reason and with the same meaning, so
	// the two adapters' ranges stay comparable.
	invisibleHeadroom = 2.0

	// defaultTokensPerGeneration is the measured pooled floor: Σ floor tokens
	// 467,307 / Σ generations 112, over the 12 conversations on this machine
	// (2026-07-31) — the same pooled Σ/Σ method used for Cursor's
	// tokens_por_code_hash and for A3's steps-per-generation ratio.
	//
	// 🏠 PROVISIONAL: 12 conversations is a very small sample and the
	// per-conversation value really does spread (2,330 … 16,215), driven by how
	// much tool output a conversation pulled in. It is a measured number, not an
	// invented one, but it is not a stable one.
	defaultTokensPerGeneration = 4172

	// tokensPerStepFloor is the same pool expressed per step (467,307 / 322), for
	// the last-resort case of a DB with no gen_metadata at all. It replaces the
	// invented 800/6000 band that stood here before T11.
	tokensPerStepFloor = 1451
)

// deriveTokensPerGeneration returns this machine's own tokens-per-generation
// factor, pooled Σ/Σ over every conversation that decoded. Deriving it from the
// live data rather than hardcoding it means a machine whose conversations look
// nothing like the calibration sample still gets its own factor; the constant is
// only the fallback for when nothing decoded at all.
//
// Pooled, not averaged per conversation: a 31-generation conversation must weigh
// 31× a 1-generation one, which is the whole point of Σ/Σ.
func deriveTokensPerGeneration(convs []conversation) int {
	var sumTokens, sumGenerations int
	for _, c := range convs {
		if c.floorTokens > 0 && c.generations > 0 {
			sumTokens += c.floorTokens
			sumGenerations += c.generations
		}
	}
	if sumGenerations == 0 {
		return defaultTokensPerGeneration
	}
	return sumTokens / sumGenerations
}

// estimateRange turns one conversation into a token range.
//
// The low end is the measured floor whenever the blob decoded — text the store
// proves reached the model. When it did not decode, the estimate degrades in two
// documented steps rather than reporting a zero that would silently erase real
// activity: generations × the derived factor, and, for a DB with no gen_metadata
// at all, steps × the measured per-step floor.
func estimateRange(c conversation, tokensPerGeneration int) (low, high int) {
	switch {
	case c.floorTokens > 0:
		low = c.floorTokens
	case c.generations > 0:
		low = c.generations * tokensPerGeneration
	default:
		low = c.steps * tokensPerStepFloor
	}
	return low, int(float64(low) * (1 + invisibleHeadroom))
}
