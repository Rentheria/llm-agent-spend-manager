package claudecode

import "github.com/Rentheria/llm-agent-spend-manager/internal/pricing"

// EstimateCostUSD estimates the USD cost of a single Entry using the fleet-wide
// pricing table (internal/pricing). known is false when the model isn't priced
// — callers should surface tokens without a dollar figure rather than show a
// silently wrong $0. Claude Code records real token counts but always a $0
// cost (Claude Max is a flat subscription, not per-token billing), so this
// recomputes an estimated-equivalent cost from the tokens — a comparison proxy,
// not money charged (see internal/pricing doc).
func EstimateCostUSD(e Entry) (cost float64, known bool) {
	return pricing.EstimateUSD(e.Model, e.InputTokens, e.OutputTokens, e.CacheCreationInputTokens, e.CacheReadInputTokens)
}
