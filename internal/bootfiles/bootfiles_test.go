package bootfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFileOfSize deja en disco un archivo del tamaño exacto pedido. El
// contenido da igual: este paquete mide bytes, no lo que dicen.
func writeFileOfSize(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("escribiendo %s: %v", path, err)
	}
}

func TestCheck_FileOverThresholdIsFlagged(t *testing.T) {
	// El caso que motivó el ticket: el archivo de arranque creció por encima de
	// su umbral y nadie se enteró. Aquí sí tiene que enterarse.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeFileOfSize(t, path, 200)

	report, _ := Check(
		[]WatchedFile{{Path: path, ThresholdBytes: 100}},
		Snapshot{},
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	)

	if len(report.Files) != 1 {
		t.Fatalf("se esperaba 1 archivo medido, se obtuvieron %d", len(report.Files))
	}
	file := report.Files[0]
	if !file.OverThreshold {
		t.Errorf("200 bytes contra un umbral de 100 debe cruzar el umbral, OverThreshold=false")
	}
	if file.Bytes != 200 {
		t.Errorf("Bytes = %d, se esperaba 200", file.Bytes)
	}
	if got := len(report.Oversized()); got != 1 {
		t.Errorf("Oversized() devolvió %d archivos, se esperaba 1", got)
	}
}

func TestCheck_FileUnderThresholdIsNotFlagged(t *testing.T) {
	// La otra mitad del contrato: un archivo sano no debe generar ruido, o el
	// aviso deja de significar algo.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeFileOfSize(t, path, 40)

	report, _ := Check(
		[]WatchedFile{{Path: path, ThresholdBytes: 100}},
		Snapshot{},
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	)

	if report.Files[0].OverThreshold {
		t.Errorf("40 bytes contra un umbral de 100 no debe cruzar el umbral, OverThreshold=true")
	}
	if got := len(report.Oversized()); got != 0 {
		t.Errorf("Oversized() devolvió %d archivos, se esperaba 0", got)
	}
}

func TestCheck_ExactlyAtThresholdIsNotOver(t *testing.T) {
	// El umbral es el último tamaño aceptable, no el primero que avisa. Fijar el
	// borde evita que alguien lo mueva sin querer al refactorizar la comparación.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeFileOfSize(t, path, 100)

	report, _ := Check(
		[]WatchedFile{{Path: path, ThresholdBytes: 100}},
		Snapshot{},
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	)

	if report.Files[0].OverThreshold {
		t.Errorf("un archivo exactamente en el umbral no lo cruza, OverThreshold=true")
	}
}

func TestCheck_FirstRunReportsNoDeltaInsteadOfZero(t *testing.T) {
	// Sin corrida anterior no hay delta. Reportar 0 se leería como "no creció",
	// que es una afirmación que este dato no respalda.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeFileOfSize(t, path, 500)

	report, _ := Check(
		[]WatchedFile{{Path: path, ThresholdBytes: 1000}},
		Snapshot{},
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	)

	if report.Files[0].HasPrevious {
		t.Errorf("en la primera corrida HasPrevious debe ser false")
	}
}

func TestCheck_DeltaIsMeasuredAgainstThePreviousSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeFileOfSize(t, path, 900)

	before := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	previous := Snapshot{Files: map[string]SizePoint{path: {Bytes: 400, At: before}}}

	report, next := Check([]WatchedFile{{Path: path, ThresholdBytes: 1000}}, previous, now)

	file := report.Files[0]
	if !file.HasPrevious {
		t.Fatalf("con snapshot previo HasPrevious debe ser true")
	}
	if file.DeltaBytes != 500 {
		t.Errorf("DeltaBytes = %d, se esperaba 500 (900 ahora vs 400 antes)", file.DeltaBytes)
	}
	if !file.PreviousAt.Equal(before) {
		t.Errorf("PreviousAt = %v, se esperaba %v", file.PreviousAt, before)
	}
	if point := next.Files[path]; point.Bytes != 900 || !point.At.Equal(now) {
		t.Errorf("el snapshot nuevo debe avanzar a (900, %v), quedó (%d, %v)", now, point.Bytes, point.At)
	}
}

func TestCheck_UnchangedSizeKeepsTheOriginalTimestamp(t *testing.T) {
	// El dashboard corre este chequeo en cada poll. Si cada corrida pisara el
	// timestamp, el delta siempre diría "sin cambios desde hace un minuto" y la
	// métrica no serviría para nada. El snapshot solo avanza cuando el tamaño
	// realmente cambió, así que la referencia sigue siendo la última vez que fue
	// distinto.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeFileOfSize(t, path, 400)

	before := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	previous := Snapshot{Files: map[string]SizePoint{path: {Bytes: 400, At: before}}}

	report, next := Check([]WatchedFile{{Path: path, ThresholdBytes: 1000}}, previous, now)

	if report.Files[0].DeltaBytes != 0 {
		t.Errorf("un tamaño idéntico no cambió: DeltaBytes = %d", report.Files[0].DeltaBytes)
	}
	if !next.Files[path].At.Equal(before) {
		t.Errorf("con el tamaño sin cambios el snapshot debe conservar %v, quedó %v", before, next.Files[path].At)
	}
}

func TestCheck_MissingFileIsNotDerivableNotZeroBytes(t *testing.T) {
	// Un archivo que no se pudo leer se reporta como no derivable con su motivo.
	// Reportarlo como 0 bytes sería inventar una medición — y además lo dejaría
	// permanentemente "por debajo del umbral".
	path := filepath.Join(t.TempDir(), "no-existe.json")

	report, next := Check(
		[]WatchedFile{{Path: path, ThresholdBytes: 100}},
		Snapshot{},
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	)

	file := report.Files[0]
	if file.Measured() {
		t.Fatalf("un archivo inexistente no es medible, Unreadable quedó vacío")
	}
	if !strings.Contains(file.Unreadable, "no derivable") {
		t.Errorf("Unreadable = %q, debe decir por qué no es derivable", file.Unreadable)
	}
	if file.OverThreshold {
		t.Errorf("un archivo no medible no puede cruzar el umbral")
	}
	if report.MeasuredCount() != 0 {
		t.Errorf("MeasuredCount() = %d, se esperaba 0", report.MeasuredCount())
	}
	if _, ok := next.Files[path]; ok {
		t.Errorf("un archivo no medible no debe entrar al snapshot")
	}
}

func TestCheck_UnreadableFileKeepsItsPreviousHistory(t *testing.T) {
	// Si hoy el archivo no se puede leer, borrar su historia haría que mañana no
	// haya delta contra nada. La medición se pierde; el historial no.
	path := filepath.Join(t.TempDir(), "no-existe.json")
	before := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	previous := Snapshot{Files: map[string]SizePoint{path: {Bytes: 400, At: before}}}

	_, next := Check(
		[]WatchedFile{{Path: path, ThresholdBytes: 100}},
		previous,
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	)

	point, ok := next.Files[path]
	if !ok {
		t.Fatalf("el snapshot previo debe sobrevivir a una corrida no medible")
	}
	if point.Bytes != 400 || !point.At.Equal(before) {
		t.Errorf("el snapshot quedó (%d, %v), se esperaba (400, %v)", point.Bytes, point.At, before)
	}
}

func TestCheck_DirectoryIsNotDerivable(t *testing.T) {
	dir := t.TempDir()

	report, _ := Check(
		[]WatchedFile{{Path: dir, ThresholdBytes: 100}},
		Snapshot{},
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	)

	if report.Files[0].Measured() {
		t.Errorf("un directorio no es un archivo de arranque medible")
	}
}

func TestLoadWatched_DefaultsWatchTheTwoRealBootFiles(t *testing.T) {
	// La lista arranca corta a propósito: solo los dos archivos que una sesión
	// real lee al arrancar en esta máquina. Agregar rutas requiere su propio
	// hallazgo, no una lista genérica.
	watched, err := LoadWatched(func(string) string { return "" }, "/home/tester")
	if err != nil {
		t.Fatalf("LoadWatched devolvió error con el entorno vacío: %v", err)
	}

	if len(watched) != 2 {
		t.Fatalf("se esperaban 2 archivos vigilados por defecto, se obtuvieron %d", len(watched))
	}
	wantPaths := []string{
		"/home/tester/.openclaw/workspace/state.json",
		"/home/tester/.openclaw/workspace/SYNC.md",
	}
	for i, want := range wantPaths {
		if watched[i].Path != want {
			t.Errorf("archivo %d = %s, se esperaba %s", i, watched[i].Path, want)
		}
		if watched[i].ThresholdBytes <= 0 {
			t.Errorf("%s quedó sin umbral positivo: %d", want, watched[i].ThresholdBytes)
		}
	}
}

func TestLoadWatched_ThresholdsAreTwiceTheDocumentedBaseline(t *testing.T) {
	// Los umbrales no son números redondos elegidos a ojo: son 2x una línea base
	// medida y documentada (docs/archivos-arranque.md). Este test los fija para
	// que cambiarlos sea una decisión consciente, no un descuido.
	watched, err := LoadWatched(func(string) string { return "" }, "/home/tester")
	if err != nil {
		t.Fatalf("LoadWatched devolvió error: %v", err)
	}

	if got, want := watched[0].ThresholdBytes, int64(67584); got != want {
		t.Errorf("umbral de state.json = %d, se esperaba %d (33 KiB x 2)", got, want)
	}
	if got, want := watched[1].ThresholdBytes, int64(131072); got != want {
		t.Errorf("umbral de SYNC.md = %d, se esperaba %d (64 KiB x 2)", got, want)
	}
}

func TestLoadWatched_EnvOverridesTheList(t *testing.T) {
	env := func(key string) string {
		if key == EnvWatchedFiles {
			return "/tmp/a.json=1024, /tmp/b.md=2048"
		}
		return ""
	}

	watched, err := LoadWatched(env, "/home/tester")
	if err != nil {
		t.Fatalf("LoadWatched devolvió error: %v", err)
	}

	want := []WatchedFile{{Path: "/tmp/a.json", ThresholdBytes: 1024}, {Path: "/tmp/b.md", ThresholdBytes: 2048}}
	if len(watched) != len(want) {
		t.Fatalf("se esperaban %d archivos, se obtuvieron %d", len(want), len(watched))
	}
	for i, w := range want {
		if watched[i] != w {
			t.Errorf("archivo %d = %+v, se esperaba %+v", i, watched[i], w)
		}
	}
}

func TestLoadWatched_MalformedEnvFailsInsteadOfFallingBack(t *testing.T) {
	// Un typo en la variable no puede degradar en silencio a los defaults: el
	// operador creería estar vigilando lo que escribió.
	cases := map[string]string{
		"sin umbral":         "/tmp/a.json",
		"umbral no numérico": "/tmp/a.json=mucho",
		"umbral cero":        "/tmp/a.json=0",
		"umbral negativo":    "/tmp/a.json=-5",
		"ruta vacía":         "=1024",
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			env := func(key string) string {
				if key == EnvWatchedFiles {
					return raw
				}
				return ""
			}
			if _, err := LoadWatched(env, "/home/tester"); err == nil {
				t.Errorf("%s=%q debe fallar, no caer a los defaults", EnvWatchedFiles, raw)
			}
		})
	}
}
