package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/enforce"
)

// realStreamedTurn is the shape Claude Code actually gets back: usage arrives
// split in two, the input side in message_start and the output side re-stated
// (cumulatively, not incrementally) in every message_delta.
const realStreamedTurn = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","model":"claude-opus-5","usage":{"input_tokens":2413,"cache_creation_input_tokens":1200,"cache_read_input_tokens":184000,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":347}}

event: message_stop
data: {"type":"message_stop"}

`

// The whole point of T118: the counter has to end up with what Anthropic says it
// spent, not with a ratio applied to the bytes that went past.
func TestUsageScanner_ReadsTheStreamedUsageOfARealTurn(t *testing.T) {
	var s usageScanner
	if _, err := s.Write([]byte(realStreamedTurn)); err != nil {
		t.Fatalf("Write = %v, quería nil", err)
	}
	s.Flush()

	want := TokenUsage{Input: 2413, Output: 347, CacheCreation: 1200, CacheRead: 184000}
	if s.usage != want {
		t.Errorf("usage = %+v, quería %+v", s.usage, want)
	}
	if got := s.usage.Total(); got != 187960 {
		t.Errorf("Total = %d, quería 187960 (las cuatro cubetas, como el techo calibrado)", got)
	}
}

// A chunk from the network cuts wherever TCP felt like it, not where the events
// end. Byte-at-a-time is the worst case of that and must read the same.
func TestUsageScanner_ReassemblesEventsSplitAcrossChunks(t *testing.T) {
	var s usageScanner
	for i := range realStreamedTurn {
		if _, err := s.Write([]byte{realStreamedTurn[i]}); err != nil {
			t.Fatalf("Write = %v, quería nil", err)
		}
	}
	s.Flush()

	if got, want := s.usage.Total(), int64(187960); got != want {
		t.Errorf("Total leyendo byte a byte = %d, quería %d", got, want)
	}
}

// Streamed usage is cumulative: adding up the deltas would charge the same
// output tokens once per event, and a long answer emits many.
func TestUsageScanner_DoesNotAddUpCumulativeDeltas(t *testing.T) {
	var s usageScanner
	_, _ = s.Write([]byte(`data: {"type":"message_delta","usage":{"output_tokens":10}}` + "\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":25}}` + "\n"))
	s.Flush()

	if s.usage.Output != 25 {
		t.Errorf("Output = %d, quería 25 (el último reporte, no 10+25)", s.usage.Output)
	}
}

// The non-streaming answer is a single JSON object with no trailing newline —
// the one case where the interesting line only ends when the body does.
func TestUsageScanner_ReadsAPlainJSONResponse(t *testing.T) {
	var s usageScanner
	_, _ = s.Write([]byte(`{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],` +
		`"usage":{"input_tokens":100,"output_tokens":25,"cache_creation_input_tokens":0,"cache_read_input_tokens":900}}`))
	s.Flush()

	want := TokenUsage{Input: 100, Output: 25, CacheRead: 900}
	if s.usage != want {
		t.Errorf("usage = %+v, quería %+v", s.usage, want)
	}
}

// Health-checks, errors and the token-count endpoint carry no usage. Inventing
// one for them is exactly the estimating this ticket removes.
func TestUsageScanner_CountsNothingWithoutAUsageReport(t *testing.T) {
	for _, body := range []string{
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		`{"input_tokens":4321}`, // /v1/messages/count_tokens: not a billed call
		"",
	} {
		var s usageScanner
		_, _ = s.Write([]byte(body))
		s.Flush()
		if !s.usage.IsZero() {
			t.Errorf("cuerpo %q contó %+v, quería cero", body, s.usage)
		}
	}
}

// The proxy does not control what upstream sends. A body that never ends a line
// must cost a bounded amount of memory and SAY it stopped counting, because a
// silent zero reads exactly like a cheap call.
func TestUsageScanner_DropsALineTooBigToBeAnEventAndSaysSo(t *testing.T) {
	var s usageScanner
	_, _ = s.Write(bytes.Repeat([]byte("x"), maxUsageLineBytes+1))
	s.Flush()

	res := s.result()
	if !res.Dropped {
		t.Error("Dropped = false: una línea descartada tiene que quedar visible")
	}
	if len(s.line) > maxUsageLineBytes {
		t.Errorf("el buffer quedó en %d bytes, quería como mucho %d", len(s.line), maxUsageLineBytes)
	}
}

// After dropping an oversized line the scanner has to keep working: the usage of
// a stream lives in the events that come after it.
func TestUsageScanner_KeepsReadingAfterADroppedLine(t *testing.T) {
	var s usageScanner
	_, _ = s.Write(bytes.Repeat([]byte("x"), maxUsageLineBytes+1))
	_, _ = s.Write([]byte("\n" + `data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n"))
	s.Flush()

	if s.usage.Output != 7 {
		t.Errorf("Output = %d, quería 7: el scanner se quedó mudo tras descartar una línea", s.usage.Output)
	}
}

// --- end to end through the proxy ---------------------------------------------

// streamingUpstream is a stand-in Anthropic that answers with a real-shaped SSE
// turn and reports what it received.
func streamingUpstream(t *testing.T) (*httptest.Server, *http.Header) {
	t.Helper()
	var seen http.Header
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, realStreamedTurn)
	}))
	t.Cleanup(s.Close)
	return s, &seen
}

func tokenProxy(t *testing.T, target string, limit int64, logs *bytes.Buffer) (*httptest.Server, *enforce.Limiter) {
	t.Helper()
	limiter := enforce.New(enforce.NewMemoryCounter(), time.Minute, limit)
	opts := []Option{WithAmountFunc(NoUpfrontAmount), WithUsageCounting()}
	if logs != nil {
		opts = append(opts, WithErrorLog(log.New(logs, "", 0)))
	}
	p, err := New(target, limiter, opts...)
	if err != nil {
		t.Fatalf("New = %v, quería nil", err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, limiter
}

// counted reads the counter without moving it (Allow with amount 0), and waits:
// the charge lands when the response body closes, which may be a hair after the
// client already has its answer.
func counted(t *testing.T, limiter *enforce.Limiter, want int64) int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var current int64
	for {
		d, err := limiter.Allow(context.Background(), DefaultKey(nil), 0)
		if err != nil {
			t.Fatalf("Allow = %v, quería nil", err)
		}
		current = d.Current
		if current >= want || time.Now().After(deadline) {
			return current
		}
		time.Sleep(time.Millisecond)
	}
}

// notCounted asserts nothing was charged, after giving the charge the moment it
// would have needed to land.
func notCounted(t *testing.T, limiter *enforce.Limiter) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	d, err := limiter.Allow(context.Background(), DefaultKey(nil), 0)
	if err != nil {
		t.Fatalf("Allow = %v, quería nil", err)
	}
	if d.Current != 0 {
		t.Errorf("contador = %d, quería 0: no había usage legible que contar", d.Current)
	}
}

func postTurn(t *testing.T, srv *httptest.Server) *http.Response {
	t.Helper()
	res, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-opus-5"}`))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res
}

// The whole ticket in one test: what lands in the counter is the token count
// Anthropic reported, not a weight derived from the request.
func TestProxy_CountsTheTokensTheProviderReports(t *testing.T) {
	up, _ := streamingUpstream(t)
	srv, limiter := tokenProxy(t, up.URL, 0, nil)

	if res := postTurn(t, srv); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, quería 200", res.StatusCode)
	}

	if got := counted(t, limiter, 187960); got != 187960 {
		t.Errorf("contador = %d, quería 187960 (los tokens reportados por el upstream)", got)
	}
}

// Nothing may be charged before the call, or the fleet pays twice for every
// turn: once for a guess, once for the truth.
func TestProxy_ChargesNothingUpfrontInTokensMode(t *testing.T) {
	up, _ := streamingUpstream(t)
	srv, _ := tokenProxy(t, up.URL, 500_000, nil)

	res := postTurn(t, srv)

	if got := res.Header.Get("X-Cap-Current"); got != "0" {
		t.Errorf("X-Cap-Current = %q, quería 0: el peso real solo se conoce con la respuesta", got)
	}
}

// The cap still has to cut. In tokens mode the charge lands after the call, so
// what it stops is the NEXT one — with the counter over the cap, no further
// request reaches the provider.
func TestProxy_BlocksTheNextCallOnceTheRealTokensCrossTheCap(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, realStreamedTurn)
	}))
	t.Cleanup(up.Close)
	// One real turn (187,960 tokens) already crosses this cap.
	srv, limiter := tokenProxy(t, up.URL, 100_000, nil)

	if res := postTurn(t, srv); res.StatusCode != http.StatusOK {
		t.Fatalf("primera llamada = %d, quería 200: el contador estaba en cero", res.StatusCode)
	}
	counted(t, limiter, 187960)

	res := postTurn(t, srv)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("segunda llamada = %d, quería 429", res.StatusCode)
	}
	if hits != 1 {
		t.Errorf("llamadas al upstream = %d, quería 1: la bloqueada no debe facturarse", hits)
	}
}

// The usage report is inside the body, so the body has to be readable. Upstream
// is asked for identity instead of carrying a decompressor on the hot path.
func TestProxy_AsksUpstreamNotToCompressWhenCountingTokens(t *testing.T) {
	up, seen := streamingUpstream(t)
	srv, _ := tokenProxy(t, up.URL, 0, nil)

	postTurn(t, srv)

	if got := seen.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding hacia el upstream = %q, quería identity", got)
	}
}

// If upstream compresses anyway, the tokens cannot be read. Counting 0 in
// silence would leave the cap looking healthy while it counted nothing at all.
func TestProxy_SaysSoWhenACompressedResponseCannotBeCounted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "\x1f\x8b\x08 no soy JSON")
	}))
	t.Cleanup(up.Close)
	var logs bytes.Buffer
	srv, limiter := tokenProxy(t, up.URL, 0, &logs)

	postTurn(t, srv)

	notCounted(t, limiter)
	if !strings.Contains(logs.String(), "Content-Encoding") {
		t.Errorf("el log no dice que dejó de contar:\n%s", logs.String())
	}
}

// A completion that comes back without usage is a hole in the cap. It gets said
// out loud; a health-check that reports none does not, or the warning becomes
// noise nobody reads.
func TestProxy_ReportsABillableCallThatBroughtNoUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error"}}`)
	}))
	t.Cleanup(up.Close)

	var logs bytes.Buffer
	srv, _ := tokenProxy(t, up.URL, 0, &logs)
	postTurn(t, srv)
	if !strings.Contains(logs.String(), "/v1/messages") {
		t.Errorf("una completación sin usage pasó callada:\n%s", logs.String())
	}

	var quiet bytes.Buffer
	srvQuiet, _ := tokenProxy(t, up.URL, 0, &quiet)
	res, err := http.Get(srvQuiet.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if quiet.Len() != 0 {
		t.Errorf("un health-check sin usage generó ruido:\n%s", quiet.String())
	}
}

// Tokens generated for a client that walked away were still billed upstream, so
// they still count. Losing them is how the counter drifts under the real usage.
func TestProxy_CountsAStreamTheClientAbandoned(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// The input side of the usage is known from the first event — which is
		// exactly the part an abandoned stream would otherwise lose.
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":500,"cache_read_input_tokens":9500}}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, `data: {"type":"message_delta","usage":{"output_tokens":40}}`+"\n\n")
	}))
	t.Cleanup(up.Close)
	srv, limiter := tokenProxy(t, up.URL, 0, nil)

	res, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := res.Body.Read(buf); err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	res.Body.Close() // the client walks away mid-stream
	close(release)

	// At least the first event. Whether the tail (40 output tokens) also lands
	// is a race with the abort and both outcomes are correct — what must never
	// happen is losing the 10000 the provider already reported, which is the
	// expensive part of an abandoned turn.
	if got := counted(t, limiter, 10_000); got < 10_000 {
		t.Errorf("contador = %d, quería al menos 10000 (lo que el primer evento ya había reportado)", got)
	}
}

// Reading the usage must not turn a stream into a wait: the client sees each
// event when it happens, or a long answer stops looking alive.
func TestProxy_KeepsStreamingWhileItCounts(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release // the answer is not over, and must not need to be
	}))
	t.Cleanup(up.Close)
	srv, _ := tokenProxy(t, up.URL, 0, nil)
	t.Cleanup(func() { close(release) })

	res, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { res.Body.Close() })

	first := make(chan string, 1)
	go func() {
		buf := make([]byte, 128)
		n, _ := res.Body.Read(buf)
		first <- string(buf[:n])
	}()

	select {
	case got := <-first:
		if !strings.Contains(got, "message_start") {
			t.Errorf("primer trozo = %q, quería el evento que el upstream ya había mandado", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("el primer evento no llegó al cliente: el tap está bufferizando el stream")
	}
}

func TestTokenUsage_TotalSumsTheFourBuckets(t *testing.T) {
	u := TokenUsage{Input: 1, Output: 2, CacheCreation: 4, CacheRead: 8}
	if got := u.Total(); got != 15 {
		t.Errorf("Total = %d, quería 15", got)
	}
	if u.IsZero() {
		t.Error("IsZero = true con tokens contados")
	}
}

func TestNoUpfrontAmount_ChargesNothing(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("x", 4096)))
	if got := NoUpfrontAmount(r); got != 0 {
		t.Errorf("NoUpfrontAmount = %d, quería 0: el peso real llega con la respuesta", got)
	}
}
