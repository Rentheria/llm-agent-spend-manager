package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/enforce"
	"github.com/Rentheria/llm-agent-spend-manager/internal/lru"
)

func TestProxy_WithRequestCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	limiter := enforce.New(enforce.NewMemoryCounter(), time.Hour, 1000)
	cache := lru.NewRequestCache(10)

	proxy, err := New(upstream.URL, limiter, WithRequestCache(cache))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Make a request
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Agent", "test-agent")
	req.Header.Set("X-Model", "test-model")

	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify the request was cached
	if cache.Len() != 1 {
		t.Errorf("Cache length = %d, want 1", cache.Len())
	}

	// Verify we can retrieve the cached info
	keys := cache.Recent()
	if len(keys) != 1 {
		t.Fatalf("Recent() returned %d keys, want 1", len(keys))
	}

	info, ok := cache.Lookup(keys[0])
	if !ok {
		t.Fatal("Lookup() failed for cached key")
	}
	if info.Agent != "test-agent" {
		t.Errorf("Agent = %q, want %q", info.Agent, "test-agent")
	}
	if info.Model != "test-model" {
		t.Errorf("Model = %q, want %q", info.Model, "test-model")
	}
}

func TestProxy_WithRequestCacheEviction(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	limiter := enforce.New(enforce.NewMemoryCounter(), time.Hour, 1000)
	cache := lru.NewRequestCache(2) // Small cache to test eviction

	// Use AgentHeaderKey so different X-Agent headers create different keys
	proxy, err := New(upstream.URL, limiter, WithRequestCache(cache), WithKeyFunc(AgentHeaderKey))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Make 3 requests with different agent headers (different keys)
	for i := 1; i <= 3; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		req.Header.Set("X-Agent", fmt.Sprintf("agent-%d", i))

		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Request %d: Status = %d, want %d", i, w.Code, http.StatusOK)
		}
	}

	// Cache should have exactly 2 items (capacity), first one evicted
	if cache.Len() != 2 {
		t.Errorf("Cache length = %d, want 2", cache.Len())
	}

	// Verify agent-1 was evicted
	if _, ok := cache.Lookup("agent:agent-1"); ok {
		t.Error("agent-1 should have been evicted")
	}

	// Verify agent-2 and agent-3 are still present
	if _, ok := cache.Lookup("agent:agent-2"); !ok {
		t.Error("agent-2 should still be present")
	}
	if _, ok := cache.Lookup("agent:agent-3"); !ok {
		t.Error("agent-3 should still be present")
	}
}

func TestProxy_WithoutRequestCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	limiter := enforce.New(enforce.NewMemoryCounter(), time.Hour, 1000)

	// Create proxy without request cache (default)
	proxy, err := New(upstream.URL, limiter)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestProxy_RequestCacheStats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	limiter := enforce.New(enforce.NewMemoryCounter(), time.Hour, 1000)
	cache := lru.NewRequestCache(10)

	proxy, err := New(upstream.URL, limiter, WithRequestCache(cache))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Make a request
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Agent", "test-agent")

	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// Get stats
	_, _, size := cache.Stats()
	if size != 1 {
		t.Errorf("Cache size = %d, want 1", size)
	}

	// Lookup should increment hits
	keys := cache.Recent()
	if len(keys) > 0 {
		cache.Lookup(keys[0])
	}

	hits, _, _ := cache.Stats()
	if hits != 1 {
		t.Errorf("Cache hits = %d, want 1", hits)
	}
}

func TestProxy_RequestCacheRejectedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	limiter := enforce.New(enforce.NewMemoryCounter(), time.Hour, 10)
	cache := lru.NewRequestCache(10)

	proxy, err := New(upstream.URL, limiter, WithRequestCache(cache))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Exhaust the limiter
	ctx := context.Background()
	_, _ = limiter.Allow(ctx, "fleet:default", 100)

	// This request should be rejected
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	// Rejected request should NOT be cached
	if cache.Len() != 0 {
		t.Errorf("Cache length = %d, want 0 (rejected requests should not be cached)", cache.Len())
	}
}

