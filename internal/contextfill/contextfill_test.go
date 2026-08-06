package contextfill

import (
	"testing"
	"time"
)

// stamp builds a chronological timestamp n minutes into a fixed day, so the
// order of the turns in a test is the order they were written.
func stamp(minute int) time.Time {
	return time.Date(2026, 7, 30, 10, minute, 0, 0, time.UTC)
}

func TestAnalyze_LiveOccupancyIsTheLastTurnNotTheSum(t *testing.T) {
	// El contexto es un NIVEL: tres turnos de 300k cada uno no son 900k de
	// ventana ocupada, son 300k. Sumarlos daría 90% de una ventana que va al 30%.
	turns := []Turn{
		{Model: "claude-sonnet-5", Timestamp: stamp(1), Context: 300_000},
		{Model: "claude-sonnet-5", Timestamp: stamp(2), Context: 300_000},
		{Model: "claude-sonnet-5", Timestamp: stamp(3), Context: 300_000},
	}

	stream := Analyze("s-1", "", turns, DefaultWarnShare)

	if got, want := stream.Live.Tokens, 300_000; got != want {
		t.Errorf("live tokens = %d, want %d (el último turno, no la suma)", got, want)
	}
	if got, want := stream.Live.Share, 0.30; got != want {
		t.Errorf("share = %v, want %v", got, want)
	}
	if stream.Status != StatusOK {
		t.Errorf("status = %q, want %q", stream.Status, StatusOK)
	}
}

// TestAnalyze_ModelSwitchRescoresTheSameContext es el caso que originó T22: la
// misma sesión, sin escribir un solo token más, medida contra dos ventanas
// distintas da dos porcentajes distintos. 600k tokens son 60% de una ventana de
// 1M y 300% de una de 200k — el 60%→298% del incidente del 2026-07-26.
func TestAnalyze_ModelSwitchRescoresTheSameContext(t *testing.T) {
	turns := []Turn{
		{Model: "claude-sonnet-5", Timestamp: stamp(1), Context: 590_000},
		{Model: "claude-sonnet-5", Timestamp: stamp(2), Context: 600_000},
		{Model: "claude-haiku-4-5", Timestamp: stamp(3), Context: 600_000},
	}

	stream := Analyze("s-1", "", turns, DefaultWarnShare)

	if len(stream.Shifts) != 1 {
		t.Fatalf("shifts = %d, want 1 cambio de modelo detectado", len(stream.Shifts))
	}
	shift := stream.Shifts[0]
	if !shift.Rescored() {
		t.Fatal("Rescored = false; las dos ventanas se conocen, el salto es medible")
	}
	if got, want := shift.CarriedTokens, 600_000; got != want {
		t.Errorf("carried = %d, want %d", got, want)
	}
	if got, want := shift.Before.Share, 0.60; got != want {
		t.Errorf("share antes = %v, want %v (600k de la ventana de 1M de sonnet-5)", got, want)
	}
	if got, want := shift.After.Share, 3.0; got != want {
		t.Errorf("share después = %v, want %v (los MISMOS 600k contra la ventana de 200k)", got, want)
	}
	if shift.Before.Tokens != shift.After.Tokens {
		t.Errorf("tokens antes (%d) != después (%d); el salto tiene que venir del techo, no de tokens nuevos",
			shift.Before.Tokens, shift.After.Tokens)
	}
}

func TestAnalyze_ModelSwitchPutsTheStreamPastItsCeiling(t *testing.T) {
	// El corolario que hace útil el ticket: el hilo cambió de modelo y quedó
	// arriba del techo sin haber escrito nada. El estado tiene que decirlo.
	turns := []Turn{
		{Model: "claude-sonnet-5", Timestamp: stamp(1), Context: 600_000},
		{Model: "claude-haiku-4-5", Timestamp: stamp(2), Context: 600_000},
	}

	stream := Analyze("s-1", "", turns, DefaultWarnShare)

	if stream.Status != StatusCeiling {
		t.Errorf("status = %q, want %q", stream.Status, StatusCeiling)
	}
	if got, want := stream.Live.Model, "claude-haiku-4-5"; got != want {
		t.Errorf("modelo activo = %q, want %q (el del último turno)", got, want)
	}
}

func TestAnalyze_WarnsBeforeTheCeiling(t *testing.T) {
	turns := []Turn{{Model: "claude-sonnet-5", Timestamp: stamp(1), Context: 850_000}}

	stream := Analyze("s-1", "", turns, DefaultWarnShare)

	if stream.Status != StatusWarning {
		t.Errorf("status = %q, want %q al 85%% con umbral en %.2f", stream.Status, StatusWarning, DefaultWarnShare)
	}
}

func TestAnalyze_WarnThresholdIsConfigurable(t *testing.T) {
	turns := []Turn{{Model: "claude-sonnet-5", Timestamp: stamp(1), Context: 500_000}}

	relaxed := Analyze("s-1", "", turns, 0.90)
	strict := Analyze("s-1", "", turns, 0.40)

	if relaxed.Status != StatusOK {
		t.Errorf("status con umbral 0.90 = %q, want %q", relaxed.Status, StatusOK)
	}
	if strict.Status != StatusWarning {
		t.Errorf("status con umbral 0.40 = %q, want %q", strict.Status, StatusWarning)
	}
}

func TestAnalyze_UnknownWindowIsNotOK(t *testing.T) {
	// Un modelo sin ventana derivable no está "bien": no se sabe dónde está. Un
	// StatusOK ahí sería la mentira más cara del reporte.
	turns := []Turn{{Model: "claude-sonnet-4-6", Timestamp: stamp(1), Context: 150_000}}

	stream := Analyze("s-1", "", turns, DefaultWarnShare)

	if stream.Status != StatusUnknown {
		t.Errorf("status = %q, want %q", stream.Status, StatusUnknown)
	}
	if stream.Live.Share != 0 || stream.Live.Limit != 0 {
		t.Errorf("share/limit = %v/%d, want 0/0 sin ventana conocida", stream.Live.Share, stream.Live.Limit)
	}
	if stream.Live.Reason == "" {
		t.Error("reason vacío; el motivo es lo que va en lugar del porcentaje")
	}
	if stream.Live.Tokens != 150_000 {
		t.Errorf("tokens = %d; el contexto medido sigue siendo real aunque falte el techo", stream.Live.Tokens)
	}
}

func TestAnalyze_ShiftWithOneUnknownWindowIsStillReported(t *testing.T) {
	// El cambio de techo pasó; que no se pueda medir el salto no lo borra.
	turns := []Turn{
		{Model: "claude-sonnet-5", Timestamp: stamp(1), Context: 400_000},
		{Model: "claude-sonnet-4-6", Timestamp: stamp(2), Context: 400_000},
	}

	stream := Analyze("s-1", "", turns, DefaultWarnShare)

	if len(stream.Shifts) != 1 {
		t.Fatalf("shifts = %d, want 1", len(stream.Shifts))
	}
	if stream.Shifts[0].Rescored() {
		t.Error("Rescored = true con una ventana no derivable")
	}
	if stream.Shifts[0].After.Reason == "" {
		t.Error("el lado sin ventana no trae motivo")
	}
}

func TestAnalyze_NoShiftWhenTheModelHoldsSteady(t *testing.T) {
	turns := []Turn{
		{Model: "claude-opus-5", Timestamp: stamp(1), Context: 100_000},
		{Model: "claude-opus-5", Timestamp: stamp(2), Context: 200_000},
		{Model: "claude-opus-5", Timestamp: stamp(3), Context: 300_000},
	}

	stream := Analyze("s-1", "", turns, DefaultWarnShare)

	if len(stream.Shifts) != 0 {
		t.Errorf("shifts = %d, want 0 sin cambio de modelo", len(stream.Shifts))
	}
}

func TestAnalyze_CountsEveryShiftIncludingTheReturn(t *testing.T) {
	// Ir y volver son dos cambios de techo, no uno.
	turns := []Turn{
		{Model: "claude-sonnet-5", Timestamp: stamp(1), Context: 100_000},
		{Model: "claude-opus-5", Timestamp: stamp(2), Context: 120_000},
		{Model: "claude-sonnet-5", Timestamp: stamp(3), Context: 140_000},
	}

	stream := Analyze("s-1", "", turns, DefaultWarnShare)

	if len(stream.Shifts) != 2 {
		t.Fatalf("shifts = %d, want 2", len(stream.Shifts))
	}
	if got, want := stream.Shifts[1].At, stamp(3); !got.Equal(want) {
		t.Errorf("el segundo cambio ocurre en %v, want %v", got, want)
	}
}

func TestAnalyze_EmptyStreamSaysWhyInsteadOfShowingZero(t *testing.T) {
	stream := Analyze("s-1", "", nil, DefaultWarnShare)

	if stream.Status != StatusUnknown {
		t.Errorf("status = %q, want %q", stream.Status, StatusUnknown)
	}
	if stream.Live.Reason == "" {
		t.Error("reason vacío; un hilo sin turnos medibles no es un hilo al 0%")
	}
}

func TestOccupancyOf_CarriesTheLimitSource(t *testing.T) {
	occupancy := OccupancyOf("claude-opus-4-8", 524_288)

	if !occupancy.Known {
		t.Fatal("known = false para un modelo con ventana en tabla")
	}
	if got, want := occupancy.Share, 0.5; got != want {
		t.Errorf("share = %v, want %v", got, want)
	}
	if occupancy.LimitSource == "" {
		t.Error("limitSource vacío; un techo tiene que decir de dónde salió")
	}
}
