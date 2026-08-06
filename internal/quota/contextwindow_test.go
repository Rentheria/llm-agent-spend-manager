package quota

import (
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/contextfill"
)

// contextTurn is one measured turn with a given live context, which is what the
// window measurement reads (input + cache-read + cache-write).
func contextTurn(session, thread, model string, context int, at time.Time) aggregate.Record {
	return aggregate.Record{
		Agent:      aggregate.AgentClaudeCode,
		Confidence: aggregate.ConfidenceMeasured,
		SessionID:  session,
		ThreadID:   thread,
		Workspace:  ccWorkspace,
		Model:      model,
		Timestamp:  at,
		CacheRead:  context,
	}
}

func TestContextWindows_RanksTheFullestStreamFirst(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		contextTurn("light", "", "claude-sonnet-5", 100_000, now.Add(-2*time.Hour)),
		contextTurn("heavy", "", "claude-sonnet-5", 900_000, now.Add(-time.Hour)),
	}

	cw := contextWindowsOf(records, now.Add(-24*time.Hour), contextfill.DefaultWarnShare)

	if len(cw.Streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(cw.Streams))
	}
	if got := cw.Streams[0].Stream.SessionID; got != "heavy" {
		t.Errorf("primera fila = %q, want \"heavy\": la respuesta va arriba, no se busca", got)
	}
	if got, want := cw.Streams[0].Stream.Status, contextfill.StatusWarning; got != want {
		t.Errorf("status = %q, want %q al 90%%", got, want)
	}
}

func TestContextWindows_SplitsThreadsOfTheSameSession(t *testing.T) {
	// Una sesión con subagentes corre varios contextos independientes: medirlos
	// juntos daría una ocupación que no es la de ninguno.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		contextTurn("s", "", "claude-sonnet-5", 500_000, now.Add(-3*time.Hour)),
		contextTurn("s", "sub-1", "claude-sonnet-5", 200_000, now.Add(-2*time.Hour)),
	}

	cw := contextWindowsOf(records, now.Add(-24*time.Hour), contextfill.DefaultWarnShare)

	if len(cw.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 (hilo principal + subagente)", len(cw.Streams))
	}
	if got, want := cw.Streams[0].Stream.Live.Tokens, 500_000; got != want {
		t.Errorf("el hilo más lleno carga %d tokens, want %d (no la suma de los dos)", got, want)
	}
}

func TestContextWindows_SkipsActivityTierRecords(t *testing.T) {
	// Cursor/Antigravity no traen tokens por turno: su "contexto" sería un 0 con
	// cara de medición.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{{
		Agent:      aggregate.AgentCursor,
		Confidence: aggregate.ConfidenceActivity,
		SessionID:  "conv-1",
		Model:      "claude-sonnet-5",
		Timestamp:  now.Add(-time.Hour),
		TokensLow:  100_000,
		TokensHigh: 900_000,
	}}

	cw := contextWindowsOf(records, now.Add(-24*time.Hour), contextfill.DefaultWarnShare)

	if len(cw.Streams) != 0 {
		t.Errorf("streams = %d, want 0 para tier de actividad", len(cw.Streams))
	}
	if cw.Blind[aggregate.AgentCursor] == "" {
		t.Error("Cursor no aparece listado como sin contexto medible: se leería como ventana vacía")
	}
}

func TestContextWindows_ZeroContextTurnNeitherResetsNorSwitches(t *testing.T) {
	// Claude Code escribe algún turno sin contexto (un <synthetic>, una negativa).
	// Contarlo fabricaría un cambio de modelo de ida y vuelta alrededor de él.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		contextTurn("s", "", "claude-sonnet-5", 400_000, now.Add(-3*time.Hour)),
		contextTurn("s", "", "<synthetic>", 0, now.Add(-2*time.Hour)),
		contextTurn("s", "", "claude-sonnet-5", 450_000, now.Add(-time.Hour)),
	}

	cw := contextWindowsOf(records, now.Add(-24*time.Hour), contextfill.DefaultWarnShare)

	if len(cw.Streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(cw.Streams))
	}
	if got := cw.Streams[0].Stream.Turns; got != 2 {
		t.Errorf("turnos = %d, want 2: el turno sin contexto no ocupa ventana", got)
	}
	if len(cw.Shifts) != 0 || cw.SameCeilingShifts != 0 {
		t.Errorf("shifts = %d (+%d mismo techo), want 0: nadie cambió de modelo",
			len(cw.Shifts), cw.SameCeilingShifts)
	}
}

func TestContextWindows_ReportsTheModelSwitchThatMovedTheCeiling(t *testing.T) {
	// El caso de T22 extremo a extremo por la capa que alimenta las tres
	// superficies: mismo contexto, dos ventanas, dos porcentajes.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		contextTurn("s", "", "claude-sonnet-5", 600_000, now.Add(-2*time.Hour)),
		contextTurn("s", "", "claude-haiku-4-5", 600_000, now.Add(-time.Hour)),
	}

	cw := contextWindowsOf(records, now.Add(-24*time.Hour), contextfill.DefaultWarnShare)

	if len(cw.Shifts) != 1 {
		t.Fatalf("shifts = %d, want 1", len(cw.Shifts))
	}
	shift := cw.Shifts[0].Shift
	if got, want := shift.Before.Share, 0.60; got != want {
		t.Errorf("antes = %v, want %v", got, want)
	}
	if got, want := shift.After.Share, 3.0; got != want {
		t.Errorf("después = %v, want %v (el mismo contexto contra una ventana 5x más chica)", got, want)
	}
	if got, want := cw.Streams[0].Stream.Status, contextfill.StatusCeiling; got != want {
		t.Errorf("status = %q, want %q: quedó arriba del techo sin escribir nada", got, want)
	}
}

func TestContextWindows_CountsSameCeilingSwitchesInsteadOfListingThem(t *testing.T) {
	// La mayoría de los modelos de esta flota cargan ~1M: listar esos cambios
	// llenaría la tabla de renglones "16% → 16%" y taparía el que sí importa.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		contextTurn("same", "", "claude-sonnet-5", 200_000, now.Add(-4*time.Hour)),
		contextTurn("same", "", "claude-fable-5", 200_000, now.Add(-3*time.Hour)),
		contextTurn("moved", "", "claude-sonnet-5", 300_000, now.Add(-2*time.Hour)),
		contextTurn("moved", "", "claude-haiku-4-5", 300_000, now.Add(-time.Hour)),
	}

	cw := contextWindowsOf(records, now.Add(-24*time.Hour), contextfill.DefaultWarnShare)

	if cw.SameCeilingShifts != 1 {
		t.Errorf("sameCeilingShifts = %d, want 1", cw.SameCeilingShifts)
	}
	if len(cw.Shifts) != 1 {
		t.Fatalf("shifts listados = %d, want 1", len(cw.Shifts))
	}
	if got := cw.Shifts[0].SessionID; got != "moved" {
		t.Errorf("el cambio listado es de %q, want \"moved\"", got)
	}
}

func TestContextWindows_UnwindowedModelsRollUpWithTheirReason(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		contextTurn("a", "", "claude-sonnet-4-6", 150_000, now.Add(-2*time.Hour)),
		contextTurn("b", "", "claude-sonnet-4-6", 160_000, now.Add(-time.Hour)),
		contextTurn("c", "", "claude-sonnet-5", 100_000, now.Add(-time.Hour)),
	}

	cw := contextWindowsOf(records, now.Add(-24*time.Hour), contextfill.DefaultWarnShare)

	if len(cw.Streams) != 1 {
		t.Errorf("streams con %% = %d, want 1: sin techo no hay porcentaje que ordenar", len(cw.Streams))
	}
	if len(cw.NoWindow) != 1 {
		t.Fatalf("noWindow = %d, want 1 modelo", len(cw.NoWindow))
	}
	if got, want := cw.NoWindow[0].Streams, 2; got != want {
		t.Errorf("hilos sin ventana = %d, want %d", got, want)
	}
	if cw.NoWindow[0].Reason == "" {
		t.Error("motivo vacío: un hilo omitido se leería como un hilo con espacio de sobra")
	}
}

func TestContextWindows_LeavesOutStreamsThatWentQuietBeforeThePeriod(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		contextTurn("old", "", "claude-sonnet-5", 900_000, now.Add(-30*24*time.Hour)),
		contextTurn("live", "", "claude-sonnet-5", 100_000, now.Add(-time.Hour)),
	}

	cw := contextWindowsOf(records, now.Add(-72*time.Hour), contextfill.DefaultWarnShare)

	if len(cw.Streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(cw.Streams))
	}
	if got := cw.Streams[0].Stream.SessionID; got != "live" {
		t.Errorf("stream = %q, want \"live\": la sección es sobre sesiones vivas", got)
	}
}

func TestContextWindows_MeasuresTheLiveTailEvenWhenItStartedBeforeThePeriod(t *testing.T) {
	// Una sesión que arrancó antes del periodo y sigue viva: su ocupación es la
	// del último turno, y su historia de cambios de modelo es completa. Recortar
	// la entrada al periodo recortaría las dos cosas.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{
		contextTurn("s", "", "claude-haiku-4-5", 150_000, now.Add(-10*24*time.Hour)),
		contextTurn("s", "", "claude-sonnet-5", 400_000, now.Add(-time.Hour)),
	}

	cw := contextWindowsOf(records, now.Add(-72*time.Hour), contextfill.DefaultWarnShare)

	if len(cw.Streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(cw.Streams))
	}
	if got, want := cw.Streams[0].Stream.Turns, 2; got != want {
		t.Errorf("turnos = %d, want %d: el hilo se mide completo", got, want)
	}
	if len(cw.Shifts) != 1 {
		t.Errorf("shifts = %d, want 1: el cambio de techo es viejo pero explica la ocupación de hoy", len(cw.Shifts))
	}
}

func TestContextWindows_FallsBackToTheDocumentedThreshold(t *testing.T) {
	// Un Config armado a mano (cero en el umbral) no puede marcar todo como
	// advertencia: se aplica el default documentado y el reporte publica cuál fue.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	records := []aggregate.Record{contextTurn("s", "", "claude-sonnet-5", 100_000, now.Add(-time.Hour))}

	cw := contextWindowsOf(records, now.Add(-24*time.Hour), 0)

	if got, want := cw.WarnAt, contextfill.DefaultWarnShare; got != want {
		t.Errorf("warnAt = %v, want %v", got, want)
	}
	if got, want := cw.Streams[0].Stream.Status, contextfill.StatusOK; got != want {
		t.Errorf("status = %q, want %q al 10%%", got, want)
	}
}

func TestAnalyze_CarriesTheContextWindowSection(t *testing.T) {
	// La sección viaja en el mismo Report que alimenta CLI, /api/quota y el
	// dashboard: las tres superficies leen el mismo objeto o no son consistentes.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	snapshot := aggregate.Snapshot{Records: []aggregate.Record{
		contextTurn("s", "", "claude-sonnet-5", 850_000, now.Add(-time.Hour)),
	}}
	cfg := Config{ClaudeTier: "Max 5x", CursorMonthlyUSD: 200, CursorRenewalDay: 1, ContextWarnShare: 0.80}

	report := Analyze(snapshot, cfg, now.Add(-72*time.Hour), now)

	if len(report.ContextWindows.Streams) != 1 {
		t.Fatalf("streams en el reporte = %d, want 1", len(report.ContextWindows.Streams))
	}
	if got, want := report.ContextWindows.Streams[0].Stream.Status, contextfill.StatusWarning; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	if got, want := report.ContextWindows.WarnAt, 0.80; got != want {
		t.Errorf("warnAt = %v, want %v (el umbral configurado llega al reporte)", got, want)
	}
}
