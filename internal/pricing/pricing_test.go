package pricing

import (
	"math"
	"testing"
)

func TestEstimateUSD_KnownModel(t *testing.T) {
	// 1M input @ $5/MTok + 1M output @ $25/MTok = $30 exactly.
	cost, known := EstimateUSD("claude-opus-4-8", 1_000_000, 1_000_000, 0, 0)
	if !known {
		t.Fatal("known = false, want true for claude-opus-4-8")
	}
	if got, want := cost, 30.0; got != want {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestEstimateUSD_StripsDatedSnapshotSuffix(t *testing.T) {
	cost, known := EstimateUSD("claude-haiku-4-5-20251001", 1_000_000, 0, 0, 0)
	if !known {
		t.Fatal("known = false, want true — the dated suffix should fall back to the base model rate")
	}
	if got, want := cost, 1.0; got != want {
		t.Errorf("cost = %v, want %v ($1/MTok for claude-haiku-4-5)", got, want)
	}
}

func TestEstimateUSD_UnknownModelReturnsNotKnown(t *testing.T) {
	cost, known := EstimateUSD("mistral-large-3", 1000, 500, 0, 0)
	if known {
		t.Fatal("known = true, want false for an unmapped model")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 when the model is unknown", cost)
	}
}

func TestEstimateUSD_CacheWriteBilledAtFiveMinuteRate(t *testing.T) {
	// 1M cache-write(5m) @ $1.25/MTok + 1M cache-read @ $0.10/MTok = $1.35.
	cost, known := EstimateUSD("claude-haiku-4-5", 0, 0, 1_000_000, 1_000_000)
	if !known {
		t.Fatal("known = false, want true")
	}
	if got, want := cost, 1.35; got != want {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestEstimateUSD_LocalModelIsFreeButKnown(t *testing.T) {
	// Un modelo local no es "precio desconocido": su precio es CERO y se sabe.
	// Si devolviera known=false, el reporte lo contaría como hueco de medición y
	// diría que el total está subestimado, que es justo lo contrario de la verdad.
	cost, known := EstimateUSD("nemotron-3-super", 1_000_000, 500_000, 0, 2_000_000)
	if !known {
		t.Error("known = false; un modelo local corre gratis, su precio se conoce y es 0")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0", cost)
	}
}

func TestEstimateUSD_UnknownModelStaysUnknown(t *testing.T) {
	// La contraparte del test de arriba: un modelo que de verdad no conocemos
	// tiene que seguir devolviendo known=false, no colarse como gratis.
	if _, known := EstimateUSD("modelo-que-no-existe", 1000, 100, 0, 0); known {
		t.Error("known = true para un modelo desconocido: eso esconde un hueco de medición")
	}
}

func TestEstimateUSD_PricesTheGeminiFallbackTheFleetActuallyRan(t *testing.T) {
	// A6: OpenClaw's google-gemini-cli fallback turns were the whole of advise E-05.
	// 1M input @ $2 + 1M output @ $12 + 1M cache-read @ $0.20 = $14.20.
	cost, known := EstimateUSD("gemini-3.1-pro-preview", 1_000_000, 1_000_000, 0, 1_000_000)
	if !known {
		t.Fatal("known = false; los turnos de gemini-3.1-pro-preview tienen precio de lista publicado")
	}
	if got, want := cost, 14.20; math.Abs(got-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestEstimateUSD_GeminiCacheWriteCostsOneOrdinaryInputPass(t *testing.T) {
	// Google no cobra recargo por escribir caché: cebar el prefijo cuesta un pase
	// de input normal ($2/MTok), no un múltiplo de él como en Anthropic. Si esto
	// se copiara del patrón 1.25× de Anthropic, el break-even de contextcurve
	// diría que reiniciar una conversación de Gemini es más caro de lo que es.
	cost, known := EstimateUSD("gemini-3.1-pro-preview", 0, 0, 1_000_000, 0)
	if !known {
		t.Fatal("known = false, want true")
	}
	if got, want := cost, 2.00; math.Abs(got-want) > 1e-9 {
		t.Errorf("cost = %v, want %v (la misma tarifa que input)", got, want)
	}
}

func TestContextRates_ReturnsTheReadAndWriteRatesThatDecideTheBreakEven(t *testing.T) {
	read, write, known := ContextRates("claude-opus-5")

	if !known {
		t.Fatal("known = false for a model in the table")
	}
	if read != 0.50/1_000_000 {
		t.Errorf("cache read rate = %v, want %v", read, 0.50/1_000_000)
	}
	if write != 6.25/1_000_000 {
		t.Errorf("cache write rate = %v, want %v (the 5-minute rate, same as EstimateByBucket)", write, 6.25/1_000_000)
	}
}

func TestContextRates_ReportsUnknownRatherThanBorrowingAnotherModelsPrice(t *testing.T) {
	read, write, known := ContextRates("some-model-nobody-priced")

	if known {
		t.Error("known = true for a model absent from the table")
	}
	if read != 0 || write != 0 {
		t.Errorf("rates = (%v, %v), want zeros when the price is unknown", read, write)
	}
}

func TestContextRates_TreatsALocalModelAsCostingNothingToCarry(t *testing.T) {
	read, write, known := ContextRates("nemotron-3-super")

	if !known {
		t.Error("known = false for a local model; its price is zero, not missing")
	}
	if read != 0 || write != 0 {
		t.Errorf("rates = (%v, %v), want zeros: a local model has no per-token charge", read, write)
	}
}
