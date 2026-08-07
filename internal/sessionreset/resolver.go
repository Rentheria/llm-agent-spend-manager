package sessionreset

import (
	"sync"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// Loader reads one snapshot of the machine. It is a port rather than a direct
// call into aggregate so the rejection path depends on "something that can be
// read" and not on the scan itself, and so tests never touch a real home.
type Loader func() (aggregate.Snapshot, error)

// Resolver serves the last known window state and refreshes it off the request.
//
// The refresh is asynchronous and single-flight, and this is not an
// optimization. The scan behind it takes ~11.5 s (see the package comment) while
// the guard that watches this proxy gives the whole 429 eight seconds before it
// declares the proxy down — computing the ETA inside the handler would turn a
// truthful rejection into a false outage alert. Refreshing on a timer instead
// would spend 11.5 s of CPU and a ~110 MB spike forever, on a shared machine,
// to answer a question nobody asked; so the read is triggered by the rejection
// itself and the caller gets the previous answer, which is exact for as long as
// the window it describes is open.
type Resolver struct {
	load Loader
	now  func() time.Time

	mu    sync.Mutex
	state State
	busy  bool
	// wg tracks refreshes started in the background so their end can be waited
	// on; nothing on the request path touches it.
	wg sync.WaitGroup
}

// Option customizes a Resolver.
type Option func(*Resolver)

// WithClock replaces the clock, so a test states the instant it is asking about
// instead of racing the real one.
func WithClock(now func() time.Time) Option { return func(r *Resolver) { r.now = now } }

// New builds a Resolver over a snapshot loader. It reads nothing yet: the first
// reading is triggered by the first lookup.
func New(load Loader, opts ...Option) *Resolver {
	r := &Resolver{load: load, now: time.Now}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Current returns the last reading as it stands now, and schedules a refresh in
// the background when that reading is missing or has expired. It never blocks on
// the scan.
func (r *Resolver) Current() State { return r.currentAt(r.now()) }

// Note is the sentence to append to a rejection. It is the Resolver's whole
// public surface for the proxy: a string, produced without blocking.
func (r *Resolver) Note() string {
	now := r.now()
	return r.currentAt(now).Note(now)
}

func (r *Resolver) currentAt(now time.Time) State {
	r.mu.Lock()
	state := r.state
	if state.needsRead(now) && !r.busy {
		r.busy = true
		r.wg.Add(1)
		go r.refreshInBackground()
	}
	r.mu.Unlock()
	return state.at(now)
}

func (r *Resolver) refreshInBackground() {
	defer r.wg.Done()
	defer r.clearBusy()
	r.Refresh()
}

// Refresh reads a snapshot and replaces the cached state. It blocks for as long
// as the scan takes — a caller on a request path wants Current.
func (r *Resolver) Refresh() State {
	snapshot, err := r.load()
	state := r.read(snapshot, err)
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
	return state
}

// read turns one load into a state. A failed scan produces an explicit unknown
// carrying its error: an ETA derived from a half-read machine would be a
// confident wrong number, which is the failure mode this package exists to fix.
func (r *Resolver) read(snapshot aggregate.Snapshot, err error) State {
	now := r.now()
	if err != nil {
		return State{Status: StatusUnknown, Err: err, ReadAt: now}
	}
	return Read(snapshot, now)
}

func (r *Resolver) clearBusy() {
	r.mu.Lock()
	r.busy = false
	r.mu.Unlock()
}

// Wait blocks until any background refresh in flight has finished. Nothing on
// the request path calls it: it is there so a shutdown — or a test — can observe
// the end of work that was deliberately started off the request.
func (r *Resolver) Wait() { r.wg.Wait() }
