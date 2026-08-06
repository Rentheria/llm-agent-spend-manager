package workload

import (
	"strings"
	"testing"
)

// aShape is a measured stream with unremarkable features, so each test only has
// to say what makes ITS case special.
func aShape() Shape {
	return Shape{
		SessionID:        "s-1",
		Agent:            "Claude Code",
		Model:            "claude-opus-5",
		Measured:         true,
		Turns:            60,
		TokensPerTurn:    80_000,
		ContextTokens:    4_800_000,
		CacheReadTokens:  4_400_000,
		CacheWriteTokens: 200_000,
		GrowthPerTurn:    900,
		NoReturnTurn:     30,
		CurveKnown:       true,
		CostUSD:          12.5,
	}
}

func TestClassify_LongConversationWhenItOutlivesItsOwnBreakEven(t *testing.T) {
	shape := aShape() // 60 turns against a measured break-even of 30, context climbing

	got := Classify(shape)

	if got.Class != ClassLongConversation {
		t.Errorf("class = %q, want %q", got.Class, ClassLongConversation)
	}
}

// The break-even is the boundary, and it is per session: a stream that never
// reached its own is NOT the accumulating shape, however many turns it ran.
func TestClassify_NotALongConversationWhenItStayedInsideItsBreakEven(t *testing.T) {
	shape := aShape()
	shape.NoReturnTurn = 120 // its own measurement says it had budget left

	got := Classify(shape)

	if got.Class == ClassLongConversation {
		t.Errorf("class = %q, want anything but %q: the stream never passed its own cut point",
			got.Class, ClassLongConversation)
	}
}

// Cache-read dominance is what makes "cap the context" the right lever. A long
// stream paying for fresh input instead of re-reading a prefix is a different
// problem and must not be handed that lever.
func TestClassify_NotALongConversationWhenCacheReadIsNotWhatItPaysFor(t *testing.T) {
	shape := aShape()
	shape.CacheReadTokens = 100_000 // ~2% of context: it's ingesting, not re-reading

	got := Classify(shape)

	if got.Class == ClassLongConversation {
		t.Errorf("class = %q, want anything but %q", got.Class, ClassLongConversation)
	}
}

func TestClassify_MechanicalBurstWhenShortAndSmall(t *testing.T) {
	shape := aShape()
	shape.Turns = 6
	shape.TokensPerTurn = 20_000
	shape.NoReturnTurn = 4 // already past it, and it still isn't a long conversation

	got := Classify(shape)

	if got.Class != ClassMechanicalBurst {
		t.Errorf("class = %q, want %q", got.Class, ClassMechanicalBurst)
	}
}

func TestClassify_BigContextWhenFewTurnsCarryAHugeContext(t *testing.T) {
	shape := aShape()
	shape.Turns = 3
	shape.TokensPerTurn = 400_000

	got := Classify(shape)

	if got.Class != ClassBigContext {
		t.Errorf("class = %q, want %q", got.Class, ClassBigContext)
	}
}

func TestClassify_OneShotWhenItWroteCacheNobodyRead(t *testing.T) {
	shape := aShape()
	shape.Turns = 1
	shape.TokensPerTurn = 30_000
	shape.CacheReadTokens = 0
	shape.CacheWriteTokens = 25_000

	got := Classify(shape)

	if got.Class != ClassOneShot {
		t.Errorf("class = %q, want %q", got.Class, ClassOneShot)
	}
}

// Rule order is load-bearing here: a one-shot is also short and small, so a
// classifier that checked the burst rule first would point at "route it cheaper"
// when the actual waste is the cache-write surcharge nobody amortized.
func TestClassify_OneShotBeatsMechanicalBurstOnTheSameStream(t *testing.T) {
	shape := aShape()
	shape.Turns = 1
	shape.TokensPerTurn = 30_000 // small enough to satisfy the burst rule too
	shape.CacheReadTokens = 0
	shape.CacheWriteTokens = 25_000

	if !isMechanicalBurst(shape) {
		t.Fatal("test premise broken: this stream should also match the burst signature")
	}
	if got := Classify(shape); got.Class != ClassOneShot {
		t.Errorf("class = %q, want %q to win on specificity", got.Class, ClassOneShot)
	}
}

// Cursor and Antigravity report one record per conversation, not per turn. Every
// feature the rules read is absent, so the honest answer is "no data", not the
// nearest-looking shape.
func TestClassify_UnclassifiedWhenTheRouteOnlyReportsEstimatedActivity(t *testing.T) {
	shape := aShape()
	shape.Measured = false
	shape.Turns = 1

	got := Classify(shape)

	if got.Class != ClassUnclassified {
		t.Fatalf("class = %q, want %q", got.Class, ClassUnclassified)
	}
	if got.Reason != ReasonActivityTier {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonActivityTier)
	}
}

// The rule that matters most: a stream that matches nothing is reported as
// unclassified, never rounded to the closest class.
func TestClassify_UnclassifiedWhenFeaturesLandBetweenTheShapes(t *testing.T) {
	shape := aShape()
	shape.Turns = 12             // few turns...
	shape.TokensPerTurn = 90_000 // ...but neither small nor huge

	got := Classify(shape)

	if got.Class != ClassUnclassified {
		t.Errorf("class = %q, want %q: 90k tokens/turn is neither a small burst nor a big-context load",
			got.Class, ClassUnclassified)
	}
	if got.Reason != ReasonBetweenShapes {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonBetweenShapes)
	}
}

func TestClassify_UnclassifiedWithItsOwnReasonWhenTheCurveCouldNotBeMeasured(t *testing.T) {
	shape := aShape()
	shape.CurveKnown = false

	got := Classify(shape)

	if got.Reason != ReasonNoMeasuredCurve {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonNoMeasuredCurve)
	}
}

// Every class must answer "so what do I do about it": a shape with no lever is a
// label, and a label doesn't save money.
func TestClassify_EveryClassCarriesTheLeverThatAppliesToIt(t *testing.T) {
	for _, class := range append(append([]string{}, classOrder...), ClassUnclassified) {
		if strings.TrimSpace(LeverFor(class)) == "" {
			t.Errorf("class %q has no lever", class)
		}
	}
}

func TestCacheReadShare_IsZeroWhenTheStreamMovedNoContext(t *testing.T) {
	var empty Shape

	if got := empty.CacheReadShare(); got != 0 {
		t.Errorf("share = %v, want 0 instead of a division by zero", got)
	}
}
