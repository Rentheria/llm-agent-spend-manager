package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/enforce"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// upstream is a stand-in LLM API that counts how many calls actually reached it.
func upstream(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	var hits int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok from upstream")
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

func testLimiter(t *testing.T, limit int64) *enforce.Limiter {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return enforce.New(enforce.NewRedisCounter(rdb), time.Minute, limit)
}

func TestProxy_ForwardsUnderCapThenBlocks(t *testing.T) {
	up, hits := upstream(t)
	p, err := New(up.URL, testLimiter(t, 2))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	// Two calls forward and hit the upstream.
	for i := 1; i <= 2; i++ {
		res, err := http.Get(srv.URL + "/v1/messages")
		if err != nil {
			t.Fatalf("request #%d: %v", i, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("request #%d status = %d, want 200", i, res.StatusCode)
		}
	}
	// Third call is over the cap: 429 and the upstream is NOT touched.
	res, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request #3: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request #3 status = %d, want 429", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Errorf("429 should carry a Retry-After header")
	}
	if *hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (the over-cap call must not reach upstream)", *hits)
	}
}

func TestProxy_CapHeadersReported(t *testing.T) {
	up, _ := upstream(t)
	p, _ := New(up.URL, testLimiter(t, 5))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res.Body.Close()
	if got := res.Header.Get("X-Cap-Limit"); got != "5" {
		t.Errorf("X-Cap-Limit = %q, want 5", got)
	}
	if got := res.Header.Get("X-Cap-Remaining"); got != "4" {
		t.Errorf("X-Cap-Remaining = %q, want 4", got)
	}
}

func TestProxy_AmountFuncWeightsRequests(t *testing.T) {
	up, hits := upstream(t)
	// Each request weighs 3; cap is 5, so the first passes (3) and the second
	// crosses (6 > 5).
	p, _ := New(up.URL, testLimiter(t, 5),
		WithAmountFunc(func(*http.Request) int64 { return 3 }))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	if res, _ := http.Get(srv.URL); res.StatusCode != http.StatusOK {
		t.Fatalf("first weighted call status = %d, want 200", res.StatusCode)
	}
	if res, _ := http.Get(srv.URL); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second weighted call status = %d, want 429", res.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}
}

func TestProxy_FailsOpenWhenRedisDown(t *testing.T) {
	up, hits := upstream(t)
	// Point the limiter at a Redis that is closed immediately, so Allow errors.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close() // coordinator is now unreachable
	p, _ := New(up.URL, enforce.New(enforce.NewRedisCounter(rdb), time.Minute, 1))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open when Redis is down)", res.StatusCode)
	}
	if res.Header.Get("X-Cap-Enforced") != "fail-open" {
		t.Errorf("expected fail-open marker header")
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (fail-open forwards)", *hits)
	}
}

func TestNew_RejectsRelativeTarget(t *testing.T) {
	if _, err := New("/not-absolute", testLimiter(t, 1)); err == nil {
		t.Fatalf("expected error for a non-absolute target URL")
	}
}

// The measured cost driver is context size, not call count: a turn dragging a
// huge history and a two-line question weigh the same under OnePerRequest, which
// is exactly the distinction a cache-read-dominated fleet needs the cap to make.
func TestProxy_ContextBytesAmountBlocksTheBigContextNotTheCallCount(t *testing.T) {
	up, hits := upstream(t)
	// Cap of 1000 bytes of context. A small call passes; the next big one crosses.
	p, _ := New(up.URL, testLimiter(t, 1000), WithAmountFunc(ContextBytesAmount))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	small, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(strings.Repeat("a", 100)))
	if err != nil {
		t.Fatalf("small request: %v", err)
	}
	small.Body.Close()
	if small.StatusCode != http.StatusOK {
		t.Fatalf("small request status = %d, want 200 (100 bytes is well under the cap)", small.StatusCode)
	}

	big, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(strings.Repeat("a", 2000)))
	if err != nil {
		t.Fatalf("big request: %v", err)
	}
	big.Body.Close()
	if big.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("big request status = %d, want 429 (100+2000 bytes is over the 1000-byte cap)", big.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1: the over-cap call must not be billed", *hits)
	}
}

func TestContextBytesAmount_WeighsTheDeclaredBodyLength(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("x", 4096)))

	if got := ContextBytesAmount(r); got != 4096 {
		t.Errorf("ContextBytesAmount = %d, want 4096", got)
	}
}

func TestContextBytesAmount_WeighsNothingWhenTheLengthIsUndeclared(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.ContentLength = -1

	if got := ContextBytesAmount(r); got != 0 {
		t.Errorf("ContextBytesAmount = %d, want 0 for an undeclared length", got)
	}
}
