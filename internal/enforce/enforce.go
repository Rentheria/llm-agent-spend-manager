// Package enforce is the optional hard-cap layer (architecture.md §3.2): a
// combined budget cap shared across agents that don't know about each other.
//
// The cap is a SLIDING window (window.go), not a fixed one — a fixed window lets
// ~2x the cap through at the boundary, which is the exact failure a budget cap
// exists to prevent.
//
// Where the tally lives is a separate choice, behind the Counter interface:
// SQLiteCounter is the default (nothing to install, survives a restart,
// coordinates across processes on one machine), MemoryCounter is the throwaway,
// and RedisCounter is the opt-in for the one case neither covers — a cap shared
// across machines.
package enforce

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowCapLua keeps the whole read-modify-write in one Lua execution, so
// two near-simultaneous requests near the cap can never both read a stale value
// and overshoot it. It is `advance` (window.go) expressed in Lua — same three
// cases, same order — over a hash holding the two buckets.
//
// This DIVERGES from llm-budget-cap, which we originally reused verbatim. That
// script is a fixed window (INCRBY + PEXPIRE), and a fixed window lets ~2x the
// cap through at the boundary; see window.go. The same bug is still open in
// llm-budget-cap.
//
// The key expires two windows out, so a key that goes quiet is collected on its
// own without a sweep. Only PEXPIRE, no PTTL branch: refreshing it on every
// write is correct here because the buckets carry their own staleness.
//
//	KEYS[1] = counter key
//	ARGV[1] = window length in milliseconds
//	ARGV[2] = amount to add this call
//	ARGV[3] = current bucket index
const slidingWindowCapLua = `
local bucket = tonumber(ARGV[3])
local amount = tonumber(ARGV[2])
local st = redis.call("HMGET", KEYS[1], "bucket", "current", "previous")
local prevBucket = tonumber(st[1])
local current, previous
if prevBucket == nil then
  current, previous = amount, 0
elseif bucket - prevBucket == 0 then
  current, previous = tonumber(st[2]) + amount, tonumber(st[3])
elseif bucket - prevBucket == 1 then
  current, previous = amount, tonumber(st[2])
else
  current, previous = amount, 0
end
redis.call("HSET", KEYS[1], "bucket", bucket, "current", current, "previous", previous)
redis.call("PEXPIRE", KEYS[1], ARGV[1] * 2)
return {current, previous}
`

// RedisCounter tallies in Redis so proxy instances on DIFFERENT MACHINES can
// share one cap. On a single machine SQLiteCounter already coordinates across
// processes and costs nothing to install — reach for this only when the cap must
// cross the network, and then loopback/auth rules apply (L-06).
type RedisCounter struct {
	rdb    redis.Scripter
	script *redis.Script
	// now is injectable so window rollover can be tested without sleeping.
	now func() time.Time
}

// NewRedisCounter wraps any go-redis client (real, cluster, or one pointed at
// miniredis in tests).
func NewRedisCounter(rdb redis.Scripter) *RedisCounter {
	return &RedisCounter{rdb: rdb, script: redis.NewScript(slidingWindowCapLua), now: time.Now}
}

// Add implements Counter.
func (c *RedisCounter) Add(ctx context.Context, key string, amount int64, window time.Duration) (int64, error) {
	now := c.now()
	res, err := c.script.Run(ctx, c.rdb, []string{key},
		window.Milliseconds(), amount, bucketIndex(now, window)).Int64Slice()
	if err != nil {
		return 0, err
	}
	if len(res) != 2 {
		return 0, fmt.Errorf("el script del tope devolvió %d valores, esperaba 2", len(res))
	}
	return estimate(windowState{current: res[0], previous: res[1]}, now, window), nil
}

// Limiter enforces a combined cap over a rolling window. The whole point of the
// shared key is that several agents increment the same counter, so the cap is
// on the fleet, not per-agent.
type Limiter struct {
	counter Counter
	window  time.Duration
	limit   int64
}

// New builds a Limiter over any Counter. limit is the maximum the counter may
// reach within window (both interpreted in the caller's chosen unit — requests,
// context bytes, or micro-dollars; the proxy decides via its amount function).
func New(counter Counter, window time.Duration, limit int64) *Limiter {
	return &Limiter{counter: counter, window: window, limit: limit}
}

// Decision is the outcome of a single Allow call.
type Decision struct {
	// Allowed is true when the post-increment counter is still within the cap.
	Allowed bool
	// Current is the counter value after adding amount.
	Current int64
	// Limit is the configured cap, echoed for convenience (e.g. headers).
	Limit int64
	// Remaining is how much headroom is left (never negative).
	Remaining int64
}

// Allow adds amount to the shared counter under key and reports whether the cap
// is still respected. A cap of <= 0 means "no limit": it still counts (so usage
// stays observable) but always allows.
//
// Note this counts first, then checks — the increment is what makes the check
// atomic across concurrent callers. An over-cap call is reflected in Current;
// callers that must not let usage exceed the cap should reject the request when
// Allowed is false and not perform the underlying (billable) work.
func (l *Limiter) Allow(ctx context.Context, key string, amount int64) (Decision, error) {
	current, err := l.counter.Add(ctx, key, amount, l.window)
	if err != nil {
		return Decision{}, err
	}

	d := Decision{Current: current, Limit: l.limit}
	if l.limit <= 0 {
		d.Allowed = true
		return d, nil
	}
	d.Allowed = current <= l.limit
	if rem := l.limit - current; rem > 0 {
		d.Remaining = rem
	}
	return d, nil
}
