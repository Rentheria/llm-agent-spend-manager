package sessionreset

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// turnsAt builds a snapshot whose Anthropic turns land at the given instants.
func turnsAt(stamps ...time.Time) aggregate.Snapshot {
	records := make([]aggregate.Record, 0, len(stamps))
	for _, t := range stamps {
		records = append(records, aggregate.Record{Agent: aggregate.AgentClaudeCode, Timestamp: t})
	}
	return aggregate.Snapshot{Records: records}
}

// TestRead_AnchorsToTheFirstTurnAndNotToTheClock is the regression T139 is
// about: the enforcement window is anchored to the epoch (Unix millis / 5 h), so
// it would put the reset on a round boundary. The real window opens with the
// account's first turn, at whatever minute that was, and that is the instant a
// 429 has to report.
func TestRead_AnchorsToTheFirstTurnAndNotToTheClock(t *testing.T) {
	start := time.Date(2026, 8, 6, 17, 38, 21, 0, time.UTC)
	now := start.Add(3 * time.Hour)

	state := Read(turnsAt(start, start.Add(time.Hour)), now)

	if state.Status != StatusLive {
		t.Fatalf("status = %s, want %q", state.Status, StatusLive)
	}
	if !state.Reset.Equal(start.Add(5 * time.Hour)) {
		t.Errorf("reset = %s, want %s", state.Reset, start.Add(5*time.Hour))
	}
	epochBoundary := time.UnixMilli((now.UnixMilli()/int64(5*time.Hour/time.Millisecond) + 1) *
		int64(5*time.Hour/time.Millisecond)).UTC()
	if state.Reset.Equal(epochBoundary) {
		t.Errorf("la ventana real quedó igual que el borde de época (%s): "+
			"la prueba ya no distingue el bug que motivó T139", epochBoundary)
	}
}

func TestRead_NoWindowInFlightIsIdleNotUnknown(t *testing.T) {
	start := time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC)

	state := Read(turnsAt(start), start.Add(6*time.Hour))

	if state.Status != StatusIdle {
		t.Fatalf("status = %s, want %q", state.Status, StatusIdle)
	}
}

// TestRead_IgnoresAgentsThatDoNotDrainTheAccount guards the plan boundary: a
// Cursor turn opens no Anthropic window, and counting it would invent a session
// out of usage that never touched the quota.
func TestRead_IgnoresAgentsThatDoNotDrainTheAccount(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	snapshot := aggregate.Snapshot{Records: []aggregate.Record{
		{Agent: aggregate.AgentCursor, Timestamp: now.Add(-time.Hour)},
	}}

	if state := Read(snapshot, now); state.Status != StatusIdle {
		t.Errorf("status = %s, want %q", state.Status, StatusIdle)
	}
}

// TestState_AnExpiredReadingIsUnknownNotIdle: once the window it described has
// refilled, the reading cannot say whether a new one opened — and "idle" would
// claim exactly that.
func TestState_AnExpiredReadingIsUnknownNotIdle(t *testing.T) {
	start := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	state := Read(turnsAt(start), start.Add(time.Hour))

	after := state.at(start.Add(6 * time.Hour))

	if after.Status != StatusUnknown {
		t.Fatalf("status = %s, want %q", after.Status, StatusUnknown)
	}
	if !after.Reset.Equal(state.Reset) {
		t.Errorf("la ventana vencida perdió su reset (%s): el aviso ya no puede nombrarla", after.Reset)
	}
}

func TestNote_LiveWindowLeadsWithTheWait(t *testing.T) {
	start := time.Date(2026, 8, 6, 17, 38, 0, 0, time.UTC)
	now := start.Add(3*time.Hour + 26*time.Minute)
	state := Read(turnsAt(start), now)

	note := state.Note(now)

	if !strings.Contains(note, "1 h 34 min") {
		t.Errorf("nota = %q, want el tiempo restante real", note)
	}
	if want := state.Reset.In(time.Local).Format(clockLayout); !strings.Contains(note, want) {
		t.Errorf("nota = %q, want la hora de reloj %q", note, want)
	}
}

func TestNote_IdleSaysTheCapIsOursNotThePlans(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	state := State{Status: StatusIdle, ReadAt: now}

	if note := state.Note(now); !strings.Contains(note, "tope propio") {
		t.Errorf("nota = %q, want que aclare de quién es el tope", note)
	}
}

// TestNote_AFailedScanSaysSoInstead: the message this ticket replaces sounded
// certain while being wrong. An unreadable machine has to read as unreadable.
func TestNote_AFailedScanSaysSoInstead(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	state := State{Status: StatusUnknown, Err: errors.New("permiso denegado"), ReadAt: now}

	note := state.Note(now)

	if !strings.Contains(note, "permiso denegado") {
		t.Errorf("nota = %q, want que cargue el error de la lectura", note)
	}
	if strings.Contains(note, "se libera en") {
		t.Errorf("nota = %q, no debe prometer un ETA que no tiene", note)
	}
}

func TestNote_NeverReadAdmitsIt(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	if note := (State{}).Note(now); !strings.Contains(note, "en segundo plano") {
		t.Errorf("nota = %q, want que diga que aún se está leyendo", note)
	}
}

func TestNeedsRead_ALiveWindowIsNotWorthRescanning(t *testing.T) {
	start := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	state := Read(turnsAt(start), start.Add(time.Minute))

	if state.needsRead(start.Add(4 * time.Hour)) {
		t.Error("relee una ventana viva: el reset ya no se mueve y el escaneo cuesta 11.5 s")
	}
	if !state.needsRead(state.Reset) {
		t.Error("no relee una ventana ya vencida, que es justo cuando hace falta")
	}
}

func TestNeedsRead_AnIdleReadingAges(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	state := State{Status: StatusIdle, ReadAt: now}

	if state.needsRead(now.Add(idleStaleAfter - time.Second)) {
		t.Error("relee de inmediato una lectura idle todavía fresca")
	}
	if !state.needsRead(now.Add(idleStaleAfter)) {
		t.Error("sirve para siempre una lectura idle: una ventana nueva pasaría inadvertida")
	}
}
