package enforce

import (
	"testing"
	"time"
)

// The bug this whole file exists to prevent: a fixed window lets the full cap
// through twice in a moment, once on each side of the boundary. Spending the
// budget at the end of a window must still be visible at the start of the next.
func TestEstimate_ClosesTheFixedWindowBoundaryHole(t *testing.T) {
	const window = time.Hour
	// 90% into a bucket, having spent the whole cap in it.
	spent := advance(windowState{}, 100, 1000)
	atEnd := time.UnixMilli(100*window.Milliseconds() + int64(0.9*float64(window.Milliseconds())))

	if got := estimate(spent, atEnd, window); got != 1000 {
		t.Fatalf("dentro de la ventana = %d, quería 1000", got)
	}

	// One millisecond later we are in the next bucket. A fixed window would read
	// 0 here and hand out the whole budget again.
	justAfter := atEnd.Add(time.Duration(0.1*float64(window)) + time.Millisecond)
	rolled := advance(spent, bucketIndex(justAfter, window), 0)
	got := estimate(rolled, justAfter, window)
	if got == 0 {
		t.Fatal("cruzar la frontera puso el contador en 0: es el agujero de la ventana fija")
	}
	if got < 900 {
		t.Errorf("justo tras la frontera = %d, quería ~1000: casi toda la ventana anterior sigue dentro", got)
	}
}

// The carried weight has to decay smoothly as the old bucket slides out, or the
// cap would release the previous window's spend in one jump.
func TestEstimate_DecaysThePreviousBucketAcrossTheWindow(t *testing.T) {
	const window = time.Hour
	st := windowState{bucket: 100, current: 0, previous: 1000}
	base := 100 * window.Milliseconds()

	for _, tc := range []struct {
		fraction float64
		want     int64
	}{
		{0.0, 1000}, // nothing of the old bucket has slid out yet
		{0.25, 750},
		{0.5, 500},
		{0.75, 250},
		{0.99, 10},
	} {
		now := time.UnixMilli(base + int64(tc.fraction*float64(window.Milliseconds())))
		if got := estimate(st, now, window); got != tc.want {
			t.Errorf("al %.0f%% de la ventana = %d, quería %d", tc.fraction*100, got, tc.want)
		}
	}
}

func TestAdvance_AccumulatesShiftsAndResets(t *testing.T) {
	start := windowState{bucket: 10, current: 300, previous: 900}

	if got := advance(start, 10, 50); got != (windowState{bucket: 10, current: 350, previous: 900}) {
		t.Errorf("misma cubeta = %+v, quería que acumulara en current sin tocar previous", got)
	}
	if got := advance(start, 11, 50); got != (windowState{bucket: 11, current: 50, previous: 300}) {
		t.Errorf("una cubeta adelante = %+v, quería que current pasara a previous", got)
	}
	// Two buckets on, everything we hold is older than the window.
	if got := advance(start, 12, 50); got != (windowState{bucket: 12, current: 50, previous: 0}) {
		t.Errorf("dos cubetas adelante = %+v, quería arrancar limpio", got)
	}
	if got := advance(start, 400, 50); got != (windowState{bucket: 400, current: 50, previous: 0}) {
		t.Errorf("muy adelante = %+v, quería arrancar limpio", got)
	}
}

// Buckets are anchored to absolute time, not to when a key was first seen, so
// separate processes agree on the boundaries without coordinating.
func TestBucketIndex_IsAnchoredToAbsoluteTime(t *testing.T) {
	const window = time.Hour
	a := time.Date(2026, 7, 30, 14, 5, 0, 0, time.UTC)
	b := time.Date(2026, 7, 30, 14, 55, 0, 0, time.UTC)
	c := time.Date(2026, 7, 30, 15, 5, 0, 0, time.UTC)

	if bucketIndex(a, window) != bucketIndex(b, window) {
		t.Error("dos instantes de la misma hora cayeron en cubetas distintas")
	}
	if bucketIndex(c, window)-bucketIndex(a, window) != 1 {
		t.Error("la hora siguiente no quedó en la cubeta siguiente")
	}
}
