package lru

import (
	"testing"
	"time"
)

func TestRequestCacheRecord(t *testing.T) {
	rc := NewRequestCache(10)

	info := RequestInfo{
		Key:        "req-123",
		Timestamp:  time.Now(),
		TokenCount: 1500,
		Model:      "claude-opus-4",
		Agent:      "Claude Code",
	}

	rc.Record(info)

	got, ok := rc.Lookup("req-123")
	if !ok {
		t.Fatal("Lookup(req-123) should return true")
	}
	if got.Key != info.Key {
		t.Errorf("Key = %q, want %q", got.Key, info.Key)
	}
	if got.TokenCount != info.TokenCount {
		t.Errorf("TokenCount = %d, want %d", got.TokenCount, info.TokenCount)
	}
}

func TestRequestCacheLookupMissing(t *testing.T) {
	rc := NewRequestCache(10)

	_, ok := rc.Lookup("missing")
	if ok {
		t.Error("Lookup(missing) should return false")
	}
}

func TestRequestCacheStats(t *testing.T) {
	rc := NewRequestCache(10)

	info1 := RequestInfo{Key: "req-1", Timestamp: time.Now()}
	info2 := RequestInfo{Key: "req-2", Timestamp: time.Now()}

	rc.Record(info1)
	rc.Record(info2)

	// Hit
	rc.Lookup("req-1")
	// Miss
	rc.Lookup("missing")

	hits, misses, size := rc.Stats()
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1", misses)
	}
	if size != 2 {
		t.Errorf("size = %d, want 2", size)
	}
}

func TestRequestCacheRecent(t *testing.T) {
	rc := NewRequestCache(10)

	info1 := RequestInfo{Key: "req-1", Timestamp: time.Now()}
	info2 := RequestInfo{Key: "req-2", Timestamp: time.Now()}
	info3 := RequestInfo{Key: "req-3", Timestamp: time.Now()}

	rc.Record(info1)
	rc.Record(info2)
	rc.Record(info3)

	recent := rc.Recent()
	want := []string{"req-3", "req-2", "req-1"} // MRU to LRU

	if len(recent) != len(want) {
		t.Fatalf("Recent() length = %d, want %d", len(recent), len(want))
	}
	for i := range recent {
		if recent[i] != want[i] {
			t.Errorf("Recent()[%d] = %q, want %q", i, recent[i], want[i])
		}
	}
}

func TestRequestCacheTouch(t *testing.T) {
	rc := NewRequestCache(3)

	info1 := RequestInfo{Key: "req-1", Timestamp: time.Now()}
	info2 := RequestInfo{Key: "req-2", Timestamp: time.Now()}
	info3 := RequestInfo{Key: "req-3", Timestamp: time.Now()}

	rc.Record(info1)
	rc.Record(info2)
	rc.Record(info3)

	// Touch req-1, moving it to head
	if !rc.Touch("req-1") {
		t.Error("Touch(req-1) should return true")
	}

	// Add req-4, should evict req-2 (now LRU)
	info4 := RequestInfo{Key: "req-4", Timestamp: time.Now()}
	rc.Record(info4)

	if _, ok := rc.Lookup("req-2"); ok {
		t.Error("req-2 should have been evicted")
	}
	if _, ok := rc.Lookup("req-1"); !ok {
		t.Error("req-1 should still be present (was touched)")
	}
}

func TestRequestCacheEviction(t *testing.T) {
	rc := NewRequestCache(2)

	info1 := RequestInfo{Key: "req-1", Timestamp: time.Now()}
	info2 := RequestInfo{Key: "req-2", Timestamp: time.Now()}
	info3 := RequestInfo{Key: "req-3", Timestamp: time.Now()}

	rc.Record(info1)
	rc.Record(info2)
	rc.Record(info3) // Should evict req-1

	if _, ok := rc.Lookup("req-1"); ok {
		t.Error("req-1 should have been evicted")
	}
	if _, ok := rc.Lookup("req-2"); !ok {
		t.Error("req-2 should still be present")
	}
	if _, ok := rc.Lookup("req-3"); !ok {
		t.Error("req-3 should be present")
	}
}

func TestRequestCacheClear(t *testing.T) {
	rc := NewRequestCache(10)

	info := RequestInfo{Key: "req-1", Timestamp: time.Now()}
	rc.Record(info)
	rc.Lookup("req-1") // Generate a hit

	rc.Clear()

	hits, misses, size := rc.Stats()
	if hits != 0 || misses != 0 || size != 0 {
		t.Errorf("After Clear: hits=%d, misses=%d, size=%d, want all 0", hits, misses, size)
	}

	if _, ok := rc.Lookup("req-1"); ok {
		t.Error("req-1 should not be present after Clear")
	}
}

func TestRequestCacheCapacity(t *testing.T) {
	rc := NewRequestCache(42)
	if got := rc.Capacity(); got != 42 {
		t.Errorf("Capacity() = %d, want 42", got)
	}
}

func TestNewRequestCacheFromEnv(t *testing.T) {
	rc := NewRequestCacheFromEnv()
	if rc == nil {
		t.Fatal("NewRequestCacheFromEnv() returned nil")
	}
	cap := rc.Capacity()
	if cap <= 0 {
		t.Errorf("Capacity() = %d, should be positive", cap)
	}
}
