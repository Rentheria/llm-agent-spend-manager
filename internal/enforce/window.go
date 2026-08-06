package enforce

import "time"

// windowState is one key's sliding-window tally: two adjacent buckets, each the
// length of the cap window.
//
// Why two and not one: a single bucket that resets on expiry is a FIXED window,
// and a fixed window lets through ~2x the cap at the boundary — spend the whole
// budget just before the reset, then the whole budget again just after, and the
// fleet burns double in a short span while every individual check said "under
// the cap". For a budget whose entire purpose is not blowing through the
// provider's quota, that is the exact failure it was built to prevent.
//
// Keeping the previous bucket lets us weigh it by how much of it still falls
// inside the rolling window (see estimate), which costs one extra integer per
// key and removes the boundary hole.
type windowState struct {
	// bucket is which window `current` counts, as an index of absolute time.
	// Anchoring to the epoch rather than to first-seen keeps every backend and
	// every process agreeing on the same boundaries without coordinating.
	bucket int64
	// current is the amount accumulated in `bucket`.
	current int64
	// previous is the amount accumulated in `bucket - 1`.
	previous int64
}

// bucketIndex returns which window now falls in.
func bucketIndex(now time.Time, window time.Duration) int64 {
	return now.UnixMilli() / window.Milliseconds()
}

// bucketElapsed returns how far into the current bucket now sits, in [0,1).
func bucketElapsed(now time.Time, window time.Duration) float64 {
	ms := window.Milliseconds()
	return float64(now.UnixMilli()%ms) / float64(ms)
}

// advance rolls st forward to bucket and adds amount.
//
// One bucket of distance shifts current into previous; two or more means the key
// went quiet for a whole window and both buckets are stale, so it starts clean.
func advance(st windowState, bucket, amount int64) windowState {
	switch bucket - st.bucket {
	case 0:
		st.current += amount
	case 1:
		st.previous, st.current = st.current, amount
	default:
		st.previous, st.current = 0, amount
	}
	st.bucket = bucket
	return st
}

// estimate is the sliding-window total: everything in the current bucket, plus
// the fraction of the previous bucket that has not yet slid out of the window.
// Thirty percent into the current window, 70% of the previous one still counts.
//
// It assumes the previous bucket's traffic was spread evenly across it, which is
// what makes this an approximation rather than an exact log of every request —
// the trade the sliding-window counter is named for. Measured error is a
// fraction of a percent, against a fixed window's 100% hole at the boundary, and
// it costs two integers per key instead of one timestamp per request.
func estimate(st windowState, now time.Time, window time.Duration) int64 {
	carry := float64(st.previous) * (1 - bucketElapsed(now, window))
	return st.current + int64(carry+0.5)
}
