// Package body is AC2's body tracking model (1.0 E09 menstrual cycle, E10
// pregnancy), ported from AC1's fiction model (body_cycle.go,
// body_pregnancy_model.go, body_pregnancy_term.go) with the same defaults
// and formulas. It is an author's story model, not a medical calculation.
// Days are whole story days on one axis (AC2's recorded Day, or a calendar
// date counted in days); every result is computed from the inputs alone.
package body

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
)

// ModelVersion names the pregnancy model; a stored result keeps it.
const ModelVersion = "body_conception_model.v1"

// Spec is one character's model settings (AC1 bodyCharacterConfig).
type Spec struct {
	CycleDays     int     `json:"cycle_days"`
	VariationDays int     `json:"variation_days"`
	PeriodDays    int     `json:"period_days"`
	LutealMinDays int     `json:"luteal_min_days"`
	LutealMaxDays int     `json:"luteal_max_days"`
	Viability     float64 `json:"viability"`
	Peak          float64 `json:"peak"`
	GestationDays int     `json:"gestation_days"`
	CanConceive   bool    `json:"can_conceive"`
	CycleEnabled  bool    `json:"cycle_enabled"`
}

// DefaultSpec is AC1's default (28±2 days, period 5, luteal 10-16,
// viability .4, peak .7, gestation 266).
func DefaultSpec() Spec {
	return Spec{CycleDays: 28, VariationDays: 2, PeriodDays: 5, LutealMinDays: 10, LutealMaxDays: 16,
		Viability: .4, Peak: .7, GestationDays: 266, CanConceive: true, CycleEnabled: true}
}

// Valid reports whether the cycle settings describe a cycle (AC1's check).
func (s Spec) Valid() bool {
	lo, hi := s.CycleDays-s.VariationDays, s.CycleDays+s.VariationDays
	return s.CycleDays > 0 && s.VariationDays >= 0 && lo > 0 && hi >= lo && s.PeriodDays > 0 &&
		s.LutealMinDays > 0 && s.LutealMaxDays >= s.LutealMinDays && s.LutealMaxDays < lo &&
		s.Viability >= 0 && s.Viability <= 1 && s.Peak >= 0 && s.Peak <= 1 && s.GestationDays > 0
}

// Range is a closed range of story days.
type Range struct{ Min, Max int }

// Cycle is the calculated estimate of a cycle on a day: conservative
// ranges, never an observed fact.
type Cycle struct {
	Status string // "estimated" or "unknown"
	Reason string // why unknown
	// CycleDay is the day of the cycle (1 = first day of a period).
	CycleDay Range
	// MenstruationPossible: the day can fall in a period; Menstruation: it
	// falls in one at every possible cycle length.
	MenstruationPossible, Menstruation bool
	FertilePossible                    bool
	NextPeriod, Ovulation, Fertile     Range // absolute days
}

// EstimateCycle estimates the cycle on day now from a period that started
// on day ref (AC1 assessBodyCycle with exact days).
func EstimateCycle(s Spec, ref, now int) Cycle {
	out := Cycle{Status: "unknown"}
	if !s.Valid() {
		out.Reason = "cycle_parameters_unresolved"
		return out
	}
	e := float64(now - ref)
	if e < 0 {
		out.Reason = "story_date_or_reference_unresolved"
		return out
	}
	lo, hi := float64(s.CycleDays-s.VariationDays), float64(s.CycleDays+s.VariationDays)
	minCycle, maxCycle := math.Floor(e/hi), math.Floor(e/lo)
	phaseMin, phaseMax := hi, 0.0
	for _, c := range []float64{minCycle, maxCycle} {
		a := math.Max(0, e-c*hi)
		b := math.Min(hi-1, e-c*lo)
		if a <= b {
			phaseMin, phaseMax = math.Min(phaseMin, a), math.Max(phaseMax, b)
		}
	}
	if phaseMin > phaseMax {
		out.Reason = "cycle_position_unresolved"
		return out
	}
	r, n := float64(ref), float64(now)
	nextMin := math.Max(n+1, r+(minCycle+1)*lo)
	nextMax := math.Min(n+hi, r+(maxCycle+1)*hi)
	ovMin, ovMax := nextMin-float64(s.LutealMaxDays), nextMax-float64(s.LutealMinDays)
	fertilePhaseMin := lo - float64(s.LutealMaxDays) - 5
	fertilePhaseMax := hi - float64(s.LutealMinDays)
	fertileCycleMin := math.Max(0, math.Ceil((e-fertilePhaseMax)/hi))
	fertileCycleMax := math.Floor((e - fertilePhaseMin) / lo)
	out.Status = "estimated"
	out.CycleDay = Range{int(phaseMin) + 1, int(phaseMax) + 1}
	out.MenstruationPossible = phaseMin < float64(s.PeriodDays)
	out.Menstruation = phaseMax < float64(s.PeriodDays)
	out.FertilePossible = fertileCycleMin <= fertileCycleMax
	out.NextPeriod = Range{int(nextMin), int(nextMax)}
	out.Ovulation = Range{int(ovMin), int(ovMax)}
	out.Fertile = Range{int(ovMin) - 5, int(ovMax)}
	return out
}

// Exposure is the pregnancy model's result for one potentially conceiving
// exposure day (AC1 calculateBodyPregnancyEvent): decided from the seed,
// the same inputs always give the same result.
type Exposure struct {
	Version string `json:"model_version"`
	Status  string `json:"status"` // evaluated | not_applicable | unknown
	Reason  string `json:"reason,omitempty"`
	// Implanted: the model has a pregnancy start from this day.
	Implanted       bool    `json:"implanted"`
	ExposureDay     int     `json:"exposure_day"` // the day it was computed for; the days below count on it
	OvulationDay    int     `json:"ovulation_day,omitempty"`
	ImplantationDay int     `json:"implantation_day,omitempty"`
	BirthDay        int     `json:"birth_day,omitempty"` // modeled, never an observed birth
	DayProbability  float64 `json:"day_probability"`     // viability × conditional, for the screen
}

// ExposureInput is what an exposure event states (AC1 exposure fields).
type ExposureInput struct {
	Day            int
	Classification string // potentially_conceiving | not_potentially_conceiving | unknown
	Compatibility  string // compatible | incompatible | unknown
	Contraception  string // none | present | unknown
	AuthorAllowed  bool   // model_profile author_allowed
}

const (
	kernelCenter, kernelWidth = -1.0, 2.0
	supportStart, supportEnd  = -5, 0
	implantMin, implantMax    = 6, 12
)

func conditional(s Spec, offset float64) float64 {
	if offset < supportStart || offset > supportEnd {
		return 0
	}
	x := (offset - kernelCenter) / kernelWidth
	return s.Peak * math.Exp(-.5*x*x)
}

// random is AC1's draw: SHA-256 of a JSON array, the upper 53 bits as a
// fraction in [0,1).
func random(seed, origin, family string, cycle int, purpose string, attempt int) (float64, uint64) {
	key, _ := json.Marshal([]any{ModelVersion, seed, origin, family, cycle, purpose, attempt})
	sum := sha256.Sum256(key)
	v := binary.BigEndian.Uint64(sum[:8])
	return float64(v>>11) / (1 << 53), v
}

func integer(seed, origin, family string, cycle int, purpose string, lo, hi int) int {
	width := uint64(hi-lo) + 1
	threshold := -width % width
	for attempt := 0; ; attempt++ {
		if _, v := random(seed, origin, family, cycle, purpose, attempt); v >= threshold {
			return lo + int(v%width)
		}
	}
}

// Conceive evaluates an exposure on day in.Day for a character whose cycle
// started on day ref (AC1 rules: not applicable when the character cannot
// conceive, the exposure is not conceiving or the partner incompatible;
// contraception and unknown context are not modeled unless the author
// allows it; a seed is needed). origin names the character.
func Conceive(s Spec, seed, origin string, ref int, in ExposureInput) Exposure {
	out := Exposure{Version: ModelVersion, Status: "unknown", ExposureDay: in.Day}
	if !s.CanConceive || in.Classification == "not_potentially_conceiving" || in.Compatibility == "incompatible" {
		out.Status, out.Reason = "not_applicable", "explicit_exposure_or_world_setting"
		return out
	}
	if in.Contraception == "present" {
		out.Reason = "contraception_context_not_modeled"
		return out
	}
	if !in.AuthorAllowed && (in.Classification != "potentially_conceiving" || in.Compatibility == "unknown" || in.Contraception == "unknown") {
		out.Reason = "exposure_context_not_modeled"
		return out
	}
	if !s.Valid() {
		out.Reason = "model_parameters_unresolved"
		return out
	}
	if in.Day < ref {
		out.Reason = "exact_event_date_or_reference_unresolved"
		return out
	}
	if seed == "" {
		out.Reason = "simulation_seed_unavailable"
		return out
	}
	fam, _ := json.Marshal([]any{origin, "day", ref})
	digest := sha256.Sum256(fam)
	dayKey, _ := json.Marshal([]any{"day", in.Day})
	return conceive(s, seed, origin, hex.EncodeToString(digest[:]), string(dayKey), ref, in, out)
}

// conceive draws the cycles of one reference family (the character and the
// cycle start it counts from) up to the exposure day.
// dayKey names the exposure day in its own draw.
func conceive(s Spec, seed, origin, family, dayKey string, ref int, in ExposureInput, out Exposure) Exposure {
	day := float64(in.Day)
	found := false
	var best Exposure
	start := float64(ref)
	for index := 0; start <= day+5; index++ {
		length := integer(seed, origin, family, index, "cycle_length", s.CycleDays-s.VariationDays, s.CycleDays+s.VariationDays)
		luteal := integer(seed, origin, family, index, "luteal_length", s.LutealMinDays, s.LutealMaxDays)
		lag := integer(seed, origin, family, index, "implantation_lag", implantMin, implantMax)
		viability, _ := random(seed, origin, family, index, "cycle_viability", 0)
		end := start + float64(length)
		ovulation := start + float64(length-luteal)
		q := conditional(s, day-ovulation)
		if (start <= day && day < end) || q > 0 {
			draw, _ := random(seed, origin, family, index, "exposure_day:"+dayKey, 0)
			if p := s.Viability * q; p > out.DayProbability {
				out.DayProbability = p
			}
			implantation := int(ovulation) + lag
			if viability < s.Viability && draw < q && (!found || int(ovulation) < best.OvulationDay ||
				(int(ovulation) == best.OvulationDay && implantation < best.ImplantationDay)) {
				found = true
				best = Exposure{OvulationDay: int(ovulation), ImplantationDay: implantation}
			}
		}
		start = end
	}
	out.Status = "evaluated"
	if found {
		out.Implanted, out.OvulationDay, out.ImplantationDay = true, best.OvulationDay, best.ImplantationDay
		// AC1 term: the modeled birth counts gestation from the modeled ovulation
		out.BirthDay = best.OvulationDay + s.GestationDays
	}
	return out
}

// Stage reads a modeled pregnancy on day now (AC1 bodyPregnancyReading):
// future_modeled_potential, modeled_pre_implantation,
// modeled_implanted_pregnancy, modeled_birth_completed (never an observed
// birth), or unknown.
func (e Exposure) Stage(now int) string {
	switch {
	case !e.Implanted:
		return "unknown"
	case now >= e.BirthDay:
		return "modeled_birth_completed"
	case now >= e.ImplantationDay:
		return "modeled_implanted_pregnancy"
	case now >= e.OvulationDay:
		return "modeled_pre_implantation"
	default:
		return "future_modeled_potential"
	}
}

// Toggles are the two global switches (AC1: off until turned on, then kept
// for every chat). Either one turns body tracking on.
type Toggles struct {
	Cycle     bool `json:"cycle_tracking_enabled"`
	Pregnancy bool `json:"automatic_pregnancy_enabled"`
}

// Any reports whether body tracking is on.
func (t Toggles) Any() bool { return t.Cycle || t.Pregnancy }
