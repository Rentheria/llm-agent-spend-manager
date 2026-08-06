package quota

import (
	"sort"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/contextfill"
)

// The context window belongs in the quota report, not in a command of its own.
// This is already the report about windows running out — how much of the 5-hour
// cycle is gone, at what pace, how long it leaves — and the context window is the
// same question asked of a different ceiling: the account's quota runs out for
// everyone at once, a session's context runs out for that session alone, and both
// stop the work mid-task. Someone about to send a turn needs both answers in the
// same breath, so they read in the same output (see docs/ventana-contexto.md §1).

// contextStreamLimit and contextShiftLimit bound the two tables. The point is to
// name the conversations about to hit a wall, not to enumerate every stream on
// the machine — the tail is noise and pushes the answer off the screen.
const (
	contextStreamLimit = 6
	contextShiftLimit  = 5
)

// ContextWindows is the per-conversation view of the OTHER window: how full each
// live context is, and which conversations had their ceiling moved underneath
// them by a mid-session model change.
type ContextWindows struct {
	// WarnAt is the share of the window at which a stream starts being flagged,
	// carried in the payload so every surface warns at the same line and a reader
	// can see which line it was.
	WarnAt  float64         `json:"warnAt"`
	Streams []ContextStream `json:"streams"`
	// Shifts are the model changes that MOVED the ceiling. Changes between two
	// models with the same window are counted in SameCeilingShifts instead: they
	// happen constantly on this fleet (most of its models carry ~1M) and listing
	// them would bury the ones that matter under rows reading "16% → 16%".
	Shifts            []ContextShift `json:"shifts"`
	SameCeilingShifts int            `json:"sameCeilingShifts"`
	// NoWindow are the models that live streams are running on without a derivable
	// ceiling. They are reported instead of dropped: a stream missing from the
	// table would read as a stream with room to spare.
	NoWindow []UnwindowedModel `json:"noWindow"`
	// Blind names the agents whose context cannot be measured at all, with the
	// reason — same role Unmetered plays for the quota cycles.
	Blind map[string]string `json:"blind"`
}

// ContextStream is one live context stream with enough identity to recognize the
// task behind it: the measurement is in Stream, the labels are what make it
// actionable instead of merely true.
type ContextStream struct {
	Agent     string             `json:"agent"`
	Label     string             `json:"label,omitempty"`
	Workspace string             `json:"workspace,omitempty"`
	Stream    contextfill.Stream `json:"stream"`
}

// ContextShift is one mid-session model change, with the conversation it happened
// in. This is the row that answers the incident that opened T22 in words: this
// session went from X% to Y% without writing a single token more.
type ContextShift struct {
	Agent     string            `json:"agent"`
	Label     string            `json:"label,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	SessionID string            `json:"sessionId"`
	ThreadID  string            `json:"threadId,omitempty"`
	Shift     contextfill.Shift `json:"shift"`
}

// Jump is how much the occupancy moved when the ceiling changed, in percentage
// points of the window. It is negative when the change cannot be measured
// (one of the two models has no derivable window), which sorts those rows last
// without pretending their magnitude is zero.
func (s ContextShift) Jump() float64 {
	if !s.Shift.Rescored() {
		return -1
	}
	return abs(s.Shift.After.Share - s.Shift.Before.Share)
}

// UnwindowedModel is a model that live streams are running on while its context
// window isn't derivable, with how many streams and why.
type UnwindowedModel struct {
	Model   string `json:"model"`
	Streams int    `json:"streams"`
	Reason  string `json:"reason"`
}

// ContextBlindAgents are the agents whose context window nobody can measure,
// with the reason. Cursor and Antigravity are activity-tier: they record a
// conversation, not a turn with token counts, so there is no "context this turn
// carried" to compare against any ceiling. Listing them beats omitting them —
// an agent absent from the table otherwise reads as an agent with an empty
// window.
var ContextBlindAgents = map[string]string{
	aggregate.AgentCursor: "no expone tokens por turno (solo actividad por conversación): no hay contexto vivo " +
		"que medir, ni modelo activo contra el que medirlo",
	aggregate.AgentAntigravity: "desde T11 sí se conoce el modelo (decodificado del protobuf), pero el piso de " +
		"tokens es por conversación, no por turno: no hay 'contexto que cargó este turno' que comparar con el techo",
}

// contextWindowsOf builds the whole section. records is EVERY turn read, not just
// the period's: a stream's live occupancy is its last turn and its ceiling changes
// are its whole history, so truncating the input would truncate both. since only
// decides which streams are live enough to report.
func contextWindowsOf(records []aggregate.Record, since time.Time, warnAt float64) ContextWindows {
	warnAt = warnShareOr(warnAt)
	streams := analyzeContextStreams(records, warnAt)
	live := make([]ContextStream, 0, len(streams))
	for _, s := range streams {
		if !s.Stream.LastActivity.Before(since) {
			live = append(live, s)
		}
	}

	moved, sameCeiling := shiftsOf(live)
	return ContextWindows{
		WarnAt:            warnAt,
		Streams:           fullestFirst(live),
		Shifts:            moved,
		SameCeilingShifts: sameCeiling,
		NoWindow:          unwindowedModels(live),
		Blind:             ContextBlindAgents,
	}
}

// contextStream accumulates one (session, thread) pair while records are scanned.
type contextStream struct {
	key       streamKey
	agent     string
	label     string
	workspace string
	turns     []contextfill.Turn
}

type streamKey struct{ sessionID, threadID string }

// analyzeContextStreams groups the measured turns into context streams and
// measures each one.
//
// Two exclusions matter. Activity-tier records carry no token buckets, so their
// context would be a zero that looks measured. And turns whose context is zero
// carry nothing to occupy a window with — Claude Code writes a few (a refusal, a
// <synthetic> turn) and keeping them would fake both a reset and a model change
// around every one of them.
func analyzeContextStreams(records []aggregate.Record, warnAt float64) []ContextStream {
	index := map[streamKey]*contextStream{}
	order := make([]streamKey, 0, len(records))
	for _, r := range records {
		context := r.Input + r.CacheRead + r.CacheWrite
		if r.Confidence != aggregate.ConfidenceMeasured || r.SessionID == "" || context == 0 {
			continue
		}
		key := streamKey{r.SessionID, r.ThreadID}
		current := index[key]
		if current == nil {
			current = &contextStream{key: key, agent: r.Agent, workspace: r.Workspace}
			index[key] = current
			order = append(order, key)
		}
		if current.label == "" {
			current.label = r.SessionLabel
		}
		current.turns = append(current.turns, contextfill.Turn{
			Model:     r.Model,
			Timestamp: r.Timestamp,
			Context:   context,
		})
	}

	out := make([]ContextStream, 0, len(order))
	for _, key := range order {
		s := index[key]
		sort.SliceStable(s.turns, func(i, j int) bool { return s.turns[i].Timestamp.Before(s.turns[j].Timestamp) })
		out = append(out, ContextStream{
			Agent:     s.agent,
			Label:     s.label,
			Workspace: s.workspace,
			Stream:    contextfill.Analyze(key.sessionID, key.threadID, s.turns, warnAt),
		})
	}
	return out
}

// fullestFirst ranks the streams whose ceiling is known by how full they are and
// keeps the top of the list. Streams with no derivable window are left out on
// purpose — they have no share to rank by — and reported as NoWindow instead of
// being sorted in at a fake 0%.
func fullestFirst(streams []ContextStream) []ContextStream {
	ranked := make([]ContextStream, 0, len(streams))
	for _, s := range streams {
		if s.Stream.Live.Known {
			ranked = append(ranked, s)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Stream.Live.Share > ranked[j].Stream.Live.Share
	})
	if len(ranked) > contextStreamLimit {
		return ranked[:contextStreamLimit]
	}
	return ranked
}

// shiftsOf flattens every live stream's model changes, keeping the ones that
// moved the ceiling ranked by how far the occupancy moved — the magnitude the
// ticket is about — and counting the rest.
func shiftsOf(streams []ContextStream) (moved []ContextShift, sameCeiling int) {
	var out []ContextShift
	for _, s := range streams {
		for _, shift := range s.Stream.Shifts {
			if shift.SameCeiling() {
				sameCeiling++
				continue
			}
			out = append(out, ContextShift{
				Agent:     s.Agent,
				Label:     s.Label,
				Workspace: s.Workspace,
				SessionID: s.Stream.SessionID,
				ThreadID:  s.Stream.ThreadID,
				Shift:     shift,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if jumpI, jumpJ := out[i].Jump(), out[j].Jump(); jumpI != jumpJ {
			return jumpI > jumpJ
		}
		return out[i].Shift.At.After(out[j].Shift.At)
	})
	if len(out) > contextShiftLimit {
		out = out[:contextShiftLimit]
	}
	return out, sameCeiling
}

// unwindowedModels rolls the streams with no derivable ceiling up by model, so
// the reason is stated once per model instead of once per stream.
func unwindowedModels(streams []ContextStream) []UnwindowedModel {
	counts := map[string]int{}
	reasons := map[string]string{}
	for _, s := range streams {
		if s.Stream.Live.Known {
			continue
		}
		model := s.Stream.Live.Model
		counts[model]++
		reasons[model] = s.Stream.Live.Reason
	}

	out := make([]UnwindowedModel, 0, len(counts))
	for model, streams := range counts {
		out = append(out, UnwindowedModel{Model: model, Streams: streams, Reason: reasons[model]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Streams != out[j].Streams {
			return out[i].Streams > out[j].Streams
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// warnShareOr keeps a zero-value Config from flagging every session at 0% or
// none at 100%. Production always comes through LoadConfig, which validates the
// range; this covers a caller that builds a Config by hand. The applied threshold
// travels in the payload (ContextWindows.WarnAt), so a reader can always see
// which line was used rather than having to trust one.
func warnShareOr(share float64) float64 {
	if share <= 0 || share >= 1 {
		return contextfill.DefaultWarnShare
	}
	return share
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
