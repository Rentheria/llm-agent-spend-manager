package proxy

// Real-token accounting (T118, 2026-08-05). Why this file exists:
//
// The cap used to be charged in BYTES of the request (ContextBytesAmount),
// because the weight has to be known BEFORE the call is forwarded and the only
// thing available then is the request itself. T96 measured the bytes/token ratio
// with four isolated probes and got 2.2–3.14, then set the cap so it would cut
// before the ceiling for any ratio in that band.
//
// On 5-ago the same ratio measured ~17.5 in real use: the proxy had counted
// 344 MB (over its 250 MB cap, so the fleet was getting 429s) while Anthropic's
// own 5h window was at 19.7M tokens — 10%. The band was not wrong by a margin,
// it was wrong by 7x.
//
// What the probe could not see: every turn resends the whole conversation, so
// bytes grow with the accumulated context, while prompt caching makes those same
// repeated tokens cheap. Bytes and tokens diverge precisely where the fleet
// lives — long sessions — and no single constant reconciles them.
//
// So the cap stops estimating. Anthropic reports the real `usage` in the
// response; this file reads it out of both shapes the API answers in (a plain
// JSON body and the SSE stream Claude Code actually uses) so the counter adds
// what was really billed.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// TokenUsage is the token split Anthropic reports for one call.
//
// Total sums the four buckets — the same arithmetic aggregate.Record.TotalTokens
// does for the reports, and the same unit the 5h ceiling was calibrated in
// (159,392,577 tokens on the low end). Counting cache reads at full weight is
// deliberate: they are 64% of this fleet's consumption (T73/T76) and the plan's
// window charges for them, so a cap that discounted them would not be comparable
// to the ceiling it exists to stay under.
type TokenUsage struct {
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
}

// Total is what the cap counts.
func (u TokenUsage) Total() int64 {
	return u.Input + u.Output + u.CacheCreation + u.CacheRead
}

// IsZero reports that nothing was read — an error response, a non-completion
// endpoint, or a body whose usage never arrived.
func (u TokenUsage) IsZero() bool { return u.Total() == 0 }

// usageResult is everything the tap knows once a body is over.
type usageResult struct {
	Usage TokenUsage
	// Dropped is true when a line was too long to parse and was discarded, so
	// Usage may be short. It is reported instead of swallowed: a cap that stops
	// counting has to say so.
	Dropped bool
}

// maxUsageLineBytes bounds what one un-terminated chunk may buffer. SSE lines
// are one small event each; the only long "line" is a non-streaming JSON body,
// and at max_tokens=64k that is a few hundred KB. Two MB leaves room for that
// and still refuses to let a body the proxy never asked for become the proxy's
// memory (the unit runs under MemoryMax=256M).
const maxUsageLineBytes = 2 << 20

// sseDataPrefix is what carries the JSON payload of each streamed event.
const sseDataPrefix = "data:"

// reportedUsage is the wire shape of Anthropic's usage block.
type reportedUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// usageScanner extracts the reported usage from a response body as the bytes
// stream past. It holds at most one line, never the whole body: the client must
// keep seeing a streamed answer arrive token by token, which buffering to parse
// would quietly take away.
type usageScanner struct {
	usage TokenUsage
	// line is the current line being assembled across Read boundaries — a chunk
	// from the network cuts wherever TCP felt like, not where the events end.
	line []byte
	// overLine marks the line in progress as already discarded for length, so its
	// remaining fragments are dropped instead of re-growing the buffer.
	overLine bool
	dropped  bool
}

// Write feeds the scanner the bytes that just passed through. It never fails:
// an unreadable body must not break the response it is riding on.
func (s *usageScanner) Write(p []byte) (int, error) {
	n := len(p)
	for {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			s.appendToLine(p)
			return n, nil
		}
		s.appendToLine(p[:end])
		s.endLine()
		p = p[end+1:]
	}
}

// Flush parses what is left without a trailing newline — which is the whole body
// of a non-streaming JSON response, the one case where the interesting line
// never ends in \n.
func (s *usageScanner) Flush() { s.endLine() }

// result reports what was read and whether anything was dropped on the way.
func (s *usageScanner) result() usageResult {
	return usageResult{Usage: s.usage, Dropped: s.dropped}
}

func (s *usageScanner) appendToLine(b []byte) {
	if s.overLine {
		return
	}
	if len(s.line)+len(b) > maxUsageLineBytes {
		s.overLine, s.dropped, s.line = true, true, nil
		return
	}
	s.line = append(s.line, b...)
}

func (s *usageScanner) endLine() {
	if !s.overLine {
		s.readUsage(s.line)
	}
	s.line, s.overLine = s.line[:0], false
}

// readUsage takes the usage out of one line, whichever of the three shapes it
// is: an SSE `message_start` (usage nested under message), an SSE
// `message_delta` (usage at the top level), or a whole non-streaming response
// (also top level). Anything else — `event:` lines, blank lines, content deltas,
// errors — carries no usage and falls through.
func (s *usageScanner) readUsage(line []byte) {
	line = bytes.TrimSpace(line)
	if after, isEvent := bytes.CutPrefix(line, []byte(sseDataPrefix)); isEvent {
		line = bytes.TrimSpace(after)
	}
	if len(line) == 0 || line[0] != '{' {
		return
	}

	var event struct {
		Usage   *reportedUsage `json:"usage"`
		Message *struct {
			Usage *reportedUsage `json:"usage"`
		} `json:"message"`
	}
	// A decode error is not a reason to drop what did decode: encoding/json fills
	// every field it understands and only reports the first one it did not, and
	// one unexpected field in a response we do not own must not cost the reading.
	_ = json.Unmarshal(line, &event)

	if event.Message != nil {
		s.apply(event.Message.Usage)
	}
	s.apply(event.Usage)
}

// apply merges one usage block. Fields are REPLACED, not added: a stream reports
// usage cumulatively (message_start carries the input side, every message_delta
// re-states the running output count), so summing the events would charge the
// same tokens once per event. A zero field means "not reported here" and leaves
// what an earlier event said standing.
func (s *usageScanner) apply(u *reportedUsage) {
	if u == nil {
		return
	}
	replaceIfReported(&s.usage.Input, u.InputTokens)
	replaceIfReported(&s.usage.Output, u.OutputTokens)
	replaceIfReported(&s.usage.CacheCreation, u.CacheCreationInputTokens)
	replaceIfReported(&s.usage.CacheRead, u.CacheReadInputTokens)
}

func replaceIfReported(dst *int64, reported int64) {
	if reported > 0 {
		*dst = reported
	}
}

// usageTap forwards a response body untouched while reading the usage out of it,
// and settles the charge once the body ends. Every byte reaches the client at
// the same moment it would without the tap.
type usageTap struct {
	body    io.ReadCloser
	scanner usageScanner
	charge  func(usageResult)
	once    sync.Once
}

func newUsageTap(body io.ReadCloser, charge func(usageResult)) *usageTap {
	return &usageTap{body: body, charge: charge}
}

func (t *usageTap) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	if n > 0 {
		_, _ = t.scanner.Write(p[:n])
	}
	if err != nil {
		t.settle()
	}
	return n, err
}

// Close settles too, and not only because of EOF: a stream the client walked
// away from was still generated and still billed upstream, so it still counts.
// httputil.ReverseProxy closes the body on both its normal and its aborted path,
// which is what makes this the one place guaranteed to run.
func (t *usageTap) Close() error {
	t.settle()
	return t.body.Close()
}

func (t *usageTap) settle() {
	t.once.Do(func() {
		t.scanner.Flush()
		t.charge(t.scanner.result())
	})
}

// chargeKeyType is the private key the pending charge travels under, from
// ServeHTTP down to ModifyResponse (which is handed the outbound request, a
// clone of the inbound one, so the context value still arrives).
type chargeKeyType struct{}

var chargeKey chargeKeyType

// usageCharge is what the response tap needs to bill a call once its real usage
// is known: the counter key it belongs to, and the request it came from so a
// failure can name it.
type usageCharge struct {
	key  string
	diag *requestDiag
}

// usageChargeTimeout bounds the post-hoc write to the counter. The client is
// already served by then, so nobody is waiting on it — but a hung counter must
// not leak a goroutine per call either.
const usageChargeTimeout = 5 * time.Second

// tapUsage is the ReverseProxy's ModifyResponse: it wraps the body so the usage
// is read as it streams to the client.
func (p *Proxy) tapUsage(resp *http.Response) error {
	if !p.countUsage {
		return nil
	}
	charge, ok := resp.Request.Context().Value(chargeKey).(*usageCharge)
	if !ok {
		return nil
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		// Only reachable if upstream ignored the identity the Director asked for.
		// Loud on purpose: silently counting 0 would leave the cap looking healthy
		// while it had stopped counting anything at all.
		p.logf("cap: respuesta con Content-Encoding=%q: no puedo leer el usage, esta llamada NO se cuenta | method=%s path=%q",
			enc, charge.diag.method, charge.diag.path)
		return nil
	}
	resp.Body = newUsageTap(resp.Body, func(res usageResult) { p.settleUsage(charge, res) })
	return nil
}

// settleUsage adds the tokens the provider reported to the shared counter, once
// the response is over.
//
// It runs on its own context, not the request's: by now the client may well have
// hung up, and cancelling the charge with it would drop tokens that were spent
// anyway — the exact way a counter starts drifting under the real usage.
func (p *Proxy) settleUsage(c *usageCharge, res usageResult) {
	if res.Usage.IsZero() {
		if c.diag.billable() {
			p.logf("cap: %s %q terminó sin usage legible (dropped=%t): esos tokens NO entraron al contador",
				c.diag.method, c.diag.path, res.Dropped)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), usageChargeTimeout)
	defer cancel()

	d, err := p.limiter.Allow(ctx, c.key, res.Usage.Total())
	if err != nil {
		p.logf("cap: no pude sumar %d tokens al contador (%v): la ventana queda corta | key=%s path=%q",
			res.Usage.Total(), err, c.key, c.diag.path)
		return
	}
	// One line per counted call is the audit trail this unit change needs: the
	// July/August incidents were all diagnosed (or not) from what journalctl had.
	p.logf("cap: +%d tokens (in=%d out=%d cache_write=%d cache_read=%d) current=%d/%d | key=%s path=%q",
		res.Usage.Total(), res.Usage.Input, res.Usage.Output, res.Usage.CacheCreation, res.Usage.CacheRead,
		d.Current, d.Limit, c.key, c.diag.path)
}
