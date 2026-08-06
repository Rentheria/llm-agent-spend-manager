package advise

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// MetricPoint is one finding's metric measured over one window: which active days
// the window covers and what the number was. An Escalation carries the whole
// series so a reader can see for themselves that the number didn't move, instead
// of taking the verdict on faith — same rule as every other figure here.
type MetricPoint struct {
	Days   []string `json:"days"`
	Metric float64  `json:"metric"`
}

// Escalation is what the report emits INSTEAD of a finding that kept firing,
// window after window, without its own metric improving.
//
// The reasoning is in docs/automejora.md §6: a tip that has been on the report
// three windows running and changed nothing is not a tip that needs better
// wording — it is evidence that no tip fixes this, and what's missing is a
// mechanism (a script, a hard cap, a different dispatch). Repeating the tip IS
// the error, so the tip is not repeated: it is replaced by this.
//
// This stays inside the project's "measure, do not learn" rule. It is a
// derivation over data already in hand:
// same records in, same escalations out, nothing remembered between runs (see
// recentWindows), no threshold inferred — they're named constants in advise.go.
type Escalation struct {
	FindingID    string `json:"findingId"`
	FindingTitle string `json:"findingTitle"`
	// Windows is how many consecutive windows the finding fired in, and
	// WindowDays how many active days each window spans.
	Windows    int    `json:"windows"`
	WindowDays int    `json:"windowDays"`
	MetricName string `json:"metricName"`
	// MetricDeltaPct is the change in the finding's metric from the oldest window
	// to the newest. Negative = improving, which is exactly the case that does
	// NOT escalate.
	MetricDeltaPct float64       `json:"metricDeltaPct"`
	History        []MetricPoint `json:"history"`
	Evidence       string        `json:"evidence"`
	// Mechanism is the hardware the situation calls for, looked up from a fixed
	// table by finding id. It is never generated: see mechanismByFinding.
	Mechanism string `json:"mechanism"`
}

// adviceWindow is one comparison window: a block of consecutive active days plus
// the records inside it.
type adviceWindow struct {
	days    []string
	records []aggregate.Record
}

// splitEscalated separates the findings the report should still emit as tips from
// the ones that have earned an architecture-gap alert instead.
//
// "Was this advice given before?" is answered by REPLAYING the same rules over
// the earlier windows of the same records — not by reading a log of past runs.
// That keeps the whole thing a pure function of the data (no stored state, no
// memory of previous executions) and makes the claim stronger rather than weaker:
// it says the condition that produces the advice has been continuously true,
// which doesn't depend on whether anyone happened to run the report those days.
func splitEscalated(current []Finding, records []aggregate.Record, loc *time.Location) ([]Finding, []Escalation) {
	windows := recentWindows(records, loc)
	if len(windows) < recurrenceWindows {
		return current, nil
	}
	history := make([]map[string]Finding, 0, len(windows))
	for _, w := range windows {
		history = append(history, replayFindings(w.records))
	}

	kept := make([]Finding, 0, len(current))
	escalated := make([]Escalation, 0, len(current))
	for _, f := range current {
		esc, isGap := escalationFor(f, windows, history)
		if !isGap {
			kept = append(kept, f)
			continue
		}
		escalated = append(escalated, esc)
	}
	return kept, escalated
}

// escalationFor decides whether one finding has stopped being a tip. Three
// conditions, all readable off the History it returns: the same finding id fired
// in EVERY window, it measured the same thing every time (a finding whose metric
// changed identity is different advice, not repeated advice), and the metric did
// not improve by more than recurrenceImprovementPct from the oldest window to the
// newest.
func escalationFor(f Finding, windows []adviceWindow, history []map[string]Finding) (Escalation, bool) {
	points := make([]MetricPoint, 0, len(windows))
	for i, fired := range history {
		past, wasEmitted := fired[f.ID]
		if !wasEmitted || past.MetricName != f.MetricName {
			return Escalation{}, false
		}
		points = append(points, MetricPoint{Days: windows[i].days, Metric: past.Metric})
	}

	oldest, newest := points[0].Metric, points[len(points)-1].Metric
	if oldest <= 0 {
		// With no baseline there is no "did it move" to answer. Staying quiet
		// beats escalating on a division by zero.
		return Escalation{}, false
	}
	deltaPct := (newest - oldest) / oldest * 100
	if deltaPct < -recurrenceImprovementPct {
		return Escalation{}, false // the advice IS moving the number: keep giving it
	}
	return newEscalation(f, points, deltaPct), true
}

// newEscalation assembles the alert once the recurrence is established.
func newEscalation(f Finding, points []MetricPoint, deltaPct float64) Escalation {
	return Escalation{
		FindingID:      f.ID,
		FindingTitle:   f.Title,
		Windows:        len(points),
		WindowDays:     recurrenceBlockDays,
		MetricName:     f.MetricName,
		MetricDeltaPct: deltaPct,
		History:        points,
		Evidence:       escalationEvidence(f, points, deltaPct),
		Mechanism:      mechanismFor(f.ID),
	}
}

// escalationEvidence renders the series the verdict came from. Every finding's
// metric is a share, so it reads as a percentage across the board.
func escalationEvidence(f Finding, points []MetricPoint, deltaPct float64) string {
	steps := make([]string, 0, len(points))
	for _, p := range points {
		steps = append(steps, fmt.Sprintf("%s…%s: %.1f%%", p.Days[0], p.Days[len(p.Days)-1], p.Metric*100))
	}
	return fmt.Sprintf("El consejo %s se emitió en %d ventanas seguidas de %d días activos y su métrica (%s) "+
		"no mejoró: %s (%+.1f%%). Un consejo ya emitido que reincide no significa que importe más; "+
		"significa que no se está arreglando.",
		f.ID, len(points), recurrenceBlockDays, f.MetricName, strings.Join(steps, " → "), deltaPct)
}

// recentWindows splits the records into the most recent recurrenceWindows blocks
// of recurrenceBlockDays ACTIVE days each, oldest block first. Days with no
// records aren't in the series at all — the same rule the trend uses, so a quiet
// weekend can't stretch a window. It returns nil when there isn't enough history
// for that many full blocks: with less, "it kept firing" is not a claim the data
// supports, and the report says nothing instead of guessing.
func recentWindows(records []aggregate.Record, loc *time.Location) []adviceWindow {
	byDay := recordsByDay(records, loc)
	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)

	needed := recurrenceBlockDays * recurrenceWindows
	if len(days) < needed {
		return nil
	}
	days = days[len(days)-needed:]

	out := make([]adviceWindow, 0, recurrenceWindows)
	for block := 0; block < recurrenceWindows; block++ {
		w := adviceWindow{days: days[block*recurrenceBlockDays : (block+1)*recurrenceBlockDays]}
		for _, day := range w.days {
			w.records = append(w.records, byDay[day]...)
		}
		out = append(out, w)
	}
	return out
}

// recordsByDay buckets records by local calendar day.
func recordsByDay(records []aggregate.Record, loc *time.Location) map[string][]aggregate.Record {
	byDay := map[string][]aggregate.Record{}
	for _, r := range records {
		day := r.Timestamp.In(loc).Format("2006-01-02")
		byDay[day] = append(byDay[day], r)
	}
	return byDay
}

// replayFindings runs the exact same rule set over one window's records and
// indexes what fired by id. Same code path as the live report on purpose: a
// finding must not "count as emitted" under different logic than the one that
// emits it.
func replayFindings(records []aggregate.Record) map[string]Finding {
	sessions := groupSessions(records)
	window := Report{Fleet: efficiencyOf(fleetName, records, sessions)}
	out := map[string]Finding{}
	for _, f := range findings(records, sessions, window) {
		out[f.ID] = f
	}
	return out
}

// mechanismByFinding maps each finding to the kind of hardware that would make
// its failure impossible. By the time a finding escalates, advice has already
// been given and measured not to work, so what's left has to be something that
// doesn't depend on anyone remembering. It's a fixed lookup table on purpose:
// the same id always yields the same mechanism, and nothing here "decides".
var mechanismByFinding = map[string]string{
	FindingDominantBucket: "El tip ya se dio y el bucket sigue cargando el costo. Fierro: un tope duro que no dependa de que " +
		"alguien se acuerde — el cap combinado de internal/enforce cableado al proxy, o un despachador que parta la tarea " +
		"antes de arrancarla. Si el bucket dominante es cache-read, el límite va sobre el tamaño del contexto por sesión, " +
		"no sobre lo que se escribe.",
	FindingCacheWasted: "El tip ya se dio y se sigue pagando caché que nadie lee. Fierro: quitar el caché en el LANZADOR de las " +
		"tareas de un solo disparo (el script que despacha, no el agente), o agruparlas en una sesión que sí la reuse. " +
		"El agente no puede tomar esta decisión turno a turno: la toma quien lo lanza.",
	FindingSessionChurn: "El tip ya se dio y las sesiones siguen muriendo antes de amortizar el prefijo. Fierro: cambiar el " +
		"despacho, no el consejo — un wrapper que agrupe las preguntas cortas del mismo tema en una sola sesión, o recortar " +
		"el prompt de sistema que cada disparo vuelve a pagar completo.",
	FindingCronOverhead: "El tip ya se dio y el trabajo programado sigue pesando igual. Fierro: esto es configuración, no " +
		"criterio — baja el intervalo del cron o fija un modelo más barato para el turno automático en el archivo de config. " +
		"Nadie va a 'acordarse' de correr menos un cron.",
	FindingUnpricedModels: "El tip ya se dio y siguen entrando modelos sin precio. Fierro: un check en CI que falle cuando " +
		"aparece un modelo que no está en internal/pricing. Mientras sea un recordatorio, el hueco vuelve con el siguiente " +
		"modelo que se estrene.",
	FindingModelMix: "El tip ya se dio y el modelo caro sigue haciendo trabajo chico. Fierro: la elección de modelo se cablea " +
		"en el despacho (una regla de ruteo por tipo de tarea), no se deja al criterio de cada turno.",
	FindingContextNoReturn: "El tip ya se dio y las sesiones siguen pasándose de su punto de no-retorno. Fierro: el corte no " +
		"puede depender de que alguien mire el contexto — se cablea donde se lanza la sesión (compactar o reiniciar al llegar " +
		"al turno medido) o se baja el tope de contexto del runtime que la corre, que es configuración y no criterio. " +
		"El número al que cablearlo lo da esta misma medición, por sesión. Ver docs/automejora.md.",
}

// genericMechanism is the fallback for a finding added later without its own
// entry above. It says the one thing that is true of every escalation.
const genericMechanism = "Ese consejo ya se emitió y la métrica no se movió, así que repetirlo no es el arreglo. " +
	"Lo que falta es un mecanismo — un script, un límite duro o un cambio de despacho — que saque la decisión del turno."

func mechanismFor(findingID string) string {
	if mechanism, registered := mechanismByFinding[findingID]; registered {
		return mechanism
	}
	return genericMechanism
}
