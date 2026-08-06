package proxy

// Cut diagnosis (admin-A5, 2026-08-01). Why this file exists:
//
// On 31-jul the `claude` CLI died twice with its catch-all message ("Execution
// error", 15 bytes of log; the real detail only ever reaches Anthropic's own
// telemetry) and `lasm-proxy` logged `proxy error: context canceled` within the
// same second of each death. That line comes from httputil.ReverseProxy's
// DEFAULT ErrorHandler, which knows how to say the error and nothing else.
//
// One line cannot tell cause from symptom. In Go, `context canceled` inside a
// ReverseProxy means the request's context was cancelled, and that happens both
// when the upstream breaks and when the CLIENT (`claude` itself) walks away
// first because it was already dying on its own. So this file fixes nothing: it
// adds the fields that will let the NEXT occurrence say which side cut first,
// when, and whether the stream had already started. Zero behaviour change —
// remedying without knowing the cause is exactly the mistake this ticket exists
// to avoid.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// diagKeyType is the private key the diagnostic record travels under, from
// ServeHTTP down to the ErrorHandler (which is handed the OUTBOUND request, a
// clone of the inbound one, so the context value still arrives).
type diagKeyType struct{}

var diagKey diagKeyType

// requestDiag is what the proxy remembers about an in-flight request so its
// death can be classified. Every field is read from a goroutine other than the
// one that wrote it (the ErrorHandler, context.AfterFunc, the body copy), hence
// the atomics.
type requestDiag struct {
	start        time.Time
	method       string
	path         string
	clientGoneAt atomic.Int64 // unix nanos the inbound context was cancelled at; 0 = still live
	status       atomic.Int64 // first status written to the client; 0 = the response never started
	written      atomic.Int64 // body bytes already handed to the client
}

func newRequestDiag(r *http.Request) *requestDiag {
	return &requestDiag{
		start:  time.Now(),
		method: r.Method,
		// Path only: an LLM API's query string can carry credentials, and this
		// goes to a log read with journalctl.
		path: r.URL.Path,
	}
}

// markClientGone dates the cancellation of the inbound request's context. It is
// registered with context.AfterFunc, which leaves no goroutine parked per
// request.
func (d *requestDiag) markClientGone() {
	d.clientGoneAt.CompareAndSwap(0, time.Now().UnixNano())
}

// clientGone reports how long after the request came in the client left, and
// whether it left at all.
func (d *requestDiag) clientGone() (time.Duration, bool) {
	ns := d.clientGoneAt.Load()
	if ns == 0 {
		return 0, false
	}
	return time.Unix(0, ns).Sub(d.start), true
}

// billable reports whether this was the kind of call that should have come back
// with a token usage report. It exists so a completion that reported none gets
// said out loud (usage.go) while the health-checks, the token-count endpoint and
// every other path stay quiet — a warning that fires on all of them is a warning
// nobody reads.
func (d *requestDiag) billable() bool {
	return d.method == http.MethodPost && strings.HasSuffix(d.path, "/messages")
}

// fields renders the key=value tail appended to a failed forward. One line, no
// new dependency: this repo has no structured logger, and adding one for nine
// fields would be a bigger change than the diagnosis it serves.
func (d *requestDiag) fields(err, ctxErr error) string {
	elapsed := time.Since(d.start)
	if ctxErr != nil {
		// The AfterFunc that dates the hang-up runs on its own goroutine, so on a
		// fast cut the classification can get here before it was ever scheduled —
		// and the line came out saying first_cut=upstream right next to
		// ctx_err=context.Canceled, which is the same event read two ways. The
		// context error is the authority: if the inbound context is already done,
		// the client is gone, whatever the goroutine has managed to run. Stamping
		// it here is a hair late (microseconds) and never wrong.
		d.markClientGone()
	}
	gone, wentAway := d.clientGone()

	// Rounded to microseconds rather than milliseconds: the question that
	// settles the diagnosis is how long BEFORE the error the client left, and
	// on most cuts that distance lives under a millisecond.
	goneField := "none"
	if wentAway {
		goneField = gone.Round(time.Microsecond).String()
	}

	f := []string{
		"class=" + errClass(err),
		"first_cut=" + firstCut(wentAway),
		// The class, not the error text: `context canceled` carries a space and
		// would split a line meant to be read as key=value pairs. The full text
		// is already at the head of the line.
		"ctx_err=" + errClass(ctxErr),
		"elapsed=" + elapsed.Round(time.Microsecond).String(),
		"client_gone_after=" + goneField,
		fmt.Sprintf("resp_status=%d", d.status.Load()),
		fmt.Sprintf("client_bytes=%d", d.written.Load()),
		"method=" + d.method,
		fmt.Sprintf("path=%q", d.path),
	}
	return strings.Join(f, " ")
}

// firstCut is the reading this whole ticket exists to make possible: if the
// INBOUND request's context was already cancelled when the forward broke, the
// side that left first was the client (`claude` dying on its own) and the proxy
// is the visible victim, not the cause. If the error arrived with the client
// still connected, the cut came from the upstream or from the proxy itself.
//
// The signal is clean because nobody else cancels that context: the
// ReverseProxy derives its own for the outbound call, and net/http cancels the
// inbound one only when the client disconnects or once the handler has already
// returned — by which point the AfterFunc has been unregistered. Even so, read
// the verdict next to client_gone_after and elapsed, which give the exact
// margin: a gap of microseconds deserves less confidence than one of seconds.
func firstCut(wentAway bool) string {
	if wentAway {
		return "client"
	}
	return "upstream"
}

// errClass separates the three ways to die that read differently in a log but
// look alike at a glance: an explicit cancellation, an expired deadline, and
// everything else (DNS, TLS, connection refused).
func errClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "net.timeout"
	}
	return "other"
}

// diagWriter counts what actually made it out to the client, which is what
// tells "cut mid-stream" apart from "never got to connect".
type diagWriter struct {
	http.ResponseWriter
	d *requestDiag
}

// Unwrap lets http.ResponseController reach the real ResponseWriter. Without
// it, wrapping would take away the Flush and Hijack the ReverseProxy streams
// with — that is, observing would have changed behaviour, the one thing this
// ticket must not do.
func (w *diagWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *diagWriter) WriteHeader(code int) {
	w.d.status.CompareAndSwap(0, int64(code))
	w.ResponseWriter.WriteHeader(code)
}

func (w *diagWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if n > 0 {
		w.d.written.Add(int64(n))
		w.d.status.CompareAndSwap(0, http.StatusOK)
	}
	return n, err
}

// errorHandler replaces the ReverseProxy default, which only does
// log.Printf("http: proxy error: %v") + 502. It keeps that prefix verbatim on
// purpose — it is the string journalctl is already grepped for and the one the
// July incidents show — and appends the classification behind it. The answer to
// the client is the same 502 as always.
func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if d, ok := r.Context().Value(diagKey).(*requestDiag); ok {
		p.logf("http: proxy error: %v | %s", err, d.fields(err, r.Context().Err()))
	} else {
		p.logf("http: proxy error: %v", err)
	}
	w.WriteHeader(http.StatusBadGateway)
}

// forward runs the ReverseProxy with the instrumentation in place.
//
// The recover is neither decoration nor a swallowed error: ReverseProxy does
// NOT call ErrorHandler once the response has started and the body copy fails
// halfway — it panics with http.ErrAbortHandler, which net/http recovers
// silently, leaving no trace in the log at all. That is precisely the "cut
// mid-stream" case, the closest thing to what a long `claude` answer runs into.
// Here it is logged and the panic is re-raised untouched, so the server still
// aborts the connection exactly as it does today.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, d *requestDiag) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		if rec == http.ErrAbortHandler {
			// ReverseProxy does not carry the copy error into the panic, so the
			// context's error is the closest thing available.
			ctxErr := r.Context().Err()
			p.logf("http: proxy error: response aborted mid-stream | %s", d.fields(ctxErr, ctxErr))
		}
		panic(rec)
	}()

	r = r.WithContext(context.WithValue(r.Context(), diagKey, d))
	p.rp.ServeHTTP(&diagWriter{ResponseWriter: w, d: d}, r)
}

// logf writes where the proxy already writes today: the ReverseProxy's ErrorLog
// when someone set one, and the standard logger (stderr) otherwise, which is
// what `journalctl --user -u lasm-proxy` collects. The new detail shows up next
// to the old line instead of in a sink nobody is watching.
func (p *Proxy) logf(format string, args ...any) {
	if p.rp.ErrorLog != nil {
		p.rp.ErrorLog.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}
