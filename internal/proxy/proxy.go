// Package proxy is the small enforcement proxy (architecture.md §3.2): it
// forwards a real LLM API call to the upstream provider, but first increments a
// shared counter via internal/enforce and rejects the call when the combined cap
// is exceeded. No accounts, no UI, no Postgres — that is exactly what we do NOT
// want to duplicate from LiteLLM.
//
// Verified live against api.anthropic.com on 2026-07-26 and again on 2026-07-30
// with the in-memory counter (docs/enforcement-cableado.md): a call under the
// cap is forwarded, the next one over it gets 429 without reaching upstream.
// Raise it with `llm-agent-spend-manager proxy`.
package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/Rentheria/llm-agent-spend-manager/internal/enforce"
)

// KeyFunc picks the counter key for a request. The whole cap is combined when
// every agent maps to the same key; return per-agent keys to cap them
// separately. Default: DefaultKey.
type KeyFunc func(*http.Request) string

// AmountFunc reports how much this request adds to the counter — 1 to cap on
// request count, or an estimated token / micro-dollar weight. Default: one per
// request. It runs before the upstream call, so it can only use what's on the
// request (headers, an estimate from the body), not the real response usage —
// for that, see WithUsageCounting.
type AmountFunc func(*http.Request) int64

// DefaultKey is the shared key that makes the cap apply to the whole fleet.
func DefaultKey(*http.Request) string { return "fleet:default" }

// AgentHeaderKey caps per the X-Agent header (falling back to the fleet key),
// so several agents can share one cap or hold separate ones by how the caller
// sets the key.
func AgentHeaderKey(r *http.Request) string {
	if a := r.Header.Get("X-Agent"); a != "" {
		return "agent:" + a
	}
	return DefaultKey(r)
}

// OnePerRequest is the default AmountFunc.
func OnePerRequest(*http.Request) int64 { return 1 }

// NoUpfrontAmount charges nothing when the request goes out. It is the weight to
// pair with WithUsageCounting: there the real number arrives with the response
// (usage.go), so guessing one here would only add a second, wrong charge.
func NoUpfrontAmount(*http.Request) int64 { return 0 }

// ContextBytesAmount weighs a request by the bytes of context it carries.
//
// SUPERSEDED (T118, 2026-08-05) by WithUsageCounting — keep reading before
// choosing it. Bytes were picked because they are exact and need no invented
// tokens-per-byte ratio; what T96 missed is that the ratio still has to exist
// the moment the cap is set against a ceiling expressed in TOKENS. Measured on
// isolated probes it was 2.2–3.14; measured on the fleet's real traffic it was
// ~17.5, because every turn resends the whole history (bytes grow) while prompt
// caching keeps the tokens cheap. The cap cut the fleet off at 10% of the real
// quota. This mode is left in for a provider that reports no usage at all; for
// Anthropic, count the tokens it reports.
//
// The reason is measured, not assumed: on a real fleet most of the
// equivalent cost is cache-read, and cache-read scales with
// (context size × turns). Under OnePerRequest a turn dragging 900k tokens of
// history weighs exactly the same as a two-line question — which is the one
// distinction the cap needs to make.
//
// A cap set in bytes is a real ceiling on total context MOVED, which is the only
// thing it ever was — do not read it as a ceiling on tokens spent, and do not
// translate one into the other with a constant (see above).
//
// Known limitation: a request that declares no length (chunked upload) weighs 0
// and slips past the cap. That is acceptable here only because it doesn't happen
// on this path — the end-to-end run against the real API logged
// `POST /v1/messages content-length=246706` on every billable call
// (docs/enforcement-cableado.md). Revisit if a client ever streams its request.
func ContextBytesAmount(r *http.Request) int64 {
	if r.ContentLength < 0 {
		return 0
	}
	return r.ContentLength
}

// Proxy forwards to a fixed upstream base URL and enforces a combined cap.
type Proxy struct {
	limiter    *enforce.Limiter
	rp         *httputil.ReverseProxy
	keyFn      KeyFunc
	amount     AmountFunc
	countUsage bool
	target     *url.URL
}

// Option customizes a Proxy.
type Option func(*Proxy)

// WithKeyFunc overrides how the counter key is chosen (default DefaultKey).
func WithKeyFunc(fn KeyFunc) Option { return func(p *Proxy) { p.keyFn = fn } }

// WithAmountFunc overrides the per-request weight (default OnePerRequest).
func WithAmountFunc(fn AmountFunc) Option { return func(p *Proxy) { p.amount = fn } }

// WithUsageCounting charges the counter the token usage the provider reports in
// its response, instead of a weight guessed from the request (T118, usage.go).
// The cap then lives in the same unit as the plan's ceiling, so the two numbers
// can be compared at all.
//
// Two consequences worth knowing before turning it on:
//
//   - The charge lands AFTER the call, so it cannot stop the call that spends it
//     — only the next one. Requests already in flight when the cap is crossed all
//     go through: the overshoot is bounded by (in-flight calls × tokens per
//     call), single-digit millions for this fleet against a cap in the hundred
//     millions. Pair it with NoUpfrontAmount; an upfront weight would double-count.
//   - The X-Cap-* headers report the counter as it stood BEFORE this call's
//     tokens were added, since they are written when the response starts.
func WithUsageCounting() Option { return func(p *Proxy) { p.countUsage = true } }

// WithErrorLog sends the proxy's error and diagnostic lines to a specific
// logger instead of the standard one (whose stderr is what
// `journalctl --user -u lasm-proxy` shows in production).
func WithErrorLog(l *log.Logger) Option { return func(p *Proxy) { p.rp.ErrorLog = l } }

// New builds a Proxy forwarding to target (e.g. https://api.anthropic.com) and
// enforcing limiter's cap. Returns an error if target is not a valid absolute
// URL.
func New(target string, limiter *enforce.Limiter, opts ...Option) (*Proxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("proxy: target %q must be an absolute URL", target)
	}

	rp := httputil.NewSingleHostReverseProxy(u)
	p := &Proxy{
		limiter: limiter,
		rp:      rp,
		keyFn:   DefaultKey,
		amount:  OnePerRequest,
		target:  u,
	}

	// NewSingleHostReverseProxy rewrites URL.Host but leaves the Host header as
	// the client's; upstream APIs route/TLS-SNI on Host, so set it too.
	inner := rp.Director
	rp.Director = func(r *http.Request) {
		inner(r)
		r.Host = u.Host
		if p.countUsage {
			// A compressed body cannot be read on the way past, and the usage
			// report is inside it. Asking upstream for identity is cheaper than
			// carrying a decompressor on the hot path of every streamed answer,
			// and the bytes it costs are the RESPONSE's — small change next to the
			// context that just went up in the request.
			r.Header.Set("Accept-Encoding", "identity")
		}
	}
	// Without an ErrorHandler of its own the ReverseProxy runs the standard
	// library's, which logs the error and nothing else — the one-line dead end
	// the July incidents left us with (see diag.go).
	rp.ErrorHandler = p.errorHandler
	rp.ModifyResponse = p.tapUsage

	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// ServeHTTP enforces the cap, then forwards. On an over-cap request it returns
// 429 and does NOT touch the upstream (the whole point: the billable call never
// happens). If the counter itself errors — only reachable with the opt-in Redis
// counter — it fails OPEN (forwards) rather than taking the fleet down:
// availability over a hard guarantee when the coordinator is broken. This is a
// deliberate, documented trade-off.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := p.keyFn(r)
	amount := p.amount(r)

	// The diagnostic clock starts here, before the counter is consulted, so a
	// slow coordinator shows up inside the elapsed time of a failed forward
	// instead of hiding behind it (diag.go).
	//
	// AfterFunc on the INBOUND context is what dates the client hanging up.
	// It is deliberately used instead of an http.Server ConnState hook: net/http
	// cancels this context at the very moment it sees the inbound connection
	// die, so the same signal arrives already tied to the request that died,
	// with no connection map on the hot path and no line logged per healthy
	// connection. stop() unregisters before the handler returns, so a normal
	// completion never gets mistaken for a client walking away.
	diag := newRequestDiag(r)
	if p.countUsage {
		// The response tap runs deep inside the ReverseProxy, where the only thing
		// reachable is the outbound request — so what it needs to bill (which key,
		// which request to name in a log line) travels there in the context.
		r = r.WithContext(context.WithValue(r.Context(), chargeKey, &usageCharge{key: key, diag: diag}))
	}
	stop := context.AfterFunc(r.Context(), diag.markClientGone)
	defer stop()

	d, err := p.limiter.Allow(r.Context(), key, amount)
	if err != nil {
		// Fail-open: coordinator down shouldn't block every agent.
		w.Header().Set("X-Cap-Enforced", "fail-open")
		p.forward(w, r, diag)
		return
	}

	setCapHeaders(w.Header(), d)
	if !d.Allowed {
		w.Header().Set("Retry-After", "60")
		http.Error(w, fmt.Sprintf("combined budget cap reached (%d/%d)", d.Current, d.Limit),
			http.StatusTooManyRequests)
		return
	}
	p.forward(w, r, diag)
}

func setCapHeaders(h http.Header, d enforce.Decision) {
	h.Set("X-Cap-Limit", strconv.FormatInt(d.Limit, 10))
	h.Set("X-Cap-Current", strconv.FormatInt(d.Current, 10))
	h.Set("X-Cap-Remaining", strconv.FormatInt(d.Remaining, 10))
}
