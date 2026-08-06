// Package workload answers the question a per-session total cannot: what SHAPE
// did this load have, and therefore which lever applies to it.
//
// Two sessions that cost the same are not the same problem. One that grew a
// context over four thousand turns gets cheaper by being cut; one that died on
// its first turn after writing a cache nobody read gets cheaper by not asking
// for the cache at all. Advice that doesn't know which of the two it's looking
// at can only say "spend less", which is not a lever.
//
// The classification is a fixed set of explicit rules over features that are
// already measured (turn count, tokens per turn, cache-read share of the
// context, the growth slope and break-even from internal/contextcurve). It is
// deliberately NOT clustering: with features this separated a learned classifier
// would land in the same place and lose the only thing that makes the output
// useful — being able to say why. Explainable beats sophisticated here, because
// the explanation IS the product (docs/workload-classes.md).
//
// A stream that matches no rule is reported as ClassUnclassified with the reason
// it didn't match. It is never pushed into the nearest class: "casi una ráfaga
// mecánica" is an invented fact, and this tool doesn't invent facts
// (docs/calibracion.md).
package workload

// Workload class ids. Stable strings: the CLI, the JSON report and the docs all
// name a shape the same way, the same as finding ids do in internal/advise.
const (
	// ClassLongConversation is the accumulating shape: it keeps going past the
	// turn at which carrying its own context stopped paying for itself.
	ClassLongConversation = "conversacion-larga"
	// ClassMechanicalBurst is short work on a small context — the shape whose
	// lever is where you send it, not how long you let it run.
	ClassMechanicalBurst = "rafaga-mecanica"
	// ClassBigContext is few turns each dragging a very large context: the bill
	// comes from what gets loaded, not from how long the thread lived.
	ClassBigContext = "contexto-grande"
	// ClassOneShot dies on its first turn after paying the cache-write surcharge
	// with nothing ever reading the cache back.
	ClassOneShot = "disparo-unico"
	// ClassUnclassified is a stream that matched no rule. It carries a Reason and
	// is never rounded to the nearest shape.
	ClassUnclassified = "sin-clasificar"
)

// Thresholds — every cut point of the classifier, in one place.
//
// These are POLICY, not measurements: each one is a line drawn through a
// distribution that was measured on this machine over the full history
// (48,026 turns / 596 context streams, 2026-07-28). The measurement each line
// was drawn through is written next to it, so anyone can pull the same
// distribution and argue with the cut instead of guessing where it came from.
const (
	// fewTurnsCeiling is where "pocos turnos" ends. Measured: the break-even turn
	// that internal/contextcurve derives per stream has p25 = 19 and median = 24
	// across this machine's 524 measurable streams. At 20 turns three quarters of
	// the fleet's sessions have not yet reached their own cut point — so for a
	// stream this short "córtala antes" is not the available lever, and the class
	// has to be decided by what it carries, not by how long it ran.
	fewTurnsCeiling = 20

	// smallContextTokensPerTurn is the ceiling for "contexto chico". Measured: the
	// fleet's median turn carries 54,293 tokens (568 measured streams). A short
	// stream under 50k is carrying less than a typical fleet turn — small enough
	// that a cheaper model can hold all of it.
	smallContextTokensPerTurn = 50_000

	// bigContextTokensPerTurn is the floor for "tokens/turno altísimo". Measured:
	// among streams that never reached their break-even, the p95 of tokens per turn
	// is 95,266 and the maximum is 198,816; the fleet median is 54,293. At 150k a
	// single turn carries more than the median PEAK of a 21-to-100-turn
	// conversation (74,690) — the load is the context, not the conversation.
	bigContextTokensPerTurn = 150_000

	// cacheReadDominanceShare is when re-reading the prefix, not ingesting new
	// material, is what a stream's context is made of. Measured: among the 351
	// streams past their own break-even, cache-read is a median 91% of context
	// tokens and 64% at p10 — a simple majority separates the accumulating shape
	// without cutting into it.
	cacheReadDominanceShare = 0.50
)

// Reasons a stream stays unclassified. User-facing wording: the report prints
// the reason where the class would go, so "no lo sé" reads as "no lo sé" and not
// as a class.
const (
	// ReasonActivityTier is Cursor and Antigravity: their adapters expose one
	// record per CONVERSATION (an activity estimate), not one per turn, so turn
	// count, tokens per turn and the cache split — every feature the rules read —
	// simply do not exist for them. See docs/architecture.md §3.3.
	ReasonActivityTier = "actividad estimada: un registro por conversación, sin turnos ni buckets medidos — " +
		"no hay con qué medir la forma de la carga"
	// ReasonNoMeasuredCurve is a stream long enough to be an accumulating
	// conversation whose context curve could not be measured (unpriced model, or a
	// context that never grew), so the evidence for that class is missing.
	ReasonNoMeasuredCurve = "sesión larga sin curva de contexto medible: su modelo no está en la tabla de precios " +
		"o su contexto no creció, así que no hay punto de no-retorno con qué compararla"
	// ReasonBetweenShapes is the honest middle: measured, but its features land
	// between the four shapes.
	ReasonBetweenShapes = "la carga no cae en ninguna de las cuatro formas: ni corta con contexto chico, " +
		"ni corta con contexto pesado, ni larga pasada de su punto de no-retorno"
)

// leverByClass is the lever each shape responds to. A fixed lookup table on
// purpose, exactly like advise.mechanismByFinding: the same class always yields
// the same lever and nothing here decides anything at runtime.
var leverByClass = map[string]string{
	ClassLongConversation: "Tope de contexto / corte por tarea. El costo escala con (tamaño del contexto × turnos): " +
		"cablea el corte donde se lanza la sesión o baja el tope de contexto del runtime que la corre. " +
		"El turno al que cablearlo lo da la medición por sesión (E-07, docs/automejora.md).",
	ClassMechanicalBurst: "Rutear a modelo o agente barato. Es trabajo corto sobre un contexto que cabe entero en un " +
		"modelo chico: la palanca es la decisión de despacho, no el largo de la sesión.",
	ClassBigContext: "No releer: pasar punteros, no archivos. Pocos turnos que arrastran mucho contexto pagan por " +
		"material cargado de nuevo, no por historia acumulada — cortar la sesión no mueve esta cuenta.",
	ClassOneShot: "No pedir caché. Escribir caché cuesta ~1.25x input y solo se paga sola si alguien la lee después; " +
		"un disparo que muere en el turno 1 nunca llega a leerla (es la palanca de E-02).",
	ClassUnclassified: "Ninguna: sin forma medida no hay palanca que aplicar. Lo que falta es el dato, no el consejo.",
}

// LeverFor returns the lever a class responds to.
func LeverFor(class string) string { return leverByClass[class] }

// Shape is one context stream's measured feature vector — the input the rules
// read. It is one CONTEXT STREAM, not one session: a session that spawned
// subagents runs several independent contexts at once and each has its own shape
// (see aggregate.Record.ThreadID).
//
// Everything here is measured or derived from measurements by the caller; this
// package computes no cost of its own.
type Shape struct {
	SessionID string `json:"sessionId"`
	ThreadID  string `json:"threadId,omitempty"`
	Agent     string `json:"agent"`
	Label     string `json:"label,omitempty"`
	// Model is the one that carried most of this stream's context.
	Model string `json:"model,omitempty"`
	// Measured is false for activity-tier routes (Cursor, Antigravity). Every
	// feature below is meaningless for them, which is why they never classify.
	Measured bool `json:"measured"`

	Turns         int `json:"turns"`
	TokensPerTurn int `json:"tokensPerTurn"`
	// ContextTokens is input + cache-read + cache-write over the whole stream, and
	// CacheReadTokens / CacheWriteTokens are its cached halves.
	ContextTokens    int `json:"contextTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`

	// GrowthPerTurn, NoReturnTurn and CurveKnown come straight from
	// internal/contextcurve: how fast the context climbs, the turn at which
	// carrying it stops paying, and whether either could be measured at all.
	GrowthPerTurn float64 `json:"growthPerTurn"`
	NoReturnTurn  int     `json:"noReturnTurn"`
	CurveKnown    bool    `json:"curveKnown"`

	CostUSD float64 `json:"costUsd"`
}

// CacheReadShare is how much of this stream's context came back from cache
// instead of being ingested fresh. Zero when the stream moved no context at all.
func (s Shape) CacheReadShare() float64 {
	if s.ContextTokens <= 0 {
		return 0
	}
	return float64(s.CacheReadTokens) / float64(s.ContextTokens)
}

// Classification is the verdict on one shape: which class it is, why it isn't
// classified when it isn't, and the lever that applies.
type Classification struct {
	Class string `json:"class"`
	// Reason is set only for ClassUnclassified.
	Reason string `json:"reason,omitempty"`
	Lever  string `json:"lever"`
}

// rule pairs a class with the signature that identifies it.
type rule struct {
	class   string
	matches func(Shape) bool
}

// rules are tried in order and the FIRST match wins, most specific first. The
// order matters in exactly one place — a one-shot that wrote cache and never
// read it also happens to be short and small, and calling it a mechanical burst
// would point at the wrong lever (route it cheaper) instead of the right one
// (stop asking for the cache).
var rules = []rule{
	{ClassOneShot, isOneShot},
	{ClassBigContext, isBigContext},
	{ClassMechanicalBurst, isMechanicalBurst},
	{ClassLongConversation, isLongConversation},
}

// Classify decides one stream's shape.
func Classify(s Shape) Classification {
	if !s.Measured {
		return unclassified(ReasonActivityTier)
	}
	for _, r := range rules {
		if r.matches(s) {
			return Classification{Class: r.class, Lever: leverByClass[r.class]}
		}
	}
	return unclassified(reasonFor(s))
}

func unclassified(reason string) Classification {
	return Classification{Class: ClassUnclassified, Reason: reason, Lever: leverByClass[ClassUnclassified]}
}

// isOneShot: the stream died on its first turn after paying the cache-write
// surcharge, and nothing ever read that cache back.
func isOneShot(s Shape) bool {
	return s.Turns == 1 && s.CacheWriteTokens > 0 && s.CacheReadTokens == 0
}

// isBigContext: few turns, each one carrying a very large context.
func isBigContext(s Shape) bool {
	return s.Turns <= fewTurnsCeiling && s.TokensPerTurn >= bigContextTokensPerTurn
}

// isMechanicalBurst: few turns on a context small enough to fit anywhere.
func isMechanicalBurst(s Shape) bool {
	return s.Turns <= fewTurnsCeiling && s.TokensPerTurn < smallContextTokensPerTurn
}

// isLongConversation: it outlived its own measured break-even while its context
// kept climbing and cache-read carried the context. All three conditions are
// required — a long stream whose context does NOT grow has nothing to cut, and a
// long stream that isn't re-reading a prefix is paying for something else.
func isLongConversation(s Shape) bool {
	return s.Turns > fewTurnsCeiling &&
		s.CurveKnown &&
		s.GrowthPerTurn > 0 &&
		s.Turns > s.NoReturnTurn &&
		s.CacheReadShare() > cacheReadDominanceShare
}

// reasonFor names why a measured stream matched nothing, so the gap is legible
// instead of anonymous.
func reasonFor(s Shape) string {
	if s.Turns > fewTurnsCeiling && !s.CurveKnown {
		return ReasonNoMeasuredCurve
	}
	return ReasonBetweenShapes
}
