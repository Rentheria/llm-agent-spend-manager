package pricing

import "testing"

func TestContextWindowOf_KnownModel(t *testing.T) {
	window := ContextWindowOf("claude-sonnet-5")
	if !window.Known {
		t.Fatal("known = false, want true for claude-sonnet-5")
	}
	if got, want := window.Tokens, 1_000_000; got != want {
		t.Errorf("tokens = %d, want %d", got, want)
	}
	if window.Source == "" {
		t.Error("source is empty; a ceiling has to say where it came from")
	}
}

func TestContextWindowOf_StripsDatedSnapshotSuffix(t *testing.T) {
	window := ContextWindowOf("claude-haiku-4-5-20251001")
	if !window.Known {
		t.Fatal("known = false, want true — the dated suffix should fall back to the base model window")
	}
	if got, want := window.Tokens, 200_000; got != want {
		t.Errorf("tokens = %d, want %d", got, want)
	}
}

func TestContextWindowOf_ModelsDoNotShareOneWindow(t *testing.T) {
	// El ticket entero (T22) vive de esta diferencia: si las ventanas fueran
	// iguales, cambiar de modelo a media sesión no movería el porcentaje.
	small := ContextWindowOf("claude-haiku-4-5")
	big := ContextWindowOf("claude-sonnet-5")

	if small.Tokens >= big.Tokens {
		t.Errorf("haiku-4-5 (%d) >= sonnet-5 (%d); la tabla estaría aplanando ventanas distintas",
			small.Tokens, big.Tokens)
	}
}

func TestContextWindowOf_ContradictorySourcesYieldNoNumber(t *testing.T) {
	// claude-sonnet-4-6: el catálogo local dice 200k y el changelog dice 1M. Un
	// 5x de diferencia decide el porcentaje, así que no se elige ninguno.
	window := ContextWindowOf("claude-sonnet-4-6")

	if window.Known {
		t.Error("known = true; con dos fuentes locales que se contradicen no hay cifra derivable")
	}
	if window.Tokens != 0 {
		t.Errorf("tokens = %d, want 0 cuando no es derivable", window.Tokens)
	}
	if window.Reason == "" {
		t.Error("reason vacío; `no derivable` sin motivo es indistinguible de un olvido")
	}
}

func TestContextWindowOf_UnlistedModelSaysSo(t *testing.T) {
	window := ContextWindowOf("gemini-3.1-pro-preview")

	if window.Known {
		t.Error("known = true, want false para un modelo que ninguna fuente local declara")
	}
	if window.Reason == "" {
		t.Error("reason vacío; el reporte tiene que poder imprimir el motivo donde iría el %")
	}
}

func TestContextWindowOf_NoModelReportedIsItsOwnReason(t *testing.T) {
	// Un turno sin modelo (Antigravity no lo expone) no es "modelo desconocido": es que
	// no hay contra qué medir, y el motivo debe decir eso y no otra cosa.
	blank := ContextWindowOf("")
	unlisted := ContextWindowOf("modelo-que-no-existe")

	if blank.Known {
		t.Error("known = true para un turno sin modelo")
	}
	if blank.Reason == unlisted.Reason {
		t.Error("un turno sin modelo comparte motivo con un modelo desconocido; son dos huecos distintos")
	}
}

// TestContextWindowTable_MatchesObservedPeaks es el cruce que valida la tabla
// contra los datos reales de esta máquina (2026-07-30): el contexto más grande
// que un modelo llegó a cargar es un PISO duro de su ventana, así que una
// ventana declarada por debajo de su propio pico observado estaría mal. No
// prueba que la cifra sea exacta —eso solo lo publica el proveedor— pero atrapa
// el error que importa: haber apuntado una ventana demasiado chica.
func TestContextWindowTable_MatchesObservedPeaks(t *testing.T) {
	observedPeaks := map[string]int{
		"claude-opus-4-8":           998_050,
		"claude-sonnet-5":           933_834,
		"claude-opus-5":             633_948,
		"claude-fable-5":            292_120,
		"claude-haiku-4-5-20251001": 60_373,
	}

	for model, peak := range observedPeaks {
		window := ContextWindowOf(model)
		if !window.Known {
			t.Errorf("%s: sin ventana conocida, pero esta máquina le midió un pico de %d tokens", model, peak)
			continue
		}
		if window.Tokens < peak {
			t.Errorf("%s: ventana %d < pico observado %d; la ventana declarada es imposible",
				model, window.Tokens, peak)
		}
	}
}
