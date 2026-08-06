// Package pricing holds the single source of truth for per-model LLM prices
// and estimates a call's USD cost from its token counts.
//
// Every adapter (Claude Code, OpenClaw, ...) reads real
// token counts from its agent's on-disk logs but cannot trust any $ the agent
// records: subscription/CLI-based agents report usage.cost = 0 because they
// aren't billed per token. So each adapter converts its own usage shape into
// the four token buckets below and calls EstimateUSD to get a consistent,
// self-computed cost with the same table across the whole fleet.
//
// IMPORTANT — this is an *estimated-equivalent* cost, not a real bill. For
// subscription/CLI agents (Claude Max, OpenClaw's CLI plan) there is no
// per-token charge at all; the flat subscription price is what's actually
// paid. The figure here is `real tokens × public API list price`, useful to
// compare relative weight between agents/tasks/models and as a proxy for how
// close an agent is to its subscription's internal usage cap — but it is NOT
// money charged. UI/CLI must label it "estimated equivalent cost", never
// "real spend" (see docs/architecture.md §3.1).
package pricing

import "regexp"

// modelRate is USD per token, converted from each vendor's published
// per-million list rates. Sources, cited per entry below:
// https://platform.claude.com/docs/en/about-claude/pricing (Anthropic, verified
// 2026-07-26) and https://ai.google.dev/gemini-api/docs/pricing (Google,
// verified 2026-08-06). Vendors change these over time (e.g. Sonnet 5's
// introductory rate ends 2026-08-31) — this table needs manual review whenever a
// model's price changes or a new model ships. An unknown model yields
// known=false rather than a guessed price.
type modelRate struct {
	inputPerToken      float64
	cacheWrite5mPerTok float64
	cacheWrite1hPerTok float64
	cacheReadPerToken  float64
	outputPerToken     float64
}

const perMillion = 1_000_000

// dateSuffix matches a trailing dated-snapshot id Anthropic sometimes appends
// to a model name (e.g. "claude-haiku-4-5-20251001"), which the table doesn't
// enumerate per-snapshot since the price is the same as the base model name.
var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// thinkingSuffix matches the extended-thinking marker some clients append to a
// model id (Antigravity reports "claude-opus-4-6-thinking"). It is the same
// model with thinking enabled, not a separate SKU: Anthropic bills thinking
// tokens as ordinary output tokens, so the base model's rates apply unchanged.
var thinkingSuffix = regexp.MustCompile(`-thinking$`)

var pricingTable = map[string]modelRate{
	"claude-opus-4-8": {
		inputPerToken:      5.00 / perMillion,
		cacheWrite5mPerTok: 6.25 / perMillion,
		cacheWrite1hPerTok: 10.00 / perMillion,
		cacheReadPerToken:  0.50 / perMillion,
		outputPerToken:     25.00 / perMillion,
	},
	// Added 2026-07-31 (T11): Antigravity's Claude-backed conversations report
	// this id, and without it every one of them stayed unpriced. Rates are the
	// published Opus-tier ones ($5.00 in / $25.00 out per million), re-checked
	// against the source above on 2026-07-31 — they coincide with opus-4-8's
	// because Anthropic prices the whole Opus tier alike, not because they were
	// copied from it.
	"claude-opus-4-6": {
		inputPerToken:      5.00 / perMillion,
		cacheWrite5mPerTok: 6.25 / perMillion,
		cacheWrite1hPerTok: 10.00 / perMillion,
		cacheReadPerToken:  0.50 / perMillion,
		outputPerToken:     25.00 / perMillion,
	},
	"claude-opus-5": {
		inputPerToken:      5.00 / perMillion,
		cacheWrite5mPerTok: 6.25 / perMillion,
		cacheWrite1hPerTok: 10.00 / perMillion,
		cacheReadPerToken:  0.50 / perMillion,
		outputPerToken:     25.00 / perMillion,
	},
	"claude-sonnet-5": {
		// Introductory pricing through 2026-08-31; becomes $3/$15 after.
		inputPerToken:      2.00 / perMillion,
		cacheWrite5mPerTok: 2.50 / perMillion,
		cacheWrite1hPerTok: 4.00 / perMillion,
		cacheReadPerToken:  0.20 / perMillion,
		outputPerToken:     10.00 / perMillion,
	},
	"claude-haiku-4-5": {
		inputPerToken:      1.00 / perMillion,
		cacheWrite5mPerTok: 1.25 / perMillion,
		cacheWrite1hPerTok: 2.00 / perMillion,
		cacheReadPerToken:  0.10 / perMillion,
		outputPerToken:     5.00 / perMillion,
	},
	"claude-sonnet-4-6": {
		inputPerToken:      3.00 / perMillion,
		cacheWrite5mPerTok: 3.75 / perMillion,
		cacheWrite1hPerTok: 6.00 / perMillion,
		cacheReadPerToken:  0.30 / perMillion,
		outputPerToken:     15.00 / perMillion,
	},
	"claude-sonnet-4-5": {
		inputPerToken:      3.00 / perMillion,
		cacheWrite5mPerTok: 3.75 / perMillion,
		cacheWrite1hPerTok: 6.00 / perMillion,
		cacheReadPerToken:  0.30 / perMillion,
		outputPerToken:     15.00 / perMillion,
	},
	"claude-fable-5": {
		inputPerToken:      10.00 / perMillion,
		cacheWrite5mPerTok: 12.50 / perMillion,
		cacheWrite1hPerTok: 20.00 / perMillion,
		cacheReadPerToken:  1.00 / perMillion,
		outputPerToken:     50.00 / perMillion,
	},
	// Added 2026-08-06 (A6): the only non-Anthropic model the fleet has actually
	// run turns on. OpenClaw falls back to it through its
	// google-gemini-cli backend when every Anthropic model in its chain fails,
	// and those turns stayed unpriced (advise E-05).
	//
	// It belongs in this table for the same reason the Anthropic entries do, and
	// on the same terms: OpenClaw reaches it over an OAuth CLI session
	// (auth.profiles.google-gemini-cli, mode oauth), so nobody is billed per
	// token — exactly like claude-cli. The figure is the package's usual
	// estimated-equivalent, not a charge. What it is NOT is a locally-run model:
	// the tokens leave this machine and Google publishes a list price for them,
	// so pricing them at zero (freeLocalModels) would be a different claim
	// altogether and a false one.
	//
	// Source: https://ai.google.dev/gemini-api/docs/pricing, verified 2026-08-06.
	// These are the Standard-tier rates for prompts up to 200K tokens. Google
	// bills prompts ABOVE 200K at a higher tier ($4.00 in / $18.00 out / $0.40
	// cache-read per million) that this flat table can't express, so a turn with a
	// prompt over 200K is valued at a floor here — the same conservative direction
	// as the 5-minute cache-write assumption in EstimateByBucket. It matters: 4 of
	// the 6 turns that motivated this entry carried prompts well past 200K.
	//
	// cacheWrite is the input rate, not a markup of it, because Google charges no
	// per-token cache-write fee at all: priming a prefix costs one ordinary input
	// pass, and what explicit caching adds on top is time-based storage
	// ($4.50/MTok/hour), which is not a per-token price and has no slot here. That
	// also keeps ContextRates honest — a zero write rate would tell
	// internal/contextcurve that restarting a Gemini conversation is free.
	"gemini-3.1-pro-preview": {
		inputPerToken:      2.00 / perMillion,
		cacheWrite5mPerTok: 2.00 / perMillion,
		cacheWrite1hPerTok: 2.00 / perMillion,
		cacheReadPerToken:  0.20 / perMillion,
		outputPerToken:     12.00 / perMillion,
	},
}

// TokenCounts is one call's usage split into the four billable buckets. Grouped
// as a struct because they always travel together and four positional ints at a
// call site are impossible to read (which one was cache write?).
type TokenCounts struct {
	Input      int
	Output     int
	CacheWrite int
	CacheRead  int
}

// BucketCosts is a call's estimated-equivalent cost attributed to each billable
// bucket. The split is what makes optimization actionable: 10M cache-read tokens
// and 10M output tokens are the same "tokens" but a ~50x difference in cost, so
// only the per-bucket view tells you which lever is worth pulling (see
// internal/advise).
type BucketCosts struct {
	Input      float64
	Output     float64
	CacheWrite float64
	CacheRead  float64
}

// Total is the sum of the four buckets — the same figure EstimateUSD returns.
func (c BucketCosts) Total() float64 {
	return c.Input + c.Output + c.CacheWrite + c.CacheRead
}

// freeLocalModels run on local hardware (Ollama) with no API
// billing at all, so their price is zero — and that is a *known* price, not a
// missing one. The distinction matters: an unknown model must render as "sin
// precio" so the reader knows the total understates reality, while a local model
// legitimately contributes $0 and should not be flagged as a measurement gap.
//
// The entry below was confirmed against a real run: the fleet fell back to a
// local model while the Claude models were unavailable.
var freeLocalModels = map[string]bool{
	"nemotron-3-super": true,
}

// IsFreeLocal reports whether a model runs locally at no cost.
func IsFreeLocal(model string) bool { return freeLocalModels[model] }

// EstimateByBucket estimates one call's cost per billable bucket. known is false
// when model isn't in the table, in which case the costs are all zero — callers
// should surface tokens without a dollar figure rather than show a silently
// wrong $0. Locally-run models return zero costs with known=true: their price
// isn't missing, it's zero.
//
// cacheWrite tokens are billed at the 5-minute-cache-write rate: that's the
// default TTL when the caller doesn't request the 1-hour cache, and callers have
// no way to know which one a given call used, so this takes the conservative
// (cheaper) assumption rather than overstating cost.
func EstimateByBucket(model string, tokens TokenCounts) (costs BucketCosts, known bool) {
	if IsFreeLocal(model) {
		return BucketCosts{}, true
	}
	rate, ok := lookup(model)
	if !ok {
		return BucketCosts{}, false
	}
	return BucketCosts{
		Input:      float64(tokens.Input) * rate.inputPerToken,
		Output:     float64(tokens.Output) * rate.outputPerToken,
		CacheWrite: float64(tokens.CacheWrite) * rate.cacheWrite5mPerTok,
		CacheRead:  float64(tokens.CacheRead) * rate.cacheReadPerToken,
	}, true
}

// EstimateUSD estimates the total USD cost of one LLM call from its token
// counts. known is false when model isn't in the table — callers should surface
// tokens without a dollar figure rather than show a silently wrong $0.
func EstimateUSD(model string, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens int) (cost float64, known bool) {
	costs, known := EstimateByBucket(model, TokenCounts{
		Input:      inputTokens,
		Output:     outputTokens,
		CacheWrite: cacheWriteTokens,
		CacheRead:  cacheReadTokens,
	})
	return costs.Total(), known
}

// ContextRates returns the two per-token prices that decide whether carrying a
// conversation forward is cheaper than rebuilding it: what re-reading the cached
// prefix costs on every turn, and what re-priming that prefix costs once. Those
// two numbers are the whole break-even calculation in internal/contextcurve.
//
// known is false when the model isn't in the table — the caller must then report
// the figure as unavailable rather than borrow another model's price. Locally-run
// models return zero rates with known=true: carrying context costs them nothing,
// so there is no break-even point to find, and that is a real answer.
//
// The write rate is the 5-minute one, matching EstimateByBucket's assumption.
func ContextRates(model string) (cacheReadPerToken, cacheWritePerToken float64, known bool) {
	if IsFreeLocal(model) {
		return 0, 0, true
	}
	rate, ok := lookup(model)
	if !ok {
		return 0, 0, false
	}
	return rate.cacheReadPerToken, rate.cacheWrite5mPerTok, true
}

// PerTokenBounds returns a model's cheapest and priciest per-token rates (input
// and output respectively). Activity-tier adapters (Cursor, Antigravity) know a
// token estimate but not the input/output split, so they value the low end of
// their token range at the input rate and the high end at the output rate — an
// honest cost range rather than a single guessed number. known is false when the
// model isn't in the table.
func PerTokenBounds(model string) (low, high float64, known bool) {
	rate, ok := lookup(model)
	if !ok {
		return 0, 0, false
	}
	return rate.inputPerToken, rate.outputPerToken, true
}

// lookup resolves a model name to its rate, tolerating the trailing suffixes the
// table deliberately doesn't enumerate because they don't change the price: an
// extended-thinking marker and a dated snapshot id.
func lookup(model string) (modelRate, bool) {
	for _, name := range baseNames(model) {
		if rate, ok := pricingTable[name]; ok {
			return rate, true
		}
	}
	return modelRate{}, false
}

// baseNames returns model followed by the progressively-stripped names to try,
// most specific first — so an exact table entry always wins over a stripped one.
func baseNames(model string) []string {
	names := []string{model}
	for _, suffix := range []*regexp.Regexp{thinkingSuffix, dateSuffix} {
		last := names[len(names)-1]
		if base := suffix.ReplaceAllString(last, ""); base != last {
			names = append(names, base)
		}
	}
	return names
}
