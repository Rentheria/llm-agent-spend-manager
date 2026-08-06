package quota

import (
	"fmt"
	"os"
	"strconv"

	"github.com/Rentheria/llm-agent-spend-manager/internal/contextfill"
)

// Environment variables that describe the plans this fleet is actually on.
// These are business values — a plan's price and renewal date change without a
// line of code changing — so they are configuration with a documented default,
// not constants compiled into the binary.
const (
	EnvClaudeTier       = "SPEND_CLAUDE_TIER"
	EnvCursorMonthlyUSD = "SPEND_CURSOR_MONTHLY_USD"
	EnvCursorRenewalDay = "SPEND_CURSOR_RENEWAL_DAY"
	// EnvContextWarnPct is how full a session's context window gets before the
	// report flags it. It is configuration for the same reason the plan price is:
	// how much runway someone wants before the wall is their call, not the
	// binary's (default and reasoning in contextfill.DefaultWarnShare).
	EnvContextWarnPct = "SPEND_CONTEXT_WARN_PCT"
)

// Defaults describing a typical fleet. Override them for your own plans.
const (
	defaultClaudeTier       = "Max 5x"
	defaultCursorMonthlyUSD = 200.0
	defaultCursorRenewalDay = 1
)

// percentScale is where a threshold stops being read as a fraction and starts
// being read as a percentage: "80" and "0.80" are both natural ways to write the
// same threshold, and rejecting one of them would only produce a typo that reads
// like a config error.
const percentScale = 100

// Config is the plan configuration for one run.
type Config struct {
	ClaudeTier       string
	CursorMonthlyUSD float64
	CursorRenewalDay int
	// ContextWarnShare is the fraction of a context window that triggers a
	// warning (0.80 = 80%).
	ContextWarnShare float64
}

// LoadConfig reads the plan configuration from the environment, falling back to
// this fleet's documented defaults. A variable that is set but unparseable is an
// error rather than a silent fallback: a typo in the plan price would otherwise
// quietly change every percentage in the report.
func LoadConfig(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Config{
		ClaudeTier:       defaultClaudeTier,
		CursorMonthlyUSD: defaultCursorMonthlyUSD,
		CursorRenewalDay: defaultCursorRenewalDay,
		ContextWarnShare: contextfill.DefaultWarnShare,
	}
	if tier := getenv(EnvClaudeTier); tier != "" {
		cfg.ClaudeTier = tier
	}
	if raw := getenv(EnvCursorMonthlyUSD); raw != "" {
		usd, err := strconv.ParseFloat(raw, 64)
		if err != nil || usd <= 0 {
			return Config{}, fmt.Errorf("%s=%q: se espera un monto positivo en USD", EnvCursorMonthlyUSD, raw)
		}
		cfg.CursorMonthlyUSD = usd
	}
	if raw := getenv(EnvCursorRenewalDay); raw != "" {
		day, err := strconv.Atoi(raw)
		if err != nil || day < 1 || day > 28 {
			return Config{}, fmt.Errorf("%s=%q: se espera un día del 1 al 28", EnvCursorRenewalDay, raw)
		}
		cfg.CursorRenewalDay = day
	}
	if raw := getenv(EnvContextWarnPct); raw != "" {
		share, err := parseWarnShare(raw)
		if err != nil {
			return Config{}, err
		}
		cfg.ContextWarnShare = share
	}
	return cfg, nil
}

// parseWarnShare reads a context-window warning threshold, accepting it as a
// fraction (0.80) or as a percentage (80). A threshold at or above the ceiling is
// rejected instead of clamped: it would silently turn the warning into a report of
// something that already happened, which is the whole failure T22 is about.
func parseWarnShare(raw string) (float64, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: se espera una fracción (0.80) o un porcentaje (80)", EnvContextWarnPct, raw)
	}
	if value > 1 {
		value /= percentScale
	}
	if value <= 0 || value >= 1 {
		return 0, fmt.Errorf("%s=%q: el umbral tiene que quedar entre 0 y el techo (exclusivos); "+
			"avisar en el 100%% ya no es avisar antes", EnvContextWarnPct, raw)
	}
	return value, nil
}

// Plans builds the fleet's plans from a configuration and a calibration.
func (c Config) Plans(calibration Calibration) []Plan {
	return []Plan{
		AnthropicMax{Tier: c.ClaudeTier, Calibration: calibration},
		CursorPlan{MonthlyUSD: c.CursorMonthlyUSD, RenewalDay: c.CursorRenewalDay},
	}
}
