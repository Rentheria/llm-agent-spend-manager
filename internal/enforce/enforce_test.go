package enforce

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestLimiter spins up an in-memory Redis (miniredis) so the real go-redis
// client and the real Lua script are exercised, without a live server.
func newTestLimiter(t *testing.T, window time.Duration, limit int64) (*Limiter, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(NewRedisCounter(rdb), window, limit), mr, rdb
}

func TestAllow_CountsAndCaps(t *testing.T) {
	lim, _, _ := newTestLimiter(t, time.Minute, 3)
	ctx := context.Background()
	key := "fleet:daily"

	for i := 1; i <= 3; i++ {
		d, err := lim.Allow(ctx, key, 1)
		if err != nil {
			t.Fatalf("Allow #%d error: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("call #%d should be allowed (current=%d, limit=%d)", i, d.Current, d.Limit)
		}
		if d.Current != int64(i) {
			t.Fatalf("call #%d current = %d, want %d", i, d.Current, i)
		}
	}

	// The 4th call crosses the cap.
	d, err := lim.Allow(ctx, key, 1)
	if err != nil {
		t.Fatalf("Allow #4 error: %v", err)
	}
	if d.Allowed {
		t.Fatalf("call #4 should be denied (current=%d, limit=%d)", d.Current, d.Limit)
	}
	if d.Remaining != 0 {
		t.Fatalf("remaining after over-cap = %d, want 0", d.Remaining)
	}
}

func TestAllow_SharedKeyIsCombinedAcrossAgents(t *testing.T) {
	lim, _, _ := newTestLimiter(t, time.Minute, 5)
	ctx := context.Background()
	key := "fleet:daily"

	// Two agents adding different weights to the same key: 3 + 2 == 5 (at cap).
	if d, _ := lim.Allow(ctx, key, 3); !d.Allowed || d.Remaining != 2 {
		t.Fatalf("claude-code: allowed=%v current=%d remaining=%d", d.Allowed, d.Current, d.Remaining)
	}
	if d, _ := lim.Allow(ctx, key, 2); !d.Allowed || d.Current != 5 {
		t.Fatalf("openclaw: allowed=%v current=%d", d.Allowed, d.Current)
	}
	// Anything more is over the combined cap.
	if d, _ := lim.Allow(ctx, key, 1); d.Allowed {
		t.Fatalf("over combined cap should be denied, current=%d", d.Current)
	}
}

// The key has to outlive one window: the sliding window carries the PREVIOUS
// bucket, and expiring at exactly one window would drop it right when it is
// still partly inside the rolling window — reopening the boundary hole the
// sliding window exists to close. Two windows is also the point past which both
// buckets are stale anyway, so the key collects itself without a sweep.
func TestAllow_KeyOutlivesOneWindowSoThePreviousBucketSurvives(t *testing.T) {
	const window = 30 * time.Second
	lim, mr, _ := newTestLimiter(t, window, 100)
	ctx := context.Background()
	key := "fleet:window"

	if _, err := lim.Allow(ctx, key, 1); err != nil {
		t.Fatalf("Allow error: %v", err)
	}

	ttl := mr.TTL(key)
	if ttl <= window {
		t.Errorf("TTL = %v, quería más de una ventana (%v): la cubeta anterior moriría antes de salir del rango", ttl, window)
	}
	if ttl > 2*window {
		t.Errorf("TTL = %v, quería a lo más dos ventanas (%v): más allá de eso ya no aporta al tope", ttl, 2*window)
	}
}

func TestAllow_ZeroLimitNeverBlocks(t *testing.T) {
	lim, _, _ := newTestLimiter(t, time.Minute, 0)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		d, err := lim.Allow(ctx, "unbounded", 1000)
		if err != nil {
			t.Fatalf("Allow error: %v", err)
		}
		if !d.Allowed {
			t.Fatalf("cap<=0 must always allow, got denied at current=%d", d.Current)
		}
	}
}
