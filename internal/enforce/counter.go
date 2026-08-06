package enforce

import (
	"context"
	"sync"
	"time"
)

// Counter is the shared tally a Limiter enforces its cap against. Add must be
// atomic with respect to concurrent callers: the increment is what makes the
// check safe when two near-simultaneous requests sit near the cap.
type Counter interface {
	// Add adds amount to key's rolling window and returns the window total after
	// the increment — a sliding-window estimate, not a raw sum (see window.go).
	Add(ctx context.Context, key string, amount int64, window time.Duration) (int64, error)
}

// MemoryCounter tallies in the proxy's own memory. Nothing to install and
// nothing to coordinate, but the tally dies with the process: use it for a quick
// run or a throwaway proxy. For anything long-lived prefer SQLiteCounter, which
// costs the same to install and survives a restart — a cap that resets on every
// upgrade, crash or reboot is a cap the fleet can walk around.
type MemoryCounter struct {
	mu      sync.Mutex
	windows map[string]windowState
	// now is injectable so expiry can be tested without sleeping.
	now func() time.Time
}

// NewMemoryCounter returns an empty in-memory counter.
func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{
		windows: make(map[string]windowState),
		now:     time.Now,
	}
}

// Add implements Counter.
func (c *MemoryCounter) Add(_ context.Context, key string, amount int64, window time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	bucket := bucketIndex(now, window)
	c.dropStale(bucket)

	st := advance(c.windows[key], bucket, amount)
	c.windows[key] = st
	return estimate(st, now, window), nil
}

// dropStale frees keys whose two buckets are both behind the window, so a
// long-running proxy does not leak memory when keys are dynamic (per-agent keys,
// for instance). Callers hold the lock.
func (c *MemoryCounter) dropStale(bucket int64) {
	for key, st := range c.windows {
		if bucket-st.bucket > 1 {
			delete(c.windows, key)
		}
	}
}
