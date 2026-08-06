package quota

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/adapters/claudecode"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

// Calibration thresholds. These are policy, not measurements, so they live here
// as named constants with the reasoning that picked them.
const (
	// minCalibrationObservations is the fewest exhaustions that can summarize a
	// ceiling. Below three, a median is a single data point wearing a statistic's
	// clothes.
	minCalibrationObservations = 3

	// maxTrustedDispersion is the coefficient of variation above which observed
	// ceilings are too scattered to derive anything finer than a range. At 25%
	// the spread is already wide; past it, a per-model weight fitted to the same
	// data would be noise with a decimal point.
	maxTrustedDispersion = 0.25

	// dominantModelShare is how much of a window one model must carry for that
	// window to say anything about THAT model's weight. Below it the window is a
	// mixture and cannot separate one model's contribution from another's.
	dominantModelShare = 0.80

	// minObservationsPerModel is how many windows a model must dominate before
	// its weight is worth estimating at all.
	minObservationsPerModel = 3

	// sameWallWithin collapses refusals that are the same wall: when the account
	// runs dry, every agent on it fails within seconds and each writes its own
	// error line. Counting those as separate exhaustions would triple the sample
	// size without adding a single independent observation.
	sameWallWithin = time.Hour
)

// Observation is one quota window that was drained until the provider refused a
// call. It is the only kind of ground truth available about where the ceiling
// sits, so everything calibrated here traces back to a list of these.
type Observation struct {
	Window      SessionWindow `json:"window"`
	ExhaustedAt time.Time     `json:"exhaustedAt"`
	// Elapsed is how long the window survived before it ran dry. This is the
	// number behind "the 5-hour window dies in 3 hours".
	Elapsed time.Duration `json:"elapsed"`
	// ResetDrift is how far the reconstructed window's reset landed from the one
	// the provider announced — this observation's own error bar.
	ResetDrift time.Duration      `json:"resetDrift"`
	Tokens     float64            `json:"tokens"`
	ByModel    map[string]float64 `json:"byModel"`
}

// DominantModel names the model that carried enough of the window to say
// something about its own weight, or "" when the window was a mixture.
func (o Observation) DominantModel() string {
	for model, tokens := range o.ByModel {
		if o.Tokens > 0 && tokens/o.Tokens >= dominantModelShare {
			return model
		}
	}
	return ""
}

// ModelWeight is what a model's tokens are worth against the quota relative to
// the lightest model observed. Derivable is false far more often than not, and
// when it is false the Reason says what is missing — the alternative would be to
// invent a weighting Anthropic has never published, which is exactly what this
// project refuses to do.
type ModelWeight struct {
	Model        string  `json:"model"`
	Observations int     `json:"observations"`
	Derivable    bool    `json:"derivable"`
	Weight       float64 `json:"weight,omitempty"`
	Reason       string  `json:"reason,omitempty"`
}

// Calibration is the empirical answer to "where is the ceiling and does one
// model cost more of it than another", derived only from exhaustions actually
// observed on this machine.
type Calibration struct {
	Observations []Observation `json:"observations"`
	Capacity     Capacity      `json:"capacity"`
	ModelWeights []ModelWeight `json:"modelWeights"`
	// Note states in one sentence what the calibration is worth, including the
	// case where it is worth nothing yet. Surfaces print it verbatim so a thin
	// calibration can never be quoted as a solid one.
	Note string `json:"note"`
}

// Calibrate derives the session-window ceiling from the account's observed
// exhaustions. It never returns a ceiling it did not measure: with too few
// observations Capacity.Known stays false and Note says how many are missing.
func Calibrate(records []aggregate.Record, events []claudecode.LimitEvent, length time.Duration) Calibration {
	windows := SessionWindows(records, length)
	observations := observe(records, events, windows)
	capacity := calibratedCapacity(observations)
	return Calibration{
		Observations: observations,
		Capacity:     capacity,
		ModelWeights: weighModels(observations),
		Note:         calibrationNote(observations, capacity),
	}
}

// observe pins each independent exhaustion to the window it drained and totals
// what that window had consumed by then.
func observe(records []aggregate.Record, events []claudecode.LimitEvent, windows []SessionWindow) []Observation {
	var observations []Observation
	for _, event := range firstOfEachWall(events) {
		window, ok := WindowOf(windows, event.Timestamp)
		if !ok {
			continue
		}
		drained := RecordsIn(records, window.Start, event.Timestamp)
		byModel := map[string]float64{}
		var total float64
		for _, r := range drained {
			tokens := float64(r.PointTokens())
			byModel[r.Model] += tokens
			total += tokens
		}
		observations = append(observations, Observation{
			Window:      window,
			ExhaustedAt: event.Timestamp,
			Elapsed:     event.Timestamp.Sub(window.Start),
			ResetDrift:  ResetDrift(window, event),
			Tokens:      total,
			ByModel:     byModel,
		})
	}
	return observations
}

// firstOfEachWall keeps one refusal per wall hit, in time order.
func firstOfEachWall(events []claudecode.LimitEvent) []claudecode.LimitEvent {
	ordered := append([]claudecode.LimitEvent(nil), events...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Timestamp.Before(ordered[j].Timestamp) })

	var walls []claudecode.LimitEvent
	for _, e := range ordered {
		if n := len(walls); n > 0 && e.Timestamp.Sub(walls[n-1].Timestamp) < sameWallWithin {
			continue
		}
		walls = append(walls, e)
	}
	return walls
}

// calibratedCapacity summarizes the observed ceilings: the median as the point
// figure, the extremes as the range, and the dispersion so nobody reads the
// median as precise.
func calibratedCapacity(observations []Observation) Capacity {
	if len(observations) < minCalibrationObservations {
		return Capacity{}
	}
	ceilings := make([]float64, 0, len(observations))
	for _, o := range observations {
		ceilings = append(ceilings, o.Tokens)
	}
	sort.Float64s(ceilings)
	return Capacity{
		Known:      true,
		Source:     SourceCalibrated,
		Dispersion: dispersion(ceilings),
		Limit: Measure{
			Low:   ceilings[0],
			Point: median(ceilings),
			High:  ceilings[len(ceilings)-1],
		},
	}
}

// weighModels tries to derive each model's weight against the quota from the
// windows it dominated, and reports honestly when it can't. A model weighs more
// when the window died on FEWER of its tokens, so the weight is the inverse of
// the observed ceiling, normalized against the lightest model.
func weighModels(observations []Observation) []ModelWeight {
	byModel := map[string][]float64{}
	for _, o := range observations {
		if model := o.DominantModel(); model != "" {
			byModel[model] = append(byModel[model], o.Tokens)
		}
	}

	weights := make([]ModelWeight, 0, len(byModel))
	for model, ceilings := range byModel {
		weights = append(weights, weighModel(model, ceilings))
	}
	sort.Slice(weights, func(i, j int) bool { return weights[i].Model < weights[j].Model })
	return normalize(needComparison(weights))
}

// minModelsToCompare is how many models must clear the evidence bar before any
// weight is reported. A weight is a RATIO — the absolute scale is arbitrary — so
// one model on its own normalizes to ×1.00 against itself and reads as a derived
// finding when it is nothing but a tautology.
const minModelsToCompare = 2

// needComparison withdraws weights that have nothing to be compared against.
func needComparison(weights []ModelWeight) []ModelWeight {
	derivable := 0
	for _, w := range weights {
		if w.Derivable {
			derivable++
		}
	}
	if derivable >= minModelsToCompare {
		return weights
	}
	for i, w := range weights {
		if !w.Derivable {
			continue
		}
		weights[i].Derivable = false
		weights[i].Weight = 0
		weights[i].Reason = fmt.Sprintf("es el único modelo con ventanas dominadas suficientes (%d); "+
			"un peso es una razón contra otro modelo y no hay con quién compararlo", w.Observations)
	}
	return weights
}

// weighModel judges one model's evidence: enough dominated windows, and enough
// agreement between them.
func weighModel(model string, ceilings []float64) ModelWeight {
	weight := ModelWeight{Model: model, Observations: len(ceilings)}
	if len(ceilings) < minObservationsPerModel {
		weight.Reason = fmt.Sprintf("solo %d ventana(s) dominadas por este modelo; hacen falta %d",
			len(ceilings), minObservationsPerModel)
		return weight
	}
	spread := dispersion(ceilings)
	if spread > maxTrustedDispersion {
		weight.Reason = fmt.Sprintf("los techos observados se dispersan %.0f%% (tope %.0f%%): la muestra no separa el peso del modelo del ruido",
			spread*100, maxTrustedDispersion*100)
		return weight
	}
	sorted := append([]float64(nil), ceilings...)
	sort.Float64s(sorted)
	weight.Derivable = true
	weight.Weight = 1 / median(sorted)
	return weight
}

// normalize rescales derivable weights so the lightest model is 1, which is the
// only form in which they mean anything: the absolute scale is arbitrary, the
// ratio between models is the finding.
func normalize(weights []ModelWeight) []ModelWeight {
	lightest := math.Inf(1)
	for _, w := range weights {
		if w.Derivable && w.Weight < lightest {
			lightest = w.Weight
		}
	}
	if math.IsInf(lightest, 1) || lightest <= 0 {
		return weights
	}
	for i, w := range weights {
		if w.Derivable {
			weights[i].Weight = w.Weight / lightest
		}
	}
	return weights
}

// calibrationNote states what this calibration is actually worth. It is written
// for the case where the answer is "not much yet", because that is the answer
// the data gives today and hiding it behind a confident-looking number is the
// failure this whole layer is built to avoid.
func calibrationNote(observations []Observation, capacity Capacity) string {
	if len(observations) < minCalibrationObservations {
		return fmt.Sprintf("Sin techo calibrado: %d agotamiento(s) observado(s), hacen falta %d. "+
			"El %% de ventana no se puede calcular todavía; lo que sí es real es el consumo y el ritmo.",
			len(observations), minCalibrationObservations)
	}
	note := fmt.Sprintf("Techo %s con %d agotamientos observados en esta máquina: mediana %.0fM tokens, "+
		"rango %.0fM–%.0fM, dispersión ±%.0f%%.",
		SourceCalibrated, len(observations),
		capacity.Limit.Point/1e6, capacity.Limit.Low/1e6, capacity.Limit.High/1e6,
		capacity.Dispersion*100)
	if capacity.Dispersion > maxTrustedDispersion {
		note += fmt.Sprintf(" Con esa dispersión (tope %.0f%%) el %% de ventana es orientativo, no una medición: "+
			"úsalo para saber si vas holgado o al filo, no como un contador exacto.",
			maxTrustedDispersion*100)
	}
	return note
}

// dispersion is the coefficient of variation: the spread relative to the mean,
// which is the form that can be compared against a threshold regardless of
// whether the figures are millions of tokens or dollars.
func dispersion(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))
	if mean <= 0 {
		return 0
	}
	var squares float64
	for _, v := range values {
		squares += (v - mean) * (v - mean)
	}
	return math.Sqrt(squares/float64(len(values))) / mean
}

// median expects values already sorted ascending.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
