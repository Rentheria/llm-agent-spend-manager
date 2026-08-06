package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/advise"
	"github.com/Rentheria/llm-agent-spend-manager/internal/bootfiles"
)

func adviceReport(t *testing.T, h http.Handler) advise.Report {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/advice", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var report advise.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return report
}

func TestAdvise_ServesTheBootFileMeasurementWhenWired(t *testing.T) {
	boot := bootfiles.Report{
		CheckedAt: fixedNow(),
		Files: []bootfiles.FileSize{
			{Path: "/w/state.json", Bytes: 80000, ThresholdBytes: 67584, OverThreshold: true},
		},
	}
	h := New(fixtureLoader,
		WithClock(fixedNow),
		WithLocation(time.UTC),
		WithBootFilesLoader(func() (bootfiles.Report, error) { return boot, nil }),
	).Handler()

	report := adviceReport(t, h)

	if len(report.BootFiles.Files) != 1 {
		t.Fatalf("se esperaba 1 archivo de arranque en la respuesta, hay %d", len(report.BootFiles.Files))
	}
	found := false
	for _, f := range report.Findings {
		if f.ID == advise.FindingBootFilesOversized {
			found = true
		}
	}
	if !found {
		t.Errorf("un archivo por encima del umbral debe salir como hallazgo %s en /api/advice",
			advise.FindingBootFilesOversized)
	}
}

func TestAdvise_WithoutABootFilesLoaderTheRestOfTheReportStillAnswers(t *testing.T) {
	// La medición es un extra sobre un reporte que ya está completo sin ella, así
	// que no tenerla cableada no puede tumbar la ruta.
	report := adviceReport(t, newTestServer())

	if len(report.BootFiles.Files) != 0 {
		t.Errorf("sin loader no debe haber archivos medidos, hay %d", len(report.BootFiles.Files))
	}
	if report.Fleet.Turns == 0 {
		t.Errorf("el resto del reporte debe seguir contestando")
	}
}

func TestAdvise_AFailedMeasurementOmitsTheSectionInsteadOfFailingTheRequest(t *testing.T) {
	h := New(fixtureLoader,
		WithClock(fixedNow),
		WithLocation(time.UTC),
		WithBootFilesLoader(func() (bootfiles.Report, error) {
			return bootfiles.Report{}, errors.New("snapshot corrupto")
		}),
	).Handler()

	report := adviceReport(t, h)

	if len(report.BootFiles.Files) != 0 {
		t.Errorf("una medición fallida no debe dejar archivos en el reporte")
	}
	if report.Fleet.Turns == 0 {
		t.Errorf("el análisis de costo debe seguir sirviéndose aunque la medición falle")
	}
}
