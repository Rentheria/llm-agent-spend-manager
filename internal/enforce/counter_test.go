package enforce

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The same sliding window is written three times in three languages: Go
// (window.go), SQL (upsertWindowSQL) and Lua (slidingWindowCapLua). Drift between
// them would be invisible — each backend passing its own tests while disagreeing
// with the others — so every backend runs the identical suite below, with a
// controllable clock.

// clockedCounter is a Counter whose notion of "now" the test drives.
type clockedCounter struct {
	Counter
	set func(time.Time)
}

// counterBackends returns one fresh instance of every backend.
func counterBackends(t *testing.T) map[string]func(*testing.T) clockedCounter {
	t.Helper()
	return map[string]func(*testing.T) clockedCounter{
		"memory": func(t *testing.T) clockedCounter {
			c := NewMemoryCounter()
			return clockedCounter{Counter: c, set: func(now time.Time) { c.now = func() time.Time { return now } }}
		},
		"sqlite": func(t *testing.T) clockedCounter {
			c, err := NewSQLiteCounter(filepath.Join(t.TempDir(), "cap.db"))
			if err != nil {
				t.Fatalf("NewSQLiteCounter: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			return clockedCounter{Counter: c, set: func(now time.Time) { c.now = func() time.Time { return now } }}
		},
		"redis": func(t *testing.T) clockedCounter {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			c := NewRedisCounter(rdb)
			return clockedCounter{Counter: c, set: func(now time.Time) { c.now = func() time.Time { return now } }}
		},
	}
}

// atBucket returns an instant `fraction` of the way into the given bucket.
func atBucket(bucket int64, fraction float64, window time.Duration) time.Time {
	ms := window.Milliseconds()
	return time.UnixMilli(bucket*ms + int64(fraction*float64(ms)))
}

func forEachBackend(t *testing.T, run func(*testing.T, clockedCounter)) {
	t.Helper()
	for name, build := range counterBackends(t) {
		t.Run(name, func(t *testing.T) { run(t, build(t)) })
	}
}

func TestCounter_AccumulatesWithinTheWindow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, c clockedCounter) {
		const window = time.Hour
		c.set(atBucket(100, 0.1, window))
		ctx := context.Background()

		for i, want := range []int64{100, 300, 600} {
			got, err := c.Add(ctx, "fleet:default", int64((i+1)*100), window)
			if err != nil {
				t.Fatalf("Add #%d: %v", i, err)
			}
			if got != want {
				t.Errorf("Add #%d = %d, quería %d", i, got, want)
			}
		}
	})
}

// A combined cap is only combined because separate keys stay separate: if
// agent:claude-code and agent:openclaw shared a tally, --key agent would silently behave like
// --key fleet.
func TestCounter_KeepsKeysIndependent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, c clockedCounter) {
		const window = time.Hour
		c.set(atBucket(100, 0.1, window))
		ctx := context.Background()

		if _, err := c.Add(ctx, "agent:claude-code", 500, window); err != nil {
			t.Fatal(err)
		}
		got, err := c.Add(ctx, "agent:openclaw", 70, window)
		if err != nil {
			t.Fatal(err)
		}
		if got != 70 {
			t.Errorf("agent:openclaw = %d, quería 70: se mezcló con el contador de otro agente", got)
		}
	})
}

// The regression that motivated the rewrite: with a fixed window, spending the
// whole cap just before the boundary and again just after let through 2x.
func TestCounter_DoesNotReleaseTheWholeBudgetAtTheBoundary(t *testing.T) {
	forEachBackend(t, func(t *testing.T, c clockedCounter) {
		const window = time.Hour
		ctx := context.Background()

		c.set(atBucket(100, 0.95, window))
		if _, err := c.Add(ctx, "fleet:default", 1000, window); err != nil {
			t.Fatal(err)
		}

		// Just over the boundary into the next bucket.
		c.set(atBucket(101, 0.01, window))
		got, err := c.Add(ctx, "fleet:default", 0, window)
		if err != nil {
			t.Fatal(err)
		}
		if got == 0 {
			t.Fatal("tras la frontera el contador quedó en 0: la ventana fija regalaba el presupuesto entero")
		}
		if got < 950 {
			t.Errorf("tras la frontera = %d, quería ~1000: el gasto de hace un minuto sigue contando", got)
		}
	})
}

// A key quiet for more than the whole window starts over — otherwise a cap that
// only ever grew would block the fleet permanently after one busy day.
func TestCounter_ResetsAfterTwoWindowsOfSilence(t *testing.T) {
	forEachBackend(t, func(t *testing.T, c clockedCounter) {
		const window = time.Hour
		ctx := context.Background()

		c.set(atBucket(100, 0.5, window))
		if _, err := c.Add(ctx, "fleet:default", 900, window); err != nil {
			t.Fatal(err)
		}

		c.set(atBucket(103, 0.5, window))
		got, err := c.Add(ctx, "fleet:default", 10, window)
		if err != nil {
			t.Fatal(err)
		}
		if got != 10 {
			t.Errorf("tras dos ventanas en silencio = %d, quería 10 (arrancar de cero)", got)
		}
	})
}

// The carried weight decays as the old bucket slides out, so the budget is
// released gradually instead of in one jump.
func TestCounter_DecaysThePreviousWindowGradually(t *testing.T) {
	forEachBackend(t, func(t *testing.T, c clockedCounter) {
		const window = time.Hour
		ctx := context.Background()

		c.set(atBucket(100, 0.5, window))
		if _, err := c.Add(ctx, "fleet:default", 1000, window); err != nil {
			t.Fatal(err)
		}

		c.set(atBucket(101, 0.25, window))
		quarter, err := c.Add(ctx, "fleet:default", 0, window)
		if err != nil {
			t.Fatal(err)
		}
		c.set(atBucket(101, 0.75, window))
		threeQuarters, err := c.Add(ctx, "fleet:default", 0, window)
		if err != nil {
			t.Fatal(err)
		}

		if quarter != 750 {
			t.Errorf("al 25%% de la ventana siguiente = %d, quería 750", quarter)
		}
		if threeQuarters != 250 {
			t.Errorf("al 75%% de la ventana siguiente = %d, quería 250", threeQuarters)
		}
	})
}

// The increment is what makes the cap safe under concurrency: the proxy serves
// every agent from one process, so parallel requests hit the counter directly.
// Run with -race.
func TestCounter_IsSafeUnderConcurrentAdds(t *testing.T) {
	forEachBackend(t, func(t *testing.T, c clockedCounter) {
		const window = time.Hour
		const goroutines, addsEach = 25, 20
		c.set(atBucket(100, 0.1, window))
		ctx := context.Background()

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()
				for range addsEach {
					if _, err := c.Add(ctx, "fleet:default", 1, window); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()

		got, err := c.Add(ctx, "fleet:default", 0, window)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(goroutines * addsEach); got != want {
			t.Errorf("total = %d, quería %d: se perdieron incrementos", got, want)
		}
	})
}

// The whole reason SQLite is the default over memory: a proxy that runs as a
// service restarts on every upgrade, crash and reboot, and a cap that forgets on
// restart is a cap the fleet can walk around.
func TestSQLiteCounter_SurvivesAReopen(t *testing.T) {
	const window = time.Hour
	path := filepath.Join(t.TempDir(), "cap.db")
	now := atBucket(100, 0.25, window)
	ctx := context.Background()

	first, err := NewSQLiteCounter(path)
	if err != nil {
		t.Fatal(err)
	}
	first.now = func() time.Time { return now }
	if _, err := first.Add(ctx, "fleet:default", 700, window); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := NewSQLiteCounter(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	second.now = func() time.Time { return now }

	got, err := second.Add(ctx, "fleet:default", 100, window)
	if err != nil {
		t.Fatal(err)
	}
	if got != 800 {
		t.Errorf("tras reabrir = %d, quería 800: la ventana no sobrevivió al reinicio", got)
	}
}

// Per-agent keys are unbounded in principle; a process meant to run for months
// must not accumulate a row per key ever seen.
func TestSQLiteCounter_DropsRowsOlderThanTheWindow(t *testing.T) {
	const window = time.Hour
	c, err := NewSQLiteCounter(filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	c.now = func() time.Time { return atBucket(100, 0.5, window) }
	for _, key := range []string{"agent:a", "agent:b", "agent:c"} {
		if _, err := c.Add(ctx, key, 1, window); err != nil {
			t.Fatal(err)
		}
	}

	c.now = func() time.Time { return atBucket(103, 0.5, window) }
	if _, err := c.Add(ctx, "agent:d", 1, window); err != nil {
		t.Fatal(err)
	}

	var rows int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM cap_window`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("quedaron %d filas, quería 1: las ventanas vencidas no se liberaron", rows)
	}
}

func TestMemoryCounter_DropsKeysOlderThanTheWindow(t *testing.T) {
	const window = time.Hour
	c := NewMemoryCounter()
	ctx := context.Background()

	c.now = func() time.Time { return atBucket(100, 0.5, window) }
	for _, key := range []string{"agent:a", "agent:b", "agent:c"} {
		if _, err := c.Add(ctx, key, 1, window); err != nil {
			t.Fatal(err)
		}
	}

	c.now = func() time.Time { return atBucket(103, 0.5, window) }
	if _, err := c.Add(ctx, "agent:d", 1, window); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.windows) != 1 {
		t.Errorf("quedaron %d ventanas, quería 1: las vencidas no se liberaron", len(c.windows))
	}
}

// The default counter has to work through the Limiter exactly like the others.
func TestLimiter_EnforcesTheCapOverAMemoryCounter(t *testing.T) {
	l := New(NewMemoryCounter(), time.Hour, 1000)
	ctx := context.Background()

	d, err := l.Allow(ctx, "fleet:default", 600)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed || d.Remaining != 400 {
		t.Errorf("bajo el tope: allowed=%v remaining=%d, quería true/400", d.Allowed, d.Remaining)
	}

	d, err = l.Allow(ctx, "fleet:default", 600)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Error("sobre el tope: allowed=true, quería que bloqueara")
	}
	if d.Remaining != 0 {
		t.Errorf("remaining = %d, quería 0 (nunca negativo)", d.Remaining)
	}
}
