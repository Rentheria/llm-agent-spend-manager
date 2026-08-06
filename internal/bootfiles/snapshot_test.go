package bootfiles

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadSnapshot_MissingFileIsAnEmptyHistoryNotAnError(t *testing.T) {
	// La primera corrida de una instalación nueva no tiene historia. Eso es
	// normal: el reporte dice que no hay delta y sigue.
	snapshot, err := LoadSnapshot(filepath.Join(t.TempDir(), "bootfiles.json"))
	if err != nil {
		t.Fatalf("un snapshot inexistente no es un error: %v", err)
	}
	if len(snapshot.Files) != 0 {
		t.Errorf("se esperaba historia vacía, se obtuvieron %d entradas", len(snapshot.Files))
	}
}

func TestLoadSnapshot_CorruptFileIsAnError(t *testing.T) {
	// Empezar de cero en silencio tiraría la historia justo cuando más falta
	// hace, y sin avisar — el mismo modo de falla que este ticket persigue.
	path := filepath.Join(t.TempDir(), "bootfiles.json")
	if err := os.WriteFile(path, []byte("{no es json"), 0o600); err != nil {
		t.Fatalf("preparando el archivo corrupto: %v", err)
	}

	if _, err := LoadSnapshot(path); err == nil {
		t.Errorf("un snapshot corrupto debe fallar explícitamente")
	}
}

func TestSaveSnapshot_RoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "bootfiles.json")
	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	want := Snapshot{Files: map[string]SizePoint{"/tmp/state.json": {Bytes: 35503, At: at}}}

	if err := SaveSnapshot(path, want); err != nil {
		t.Fatalf("SaveSnapshot devolvió error: %v", err)
	}

	got, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot devolvió error: %v", err)
	}
	point, ok := got.Files["/tmp/state.json"]
	if !ok {
		t.Fatalf("el snapshot leído no tiene la ruta guardada")
	}
	if point.Bytes != 35503 || !point.At.Equal(at) {
		t.Errorf("se leyó (%d, %v), se esperaba (35503, %v)", point.Bytes, point.At, at)
	}
}

func TestSnapshotPath_LivesInTheSameStateDirAsTheRest(t *testing.T) {
	// El contador de enforcement ya guarda su estado ahí. Un solo lugar donde
	// mirar "qué persiste esta herramienta" en vez de dos.
	want := "/home/tester/.local/state/llm-agent-spend-manager/bootfiles.json"
	if got := SnapshotPath("/home/tester"); got != want {
		t.Errorf("SnapshotPath = %s, se esperaba %s", got, want)
	}
}
