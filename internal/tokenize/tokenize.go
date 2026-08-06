// Package tokenize counts tokens in text with a real BPE tokenizer, for the
// adapters whose agent exposes no token counts of its own (Cursor today).
//
// # Why a tokenizer at all, and what it does NOT buy
//
// The measured agents (Claude Code, OpenClaw) never come through here: their logs carry the
// provider's own token counts, which are ground truth and cannot be improved on.
// This is for the activity-estimated tier, where the only thing on disk is the
// text itself.
//
// The tokenizer is o200k_base — OpenAI's. Anthropic does not publish one, so for
// a conversation held with a Claude model this is a **cross-family
// approximation**, and it is worth being exact about what that means: the
// segmentation is a different vocabulary's, so the count is not Anthropic's
// count. What changes versus the bytes/4 constant it replaces is the kind of
// error: bytes/4 assumed one ratio for prose, code, JSON and base64 alike, while
// a BPE actually segments each of them differently — which is most of the error
// on a corpus that is mostly code and tool output.
//
// The honest limit stays declared rather than papered over: this number feeds a
// **range**, never a point, and the tier label ("actividad estimada", `≈`) does
// not move up because the tokenizer got better. It could NOT be validated
// against Anthropic ground truth on this machine — the Claude transcripts pair
// `output_tokens` with text that is only part of what was billed (thinking is
// stored summarized, so the stored text tokenizes to ~25% of the billed count),
// so a comparison would measure the transcript's gaps, not the tokenizer's
// error. See docs/calibracion.md §Cursor.
package tokenize

import (
	"sync"

	"github.com/tiktoken-go/tokenizer"
)

var (
	once  sync.Once
	codec tokenizer.Codec
	// mu serializes Encode: the codec holds a regexp2 matcher, which is not
	// documented as safe for concurrent use. Adapters call this from one
	// goroutine today; the lock keeps a future parallel collector from
	// corrupting counts silently instead of failing loudly.
	mu sync.Mutex
)

// Count returns the number of tokens in s, or 0 if the tokenizer is unavailable.
//
// Zero on failure is deliberate: this feeds the FLOOR of an estimate range, and
// a floor that grows on a broken tokenizer would inflate the estimate. Losing
// the floor makes the conversation fall back to the code-hash path, which is
// visible in the output; a wrong floor would not be.
func Count(s string) int {
	if s == "" {
		return 0
	}
	once.Do(func() {
		// The vocabulary is embedded in the module: no network, no data files
		// to ship next to the binary.
		c, err := tokenizer.Get(tokenizer.O200kBase)
		if err != nil {
			return
		}
		codec = c
	})
	if codec == nil {
		return 0
	}
	mu.Lock()
	defer mu.Unlock()
	ids, _, err := codec.Encode(s)
	if err != nil {
		return 0
	}
	return len(ids)
}
