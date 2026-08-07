package sessionreset

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// stubLoader counts how many scans were asked for and lets a test hold one open,
// which is how the async path is observed without sleeping on it.
type stubLoader struct {
	mu       sync.Mutex
	calls    int
	snapshot aggregate.Snapshot
	err      error
	release  chan struct{}
}

func (s *stubLoader) load() (aggregate.Snapshot, error) {
	s.mu.Lock()
	s.calls++
	release := s.release
	s.mu.Unlock()
	if release != nil {
		<-release
	}
	return s.snapshot, s.err
}

func (s *stubLoader) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// fixedClock is a hand-wound clock: the test states the instant, so nothing here
// depends on how long the machine took.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) time() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// TestCurrent_AnswersBeforeTheScanFinishes is the property the rejection path
// depends on: the scan takes ~11.5 s and the guard watching this proxy gives the
// whole 429 eight seconds, so the first lookup must return immediately with what
// it has (nothing, honestly labeled) rather than wait.
func TestCurrent_AnswersBeforeTheScanFinishes(t *testing.T) {
	start := time.Date(2026, 8, 6, 17, 38, 0, 0, time.UTC)
	clock := &fixedClock{now: start.Add(time.Hour)}
	loader := &stubLoader{snapshot: turnsAt(start), release: make(chan struct{})}
	r := New(loader.load, WithClock(clock.time))

	state := r.Current()

	if state.Status != StatusUnknown {
		t.Fatalf("status = %s, want %s mientras el escaneo sigue corriendo", state.Status, StatusUnknown)
	}
	close(loader.release)
	r.Wait()
	if got := r.Current().Status; got != StatusLive {
		t.Errorf("status tras el escaneo = %s, want %q", got, StatusLive)
	}
}

// TestCurrent_OneScanAtATime: a fleet being rejected hits this on every request,
// and each scan costs ~110 MB on a shared machine.
func TestCurrent_OneScanAtATime(t *testing.T) {
	clock := &fixedClock{now: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	loader := &stubLoader{release: make(chan struct{})}
	r := New(loader.load, WithClock(clock.time))

	for i := 0; i < 5; i++ {
		r.Current()
	}

	close(loader.release)
	r.Wait()
	if got := loader.count(); got != 1 {
		t.Errorf("escaneos = %d, want 1: cinco rechazos no pueden disparar cinco lecturas", got)
	}
}

// TestCurrent_ServesALiveWindowWithoutRescanning: the reset of an open window
// does not move, so re-reading it would spend 11.5 s to learn the same instant.
func TestCurrent_ServesALiveWindowWithoutRescanning(t *testing.T) {
	start := time.Date(2026, 8, 6, 17, 38, 0, 0, time.UTC)
	clock := &fixedClock{now: start.Add(time.Hour)}
	loader := &stubLoader{snapshot: turnsAt(start)}
	r := New(loader.load, WithClock(clock.time))
	r.Refresh()

	clock.set(start.Add(4 * time.Hour))
	state := r.Current()
	r.Wait()

	if state.Status != StatusLive {
		t.Fatalf("status = %s, want %q", state.Status, StatusLive)
	}
	if got := loader.count(); got != 1 {
		t.Errorf("escaneos = %d, want 1: la ventana viva ya estaba leída", got)
	}
}

// TestCurrent_RereadsOnceTheWindowRefilled closes the other half: the cached
// answer stops being exact the moment the window it described expires.
func TestCurrent_RereadsOnceTheWindowRefilled(t *testing.T) {
	start := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: start.Add(time.Hour)}
	loader := &stubLoader{snapshot: turnsAt(start)}
	r := New(loader.load, WithClock(clock.time))
	r.Refresh()

	clock.set(start.Add(6 * time.Hour))
	r.Current()
	r.Wait()

	if got := loader.count(); got != 2 {
		t.Errorf("escaneos = %d, want 2: la ventana vencida hay que releerla", got)
	}
}

// TestRefresh_CarriesTheScanErrorInsteadOfDroppingIt: a failed scan must not be
// reported as "no window open", which reads as good news.
func TestRefresh_CarriesTheScanErrorInsteadOfDroppingIt(t *testing.T) {
	clock := &fixedClock{now: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	loader := &stubLoader{err: errors.New("no pude leer las transcripciones")}
	r := New(loader.load, WithClock(clock.time))

	state := r.Refresh()

	if state.Status != StatusUnknown {
		t.Fatalf("status = %s, want %q", state.Status, StatusUnknown)
	}
	if state.Err == nil {
		t.Fatal("el error del escaneo se perdió")
	}
	if note := r.Note(); !strings.Contains(note, "no pude leer las transcripciones") {
		t.Errorf("nota = %q, want que el 429 muestre por qué no hay ETA", note)
	}
}

func TestNote_ComesFromTheCachedReading(t *testing.T) {
	start := time.Date(2026, 8, 6, 17, 38, 0, 0, time.UTC)
	clock := &fixedClock{now: start.Add(3*time.Hour + 26*time.Minute)}
	loader := &stubLoader{snapshot: turnsAt(start)}
	r := New(loader.load, WithClock(clock.time))
	r.Refresh()

	if note := r.Note(); !strings.Contains(note, "1 h 34 min") {
		t.Errorf("nota = %q, want el tiempo real que falta", note)
	}
}
