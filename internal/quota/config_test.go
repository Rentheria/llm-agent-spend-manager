package quota

import (
	"testing"

	"github.com/Rentheria/llm-agent-spend-manager/internal/contextfill"
)

func envFrom(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestLoadConfig_FallsBackToTheDocumentedFleetDefaults(t *testing.T) {
	cfg, err := LoadConfig(envFrom(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ClaudeTier != defaultClaudeTier {
		t.Errorf("tier = %q, want %q", cfg.ClaudeTier, defaultClaudeTier)
	}
	if cfg.CursorMonthlyUSD != defaultCursorMonthlyUSD || cfg.CursorRenewalDay != defaultCursorRenewalDay {
		t.Errorf("cursor = $%v day %d, want $%v day %d",
			cfg.CursorMonthlyUSD, cfg.CursorRenewalDay, defaultCursorMonthlyUSD, defaultCursorRenewalDay)
	}
}

func TestLoadConfig_ReadsTheEnvironment(t *testing.T) {
	cfg, err := LoadConfig(envFrom(map[string]string{
		EnvClaudeTier:       "Max 20x",
		EnvCursorMonthlyUSD: "20",
		EnvCursorRenewalDay: "14",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ClaudeTier != "Max 20x" || cfg.CursorMonthlyUSD != 20 || cfg.CursorRenewalDay != 14 {
		t.Errorf("config = %+v", cfg)
	}
}

// A typo in the plan price would otherwise silently rescale every percentage in
// the report, which is worse than refusing to run.
func TestLoadConfig_RejectsUnparseableValuesInsteadOfFallingBack(t *testing.T) {
	bad := []map[string]string{
		{EnvCursorMonthlyUSD: "doscientos"},
		{EnvCursorMonthlyUSD: "0"},
		{EnvCursorMonthlyUSD: "-20"},
		{EnvCursorRenewalDay: "31"},
		{EnvCursorRenewalDay: "0"},
		{EnvCursorRenewalDay: "el primero"},
	}
	for _, vars := range bad {
		if _, err := LoadConfig(envFrom(vars)); err == nil {
			t.Errorf("accepted %v without an error", vars)
		}
	}
}

func TestLoadConfig_AcceptsTheContextThresholdAsFractionOrPercentage(t *testing.T) {
	// Las dos formas son maneras naturales de escribir el mismo umbral; rechazar
	// una solo produciría un "error de configuración" que en realidad es un typo.
	for _, raw := range []string{"0.65", "65"} {
		cfg, err := LoadConfig(envFrom(map[string]string{EnvContextWarnPct: raw}))
		if err != nil {
			t.Fatalf("LoadConfig(%q): %v", raw, err)
		}
		if got, want := cfg.ContextWarnShare, 0.65; got != want {
			t.Errorf("LoadConfig(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestLoadConfig_DefaultsTheContextThresholdToTheDocumentedShare(t *testing.T) {
	cfg, err := LoadConfig(envFrom(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg.ContextWarnShare, contextfill.DefaultWarnShare; got != want {
		t.Errorf("umbral = %v, want %v", got, want)
	}
}

// Un umbral en el techo (o arriba) ya no avisa antes, que es la única razón por
// la que el umbral existe. Se rechaza en vez de recortarse en silencio.
func TestLoadConfig_RejectsAContextThresholdThatCannotWarnEarly(t *testing.T) {
	for _, raw := range []string{"0", "1", "100", "120", "-0.5", "mitad"} {
		if _, err := LoadConfig(envFrom(map[string]string{EnvContextWarnPct: raw})); err == nil {
			t.Errorf("aceptó %s=%q sin error", EnvContextWarnPct, raw)
		}
	}
}

func TestConfig_PlansCoverBothProviders(t *testing.T) {
	cfg, err := LoadConfig(envFrom(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	providers := map[string]bool{}
	for _, p := range cfg.Plans(Calibration{}) {
		providers[p.Provider()] = true
	}
	if len(providers) != 2 {
		t.Errorf("providers = %v, want Anthropic and Cursor", providers)
	}
}
