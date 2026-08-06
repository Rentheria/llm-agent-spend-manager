package advise

import (
	"fmt"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// wastefulDay is one day of the exact shape E-02 fires on: a one-turn session
// that writes cache and never reads it back, so the write surcharge bought
// nothing. Repeating it day after day is what "the advice was already given and
// nothing changed" looks like in the data.
func wastefulDay(day, cacheWrite int) aggregate.Record {
	return measuredTurn(fmt.Sprintf("waste-%d", day), day, [4]int{0, 100, cacheWrite, 0})
}

// healthyDay is a two-turn session that writes cache and then reads it back. It
// carries real cost without being wasteful, which is what lets a test move the
// wasted-cache SHARE without touching the waste itself.
func healthyDay(day, cacheRead int) []aggregate.Record {
	session := fmt.Sprintf("healthy-%d", day)
	return []aggregate.Record{
		measuredTurn(session, day, [4]int{0, 100, 10_000, 0}),
		measuredTurn(session, day, [4]int{0, 100, 0, cacheRead}),
	}
}

func escalationByID(report Report, id string) (Escalation, bool) {
	for _, e := range report.Escalations {
		if e.FindingID == id {
			return e, true
		}
	}
	return Escalation{}, false
}

// firstDay is the first of the nine consecutive active days these fixtures use:
// three windows of recurrenceBlockDays each, the minimum the check needs.
const firstDay = 10

func TestAnalyze_EscalatesAdviceThatKeptFiringWithoutMovingItsMetric(t *testing.T) {
	// Nine active days of the same waste: the tip fires in all three windows and
	// its metric is identical in each one.
	var records []aggregate.Record
	for day := firstDay; day < firstDay+recurrenceBlockDays*recurrenceWindows; day++ {
		records = append(records, wastefulDay(day, 200_000))
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	escalation, ok := escalationByID(report, FindingCacheWasted)
	if !ok {
		t.Fatalf("expected %s to escalate after %d windows; escalations = %+v",
			FindingCacheWasted, recurrenceWindows, report.Escalations)
	}
	if escalation.Windows != recurrenceWindows {
		t.Errorf("windows = %d, want %d", escalation.Windows, recurrenceWindows)
	}
	if len(escalation.History) != recurrenceWindows {
		t.Errorf("history has %d points, want %d — the series IS the evidence", len(escalation.History), recurrenceWindows)
	}
	if escalation.Mechanism == "" {
		t.Error("escalation carries no mechanism; the whole point is naming the hardware that's missing")
	}
}

func TestAnalyze_DoesNotRepeatTheTipItEscalated(t *testing.T) {
	// The perverse case: the waste GROWS every window (10x by the last one), so a
	// naive report would rank this advice higher and higher — hardest exactly when
	// the evidence says advice is the wrong remedy. The share it's measured on is
	// flat, so it escalates instead of being re-emitted.
	var records []aggregate.Record
	for i, cacheWrite := range []int{100_000, 500_000, 2_000_000} {
		for offset := 0; offset < recurrenceBlockDays; offset++ {
			records = append(records, wastefulDay(firstDay+i*recurrenceBlockDays+offset, cacheWrite))
		}
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if naive := replayFindings(records)[FindingCacheWasted]; naive.Impact != ImpactHigh {
		t.Fatalf("fixture is not exercising the perverse case: impact = %q, want %q", naive.Impact, ImpactHigh)
	}
	if _, stillATip := findingByID(report, FindingCacheWasted); stillATip {
		t.Errorf("%s was re-emitted as a tip after escalating; repeating it IS the error", FindingCacheWasted)
	}
	if _, ok := escalationByID(report, FindingCacheWasted); !ok {
		t.Errorf("expected %s among the escalations; got %+v", FindingCacheWasted, report.Escalations)
	}
}

func TestAnalyze_KeepsGivingAdviceWhoseMetricIsImproving(t *testing.T) {
	// Same finding in all three windows, but the share it measures drops sharply:
	// the advice is working, so it stays advice.
	var records []aggregate.Record
	for i, cacheWrite := range []int{200_000, 100_000, 20_000} {
		for offset := 0; offset < recurrenceBlockDays; offset++ {
			day := firstDay + i*recurrenceBlockDays + offset
			records = append(records, wastefulDay(day, cacheWrite))
			records = append(records, healthyDay(day, 4_000_000)...)
		}
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if _, ok := findingByID(report, FindingCacheWasted); !ok {
		t.Errorf("expected %s to still be a tip: its metric is improving (%+v)", FindingCacheWasted, report.Findings)
	}
	if _, escalated := escalationByID(report, FindingCacheWasted); escalated {
		t.Errorf("%s escalated while its metric was dropping; that's advice that IS working", FindingCacheWasted)
	}
}

func TestAnalyze_NeedsFullWindowsBeforeCallingSomethingRecurrent(t *testing.T) {
	// One active day short of three full windows: the pattern may well be there,
	// but the data doesn't support the claim yet, so the report says nothing.
	var records []aggregate.Record
	for day := firstDay; day < firstDay+recurrenceBlockDays*recurrenceWindows-1; day++ {
		records = append(records, wastefulDay(day, 200_000))
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if len(report.Escalations) != 0 {
		t.Errorf("escalated with less than %d full windows of history: %+v", recurrenceWindows, report.Escalations)
	}
	if _, ok := findingByID(report, FindingCacheWasted); !ok {
		t.Errorf("expected %s to still be reported as a tip; got %+v", FindingCacheWasted, report.Findings)
	}
}

func TestAnalyze_DoesNotTreatDifferentAdviceAsTheSameAdviceRepeated(t *testing.T) {
	// E-01 fires in every window, but the dominant bucket changes in the last one
	// — and the recommendation is per-bucket. That's different advice, not advice
	// repeated, so nothing has been "given and ignored".
	var records []aggregate.Record
	for day := firstDay; day < firstDay+recurrenceBlockDays*2; day++ {
		records = append(records, measuredTurn(fmt.Sprintf("write-%d", day), day, [4]int{0, 1_000, 1_000_000, 0}))
	}
	for day := firstDay + recurrenceBlockDays*2; day < firstDay+recurrenceBlockDays*recurrenceWindows; day++ {
		records = append(records, measuredTurn(fmt.Sprintf("out-%d", day), day, [4]int{0, 1_000_000, 1_000, 0}))
	}

	report := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if _, escalated := escalationByID(report, FindingDominantBucket); escalated {
		t.Errorf("%s escalated across windows that carried different recommendations: %+v",
			FindingDominantBucket, report.Escalations)
	}
	if _, ok := findingByID(report, FindingDominantBucket); !ok {
		t.Errorf("expected %s to still be reported as a tip; got %+v", FindingDominantBucket, report.Findings)
	}
}

func TestMechanismFor_EveryFindingNamesItsHardware(t *testing.T) {
	// An escalation whose mechanism is the generic fallback is an escalation that
	// says "something's missing" without saying what. Every rule that can escalate
	// must name its own.
	for _, id := range []string{
		FindingDominantBucket, FindingCacheWasted, FindingSessionChurn,
		FindingCronOverhead, FindingUnpricedModels, FindingModelMix,
	} {
		if mechanismFor(id) == genericMechanism {
			t.Errorf("%s has no mechanism of its own in mechanismByFinding", id)
		}
	}
}

func TestAnalyze_EscalationIsDeterministic(t *testing.T) {
	// The claim that makes this a measurement and not a heuristic: same records
	// in, same verdict out, with nothing remembered between runs.
	var records []aggregate.Record
	for day := firstDay; day < firstDay+recurrenceBlockDays*recurrenceWindows; day++ {
		records = append(records, wastefulDay(day, 200_000))
	}

	first := Analyze(records, string(aggregate.WindowAll), time.UTC)
	second := Analyze(records, string(aggregate.WindowAll), time.UTC)

	if len(first.Escalations) != len(second.Escalations) {
		t.Fatalf("escalation count changed between identical runs: %d then %d", len(first.Escalations), len(second.Escalations))
	}
	for i := range first.Escalations {
		if first.Escalations[i].Evidence != second.Escalations[i].Evidence {
			t.Errorf("escalation %d differs between identical runs:\n%q\n%q",
				i, first.Escalations[i].Evidence, second.Escalations[i].Evidence)
		}
	}
}
