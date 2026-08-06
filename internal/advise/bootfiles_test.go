package advise

import (
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/bootfiles"
)

func bootReport(files ...bootfiles.FileSize) bootfiles.Report {
	return bootfiles.Report{
		CheckedAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		Files:     files,
	}
}

func TestWithBootFiles_OversizedFileRaisesTheFinding(t *testing.T) {
	// El aviso es todo el punto del ticket: si un archivo de arranque cruzó su
	// umbral, tiene que salir por el mismo canal que el resto de los hallazgos.
	boot := bootReport(bootfiles.FileSize{
		Path:           "/home/tester/.openclaw/workspace/state.json",
		Bytes:          80000,
		ThresholdBytes: 67584,
		OverThreshold:  true,
		HasPrevious:    true,
		DeltaBytes:     12000,
		PreviousAt:     time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
	})

	report := WithBootFiles(Report{}, boot)

	finding, ok := findingByID(report, FindingBootFilesOversized)
	if !ok {
		t.Fatalf("un archivo por encima del umbral debe producir el hallazgo %s", FindingBootFilesOversized)
	}
	if !strings.Contains(finding.Evidence, "workspace/state.json") {
		t.Errorf("la evidencia debe nombrar el archivo, quedó: %q", finding.Evidence)
	}
	if !strings.Contains(finding.Evidence, "2026-07-28") {
		t.Errorf("la evidencia debe decir desde cuándo creció, quedó: %q", finding.Evidence)
	}
}

func TestWithBootFiles_FileUnderThresholdRaisesNoFinding(t *testing.T) {
	// Un reporte que siempre lista un problema enseña a ignorarlo.
	boot := bootReport(bootfiles.FileSize{
		Path:           "/home/tester/.openclaw/workspace/state.json",
		Bytes:          35503,
		ThresholdBytes: 67584,
	})

	report := WithBootFiles(Report{}, boot)

	if _, ok := findingByID(report, FindingBootFilesOversized); ok {
		t.Errorf("un archivo dentro de su umbral no debe producir hallazgo")
	}
	if len(report.BootFiles.Files) != 1 {
		t.Errorf("la medición se adjunta al reporte aunque no haya hallazgo")
	}
}

func TestWithBootFiles_UnreadableFileIsNotTreatedAsOversized(t *testing.T) {
	// "No lo pude medir" y "está gordo" son cosas distintas. Confundirlas
	// levantaría una alarma que la medición no respalda.
	boot := bootReport(bootfiles.FileSize{
		Path:           "/home/tester/.openclaw/workspace/state.json",
		ThresholdBytes: 67584,
		Unreadable:     "no derivable: el archivo no existe en esta máquina",
	})

	report := WithBootFiles(Report{}, boot)

	if _, ok := findingByID(report, FindingBootFilesOversized); ok {
		t.Errorf("un archivo no medible no debe producir el hallazgo de tamaño")
	}
}

func TestWithBootFiles_MetricIsAShareOfTheFilesActuallyMeasured(t *testing.T) {
	// Regla dura del paquete: Metric siempre es una proporción 0..1. Y el
	// denominador son los archivos que sí se pudieron medir — contar los que no
	// se pudieron leer haría que el número dijera algo que el dato no sostiene.
	boot := bootReport(
		bootfiles.FileSize{Path: "/w/state.json", Bytes: 80000, ThresholdBytes: 67584, OverThreshold: true},
		bootfiles.FileSize{Path: "/w/SYNC.md", Bytes: 64404, ThresholdBytes: 131072},
		bootfiles.FileSize{Path: "/w/falta.md", ThresholdBytes: 1024, Unreadable: "no derivable: no existe"},
	)

	report := WithBootFiles(Report{}, boot)

	finding, ok := findingByID(report, FindingBootFilesOversized)
	if !ok {
		t.Fatalf("se esperaba el hallazgo %s", FindingBootFilesOversized)
	}
	if finding.MetricName != MetricOversizedBootFileShare {
		t.Errorf("MetricName = %q, se esperaba %q", finding.MetricName, MetricOversizedBootFileShare)
	}
	if finding.Metric != 0.5 {
		t.Errorf("Metric = %v, se esperaba 0.5 (1 de 2 archivos medibles)", finding.Metric)
	}
}

func TestWithBootFiles_NoInventedSavings(t *testing.T) {
	// Atribuirle dólares exigiría saber cuántas sesiones abre cada agente y con
	// qué modelo. No se deriva de estos datos, así que va en cero y la evidencia
	// lo dice — misma regla que docs/calibracion.md.
	boot := bootReport(bootfiles.FileSize{
		Path: "/w/state.json", Bytes: 80000, ThresholdBytes: 67584, OverThreshold: true,
	})

	finding, ok := findingByID(WithBootFiles(Report{}, boot), FindingBootFilesOversized)
	if !ok {
		t.Fatalf("se esperaba el hallazgo %s", FindingBootFilesOversized)
	}
	if finding.SavingsUSD != 0 {
		t.Errorf("SavingsUSD = %v, debe ser 0: el costo no es derivable", finding.SavingsUSD)
	}
	if !strings.Contains(finding.Evidence, "no es derivable") {
		t.Errorf("la evidencia debe decir por qué no hay cifra en dólares, quedó: %q", finding.Evidence)
	}
	if finding.Impact != ImpactMedium {
		t.Errorf("Impact = %q, se esperaba %q: sin costo medido no puede desplazar a los hallazgos que sí lo tienen",
			finding.Impact, ImpactMedium)
	}
}

func TestWithBootFiles_AppendedFindingRespectsTheImpactOrder(t *testing.T) {
	// El hallazgo se agrega después de que Analyze ya ordenó, así que tiene que
	// reordenarse: un tip de impacto medio no puede quedar arriba de uno alto
	// solo por haber llegado al final.
	existing := Report{Findings: []Finding{
		{ID: "E-99-medium", Impact: ImpactMedium},
		{ID: "E-99-high", Impact: ImpactHigh},
	}}
	boot := bootReport(bootfiles.FileSize{
		Path: "/w/state.json", Bytes: 80000, ThresholdBytes: 67584, OverThreshold: true,
	})

	report := WithBootFiles(existing, boot)

	if report.Findings[0].Impact != ImpactHigh {
		t.Errorf("el hallazgo de impacto alto debe seguir primero, quedó %q", report.Findings[0].ID)
	}
	if len(report.Findings) != 3 {
		t.Errorf("se esperaban 3 hallazgos, quedaron %d", len(report.Findings))
	}
}

func TestWithBootFiles_AZeroDeltaIsWordedInsteadOfPrintedAsMinusZero(t *testing.T) {
	// Un archivo que ya estaba pasado de umbral la última vez que cambió de
	// tamaño tiene delta 0. Imprimir "-0 B" leería como una medición de que
	// encogió; el dato real es que lleva así desde esa fecha.
	boot := bootReport(bootfiles.FileSize{
		Path: "/w/state.json", Bytes: 80000, ThresholdBytes: 67584, OverThreshold: true,
		HasPrevious: true, DeltaBytes: 0,
		PreviousAt: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
	})

	finding, ok := bootFilesFinding(boot)

	if !ok {
		t.Fatalf("un archivo por encima del umbral debe levantar el hallazgo")
	}
	if strings.Contains(finding.Evidence, "-0 B") {
		t.Errorf("la evidencia no debe imprimir un delta de -0 B, quedó: %s", finding.Evidence)
	}
	if !strings.Contains(finding.Evidence, "sin cambio desde 2026-07-31") {
		t.Errorf("la evidencia debe decir desde cuándo lleva así, quedó: %s", finding.Evidence)
	}
}
