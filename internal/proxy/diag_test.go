package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// diagLog captures what the proxy logs. It is written from the handler
// goroutine and read from the test one, so it holds its own lock.
type diagLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *diagLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *diagLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// waitForLine waits for a line matching want, because the request that produced
// it is aborted from the client side: the client is already gone when the
// handler finishes writing the diagnosis.
func (l *diagLog) waitForLine(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for line := range strings.SplitSeq(l.String(), "\n") {
			if strings.Contains(line, want) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line containing %q after 3s; got:\n%s", want, l.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wantFields asserts the diagnostic key=value pairs the next incident has to
// carry. A missing field here means `journalctl` goes back to being a dead end.
func wantFields(t *testing.T, line string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(k) + `=(\S+)`)
		m := re.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("diagnostic line has no %s= field:\n%s", k, line)
			continue
		}
		if v != "" && m[1] != v {
			t.Errorf("%s = %s, want %s\nline: %s", k, m[1], v, line)
		}
	}
}

// drainBody is not decoration: net/http only starts watching an inbound
// connection for a hang-up once the handler has read the request body to EOF.
// An upstream stand-in that skips it never sees its context cancelled and hangs
// the whole test binary at Close.
func drainBody(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
}

// waitForHangUp blocks a stand-in upstream until its caller disappears, with a
// ceiling so a broken expectation fails the test instead of hanging it.
func waitForHangUp(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

func diagProxy(t *testing.T, target string) (*httptest.Server, *diagLog) {
	t.Helper()
	captured := &diagLog{}
	p, err := New(target, testLimiter(t, 100), WithErrorLog(log.New(captured, "", 0)))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, captured
}

// This is the exact shape of the July incidents: `claude` walks away while the
// proxy is still waiting on Anthropic, and all the old log said was
// "proxy error: context canceled" — which cannot tell a dying client from a
// dying upstream. The new line has to name which side left first.
func TestProxy_ClassifiesAContextCancelBeforeTheResponseStarts(t *testing.T) {
	upstreamHit := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		close(upstreamHit)
		waitForHangUp(r) // never answers; the client gives up first
	}))
	t.Cleanup(up.Close)

	srv, captured := diagProxy(t, up.URL)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/messages", strings.NewReader("{}"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
		}
	}()

	<-upstreamHit
	cancel()
	<-done

	line := captured.waitForLine(t, "http: proxy error:")
	wantFields(t, line, map[string]string{
		"class":        "context.Canceled",
		"first_cut":    "client",
		"ctx_err":      "context.Canceled",
		"resp_status":  "0", // upstream never sent headers
		"client_bytes": "0", // nothing ever reached the client
		"method":       "POST",
		"elapsed":      "",
		"path":         "",
	})
	if !strings.Contains(line, `path="/v1/messages"`) {
		t.Errorf("diagnostic line should carry the request path:\n%s", line)
	}
	if strings.Contains(line, "client_gone_after=none") {
		t.Errorf("client_gone_after should hold the moment the client left:\n%s", line)
	}
}

// The other half of the classification: the client is still connected and the
// upstream is what broke. Same generic error before A5, opposite verdict.
func TestProxy_ClassifiesAnUpstreamFailureAsNotTheClient(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening there anymore

	srv, captured := diagProxy(t, deadURL)

	res, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (the answer to the client must not change)", res.StatusCode)
	}

	line := captured.waitForLine(t, "http: proxy error:")
	wantFields(t, line, map[string]string{
		"first_cut":         "upstream",
		"ctx_err":           "none",
		"client_gone_after": "none",
		"resp_status":       "0",
		"client_bytes":      "0",
	})
	if strings.Contains(line, "class=context.Canceled") {
		t.Errorf("a dial failure is not a cancellation:\n%s", line)
	}
}

// A cut in the middle of a stream is the case the standard library logs
// NOTHING about: ReverseProxy panics with http.ErrAbortHandler instead of
// calling ErrorHandler, and net/http swallows it. This is the half of the
// diagnosis that did not exist before, and the one that says "the stream had
// already started" — the difference between a mid-answer death and a call that
// never connected.
func TestProxy_ReportsAStreamCutAfterTheResponseStarted(t *testing.T) {
	streaming := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first chunk\n\n")
		http.NewResponseController(w).Flush()
		close(streaming)
		waitForHangUp(r) // the rest of the answer never arrives
	}))
	t.Cleanup(up.Close)

	srv, captured := diagProxy(t, up.URL)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/messages", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	<-streaming
	// Read the first chunk so the proxy has really written bytes to the client
	// before the connection dies.
	buf := make([]byte, len("data: first chunk\n\n"))
	if _, err := io.ReadFull(res.Body, buf); err != nil {
		t.Fatalf("reading the start of the stream: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	res.Body.Close()

	line := captured.waitForLine(t, "aborted mid-stream")
	wantFields(t, line, map[string]string{
		"first_cut":   "client",
		"ctx_err":     "context.Canceled",
		"resp_status": "200",
	})
	if got := fieldValue(line, "client_bytes"); got == "0" || got == "" {
		t.Errorf("client_bytes = %q, want the bytes already streamed before the cut:\n%s", got, line)
	}
}

// The AfterFunc that dates the hang-up runs on its own goroutine, and on a fast
// cut the line was rendered before it got scheduled: first_cut=upstream sitting
// next to ctx_err=context.Canceled, the same event read two ways. That is the
// worst kind of diagnostic — one that points at the other side of the wire.
// Whatever the goroutine has managed to run, a cancelled inbound context means
// the client left.
func TestRequestDiag_DoesNotBlameUpstreamWhenTheClientContextIsAlreadyDone(t *testing.T) {
	d := newRequestDiag(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	line := d.fields(context.Canceled, context.Canceled) // markClientGone never ran

	if got := fieldValue(line, "first_cut"); got != "client" {
		t.Errorf("first_cut = %q, quería client: el contexto de entrada ya estaba cancelado\n%s", got, line)
	}
	if got := fieldValue(line, "client_gone_after"); got == "none" {
		t.Errorf("client_gone_after = none con el contexto cancelado, contradice a first_cut:\n%s", line)
	}
}

// Wrapping the ResponseWriter to count bytes must not cost the proxy its
// streaming: if the wrapper hid http.Flusher from the ReverseProxy, a
// server-sent-event response would only reach the client at the end. This
// ticket is observability with zero behaviour change, and this is the test that
// holds that line.
func TestProxy_StillStreamsWhileTheResponseIsOpen(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		http.NewResponseController(w).Flush()
		<-release
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })

	srv, _ := diagProxy(t, up.URL)

	res, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()

	first := make(chan error, 1)
	go func() {
		buf := make([]byte, len("data: one\n\n"))
		_, err := io.ReadFull(res.Body, buf)
		first <- err
	}()

	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("reading the first flushed chunk: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first chunk never arrived: wrapping the ResponseWriter broke streaming")
	}
}

// A forward that works must stay silent. A diagnostic that logs a line per
// request would drown the one line that matters in a fleet's worth of traffic.
func TestProxy_LogsNothingWhenTheForwardSucceeds(t *testing.T) {
	up, _ := upstream(t)
	srv, captured := diagProxy(t, up.URL)

	res, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res.Body.Close()

	if got := captured.String(); got != "" {
		t.Errorf("a healthy request should log nothing, got:\n%s", got)
	}
}

func TestFirstCut_NamesTheSideThatLeftFirst(t *testing.T) {
	if got := firstCut(false); got != "upstream" {
		t.Errorf("firstCut with the client still connected = %q, want upstream", got)
	}
	if got := firstCut(true); got != "client" {
		t.Errorf("firstCut with the client already gone = %q, want client", got)
	}
}

func TestErrClass_SeparatesCancelFromDeadlineFromTheRest(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "none"},
		{context.Canceled, "context.Canceled"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "context.Canceled"},
		{context.DeadlineExceeded, "context.DeadlineExceeded"},
		{errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), "other"},
	}
	for _, c := range cases {
		if got := errClass(c.err); got != c.want {
			t.Errorf("errClass(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func fieldValue(line, key string) string {
	m := regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `=(\S+)`).FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}
