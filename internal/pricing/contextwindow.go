package pricing

// Context-window sizes live next to the prices because they are the same kind of
// fact about a model — a datum the provider decides and changes, that every
// surface has to agree on — and because the two travel together: the same turn
// that costs money also occupies a window, and a report that priced a turn with
// one model's table and measured its occupancy with another's would contradict
// itself (docs/ventana-contexto.md).
//
// The window is what T22 is about: a session at 60% of Sonnet 5 jumped to 298%
// the moment it switched to Opus 5, because the ceiling moved and the tokens
// didn't. So the ceiling has to be a per-model datum, never a global constant.
//
// SOURCES. Both are local, machine-readable and were read on 2026-07-30; no
// figure below was typed from memory:
//
//   - CATÁLOGO OPENCLAW: node_modules/openclaw/dist/extensions/anthropic/
//     openclaw.plugin.json — the model catalog shipped by the runtime that
//     actually drives OpenClaw, with a contextWindow per model id.
//   - CHANGELOG DE CLAUDE CODE: ~/.claude/cache/changelog.md — Claude Code's own
//     release notes, which state a model's window when it ships.
//
// Every figure was cross-checked against the largest context this machine's
// transcripts ever recorded for that model, and none of the observed peaks
// exceeds the limit the table gives it (the peaks are in
// docs/ventana-contexto.md). Where the two sources DISAGREE, the model gets no
// number at all: it goes to unknownContextWindows with the disagreement as its
// reason. Same rule as the per-model quota weight of T81 — `no derivable` with
// the motive beats a plausible-looking invention (docs/calibracion.md).

// Provenance labels for a limit, so a reader can weigh a ceiling by where it
// came from — the same role SourcePlan/SourceCalibrated play for quota capacity.
const (
	sourceOpenClawCatalog = "catálogo de modelos de OpenClaw (local)"
	sourceClaudeChangelog = "changelog de Claude Code (local)"
	sourceBothSources     = "catálogo de OpenClaw + changelog de Claude Code (coinciden)"
)

// ContextWindow is how many tokens of context one model can hold. Known is false
// when the local sources don't pin it down — either because they contradict each
// other or because the model isn't in any of them — and then Reason says which of
// the two it was. A caller must render the reason where the percentage would go;
// there is no fallback number to borrow.
type ContextWindow struct {
	Tokens int    `json:"tokens"`
	Source string `json:"source,omitempty"`
	Known  bool   `json:"known"`
	Reason string `json:"reason,omitempty"`
}

type contextWindowEntry struct {
	tokens int
	source string
}

var contextWindowTable = map[string]contextWindowEntry{
	// Catalog says 1048576 under both the claude-cli and anthropic providers; the
	// changelog says nothing about this model's window, so there is nothing to
	// contradict it. Observed peak on this machine: 998,050.
	"claude-opus-4-8": {1_048_576, sourceOpenClawCatalog},
	// Not in the catalog. Changelog: "Added Claude Opus 5 (`claude-opus-5`), now
	// the default Opus model — 1M context". Observed peak: 633,948 — which is why
	// the 200K ceiling that Claude Code's own indicator used during the incident
	// (a bug it later fixed for Opus 4.7) is NOT what this table records.
	"claude-opus-5": {1_000_000, sourceClaudeChangelog},
	// Catalog: 1000000. Changelog: "a native 1M-token context window".
	"claude-sonnet-5": {1_000_000, sourceBothSources},
	// Catalog: 1000000. Changelog: "Fable 5 includes 1M context by default".
	"claude-fable-5": {1_000_000, sourceBothSources},
	// Catalog: 200000, for both the bare id and the dated snapshot.
	"claude-haiku-4-5": {200_000, sourceOpenClawCatalog},
}

// unknownContextWindows are the models whose window the local sources cannot
// settle, each with the reason a percentage is missing. They are listed
// explicitly rather than left to fall through to the generic reason: "the two
// sources disagree" is a different, more useful thing to tell a reader than "we
// never heard of this model".
var unknownContextWindows = map[string]string{
	"claude-sonnet-4-6": "las dos fuentes locales se contradicen: el catálogo de OpenClaw dice 200,000 y el " +
		"changelog de Claude Code dice que Sonnet 4.6 «now has 1M context». Con 5x de diferencia, elegir una " +
		"sería inventar el número que decide el porcentaje",
	"claude-sonnet-4-5": "no está en el catálogo local, y el changelog dice que su variante de 1M «is being " +
		"removed from the Max plan»: la ventana depende del plan, así que no hay una sola cifra derivable",
	"nemotron-3-super": "modelo local (Ollama): la ventana la fija el runtime al cargar el modelo y hoy no " +
		"está declarada en la config local de la flota",
}

// Reasons for a window that no table entry covers.
const (
	reasonNoModelReported = "el turno no reporta modelo, así que no hay ventana contra la que medir"
	reasonUnlistedModel   = "el modelo no aparece con ventana de contexto en ninguna fuente local " +
		"(catálogo de OpenClaw, changelog de Claude Code)"
)

// ContextWindowOf returns a model's context window, tolerating a trailing dated
// snapshot suffix the same way prices do. It never guesses: an unknown model
// comes back with Known false and the reason to print in the percentage's place.
func ContextWindowOf(model string) ContextWindow {
	if model == "" {
		return ContextWindow{Reason: reasonNoModelReported}
	}
	for _, name := range []string{model, dateSuffix.ReplaceAllString(model, "")} {
		if entry, ok := contextWindowTable[name]; ok {
			return ContextWindow{Tokens: entry.tokens, Source: entry.source, Known: true}
		}
		if reason, ok := unknownContextWindows[name]; ok {
			return ContextWindow{Reason: reason}
		}
	}
	return ContextWindow{Reason: reasonUnlistedModel}
}
