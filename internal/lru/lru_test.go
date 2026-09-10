package lru

import (
	"testing"
)

func TestNew(t *testing.T) {
	c := New(5)
	if got := c.Capacity(); got != 5 {
		t.Errorf("Capacity() = %d, want 5", got)
	}
	if got := c.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestNewZeroCapacity(t *testing.T) {
	c := New(0)
	if got := c.Capacity(); got <= 0 {
		t.Errorf("Capacity() = %d, should be positive", got)
	}
}

func TestPutAndGet(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	if got, ok := c.Get("a"); !ok || got != 1 {
		t.Errorf("Get(a) = (%v, %v), want (1, true)", got, ok)
	}
	if got, ok := c.Get("b"); !ok || got != 2 {
		t.Errorf("Get(b) = (%v, %v), want (2, true)", got, ok)
	}
	if got, ok := c.Get("c"); !ok || got != 3 {
		t.Errorf("Get(c) = (%v, %v), want (3, true)", got, ok)
	}
}

func TestGetMissing(t *testing.T) {
	c := New(3)
	if got, ok := c.Get("missing"); ok {
		t.Errorf("Get(missing) = (%v, %v), want (nil, false)", got, ok)
	}
}

func TestUpdateExisting(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("a", 10)

	if got, ok := c.Get("a"); !ok || got != 10 {
		t.Errorf("Get(a) = (%v, %v), want (10, true)", got, ok)
	}
	if got := c.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
}

func TestEviction(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	c.Put("d", 4) // Should evict "a" (LRU)

	if _, ok := c.Get("a"); ok {
		t.Error("Get(a) should return false after eviction")
	}
	if got, ok := c.Get("b"); !ok || got != 2 {
		t.Errorf("Get(b) = (%v, %v), want (2, true)", got, ok)
	}
	if got, ok := c.Get("c"); !ok || got != 3 {
		t.Errorf("Get(c) = (%v, %v), want (3, true)", got, ok)
	}
	if got, ok := c.Get("d"); !ok || got != 4 {
		t.Errorf("Get(d) = (%v, %v), want (4, true)", got, ok)
	}
	if got := c.Len(); got != 3 {
		t.Errorf("Len() = %d, want 3", got)
	}
}

func TestGetMovesToHead(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	// Access "a", moving it to head
	c.Get("a")

	// Add "d", which should evict "b" (now LRU)
	c.Put("d", 4)

	if _, ok := c.Get("b"); ok {
		t.Error("Get(b) should return false after eviction")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("Get(a) should return true (was moved to head)")
	}
}

func TestTouch(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	// Touch "a", moving it to head
	if !c.Touch("a") {
		t.Error("Touch(a) should return true")
	}

	// Add "d", which should evict "b" (now LRU)
	c.Put("d", 4)

	if _, ok := c.Get("b"); ok {
		t.Error("Get(b) should return false after eviction")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("Get(a) should return true (was touched)")
	}
}

func TestTouchMissing(t *testing.T) {
	c := New(3)
	if c.Touch("missing") {
		t.Error("Touch(missing) should return false")
	}
}

func TestDelete(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	if !c.Delete("b") {
		t.Error("Delete(b) should return true")
	}
	if c.Delete("b") {
		t.Error("Delete(b) second time should return false")
	}
	if got := c.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if _, ok := c.Get("b"); ok {
		t.Error("Get(b) should return false after deletion")
	}
}

func TestDeleteHead(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	// Delete head (most recent)
	c.Delete("c")

	if got := c.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if _, ok := c.Get("c"); ok {
		t.Error("Get(c) should return false after deletion")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("Get(a) should still be present")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("Get(b) should still be present")
	}
}

func TestDeleteTail(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	// Delete tail (least recent)
	c.Delete("a")

	if got := c.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if _, ok := c.Get("a"); ok {
		t.Error("Get(a) should return false after deletion")
	}
}

func TestClear(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	c.Clear()

	if got := c.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0 after Clear", got)
	}
	if _, ok := c.Get("a"); ok {
		t.Error("Get(a) should return false after Clear")
	}
	if _, ok := c.Get("b"); ok {
		t.Error("Get(b) should return false after Clear")
	}
	if _, ok := c.Get("c"); ok {
		t.Error("Get(c) should return false after Clear")
	}
}

func TestKeys(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	keys := c.Keys()
	want := []string{"c", "b", "a"} // MRU to LRU

	if len(keys) != len(want) {
		t.Fatalf("Keys() length = %d, want %d", len(keys), len(want))
	}
	for i := range keys {
		if keys[i] != want[i] {
			t.Errorf("Keys()[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

func TestKeysAfterAccess(t *testing.T) {
	c := New(3)

	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	// Access "a", moving it to head
	c.Get("a")

	keys := c.Keys()
	want := []string{"a", "c", "b"} // MRU to LRU after access

	if len(keys) != len(want) {
		t.Fatalf("Keys() length = %d, want %d", len(keys), len(want))
	}
	for i := range keys {
		if keys[i] != want[i] {
			t.Errorf("Keys()[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

func TestOrderAfterMultipleOperations(t *testing.T) {
	c := New(4)

	// Add a, b, c
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	// Access b (moves to head)
	c.Get("b")

	// Add d
	c.Put("d", 4)

	keys := c.Keys()
	want := []string{"d", "b", "c", "a"} // MRU to LRU

	if len(keys) != len(want) {
		t.Fatalf("Keys() length = %d, want %d", len(keys), len(want))
	}
	for i := range keys {
		if keys[i] != want[i] {
			t.Errorf("Keys()[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

func TestSingleItemCache(t *testing.T) {
	c := New(1)

	c.Put("a", 1)
	if got, ok := c.Get("a"); !ok || got != 1 {
		t.Errorf("Get(a) = (%v, %v), want (1, true)", got, ok)
	}

	c.Put("b", 2)
	if _, ok := c.Get("a"); ok {
		t.Error("Get(a) should return false after eviction")
	}
	if got, ok := c.Get("b"); !ok || got != 2 {
		t.Errorf("Get(b) = (%v, %v), want (2, true)", got, ok)
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New(100)

	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 50; i++ {
			c.Put("key", i)
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 50; i++ {
			c.Get("key")
		}
		done <- true
	}()

	// Wait for both
	<-done
	<-done

	// Cache should be in a valid state
	if got := c.Len(); got < 0 || got > c.Capacity() {
		t.Errorf("Invalid Len() = %d after concurrent access", got)
	}
}
