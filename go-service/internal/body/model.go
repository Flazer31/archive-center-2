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
	"fmt"
	"math"
	"slices"
)

// ModelVersion names the pregnancy model; a stored result keeps it.
const ModelVersion = "body_conception_model.v1"

// TermStatus marks the result kept for a confirmed pregnancy: the gestation
// days its term was first read with (Spec.GestationDays), so a later
// settings change does not move it (AC1 keeps modeled_birth_time).
const TermStatus = "confirmed_term"

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
	// PeriodDays is the drawn cycle's own period length (its stored
	// settings); 0 for the range of every length
	PeriodDays int
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

// cycleFamily keys one person's cycles drawn from one start (AC1's family).
func cycleFamily(origin string, ref int) string {
	fam, _ := json.Marshal([]any{origin, "day", ref})
	digest := sha256.Sum256(fam)
	return hex.EncodeToString(digest[:])
}

// drawCycles are a person's cycles from the one day is in through day+5:
// a stored cycle as stored, the days between drawn from the seed with the
// settings now (each cycle's length, luteal phase, implantation lag and
// viability). The pregnancy model and the cycle shown read the same cycles.
func drawCycles(s Spec, seed, origin string, ref int, stored []StoredCycle, day int) []StoredCycle {
	family := cycleFamily(origin, ref)
	// where the cycles start: the stored cycle the day is in, else the end
	// of the last stored cycle before it, else the reference
	pos, index := ref, 0
	for _, c := range stored {
		if c.Start <= day {
			if day < c.End {
				pos, index = c.Start, c.Index
			} else {
				pos, index = c.End, c.Index+1
			}
		}
	}
	var cycles []StoredCycle
	for pos <= day+5 {
		at := -1
		stop, stopOK := 0, false
		for i, c := range stored {
			if c.Start <= pos && pos < c.End {
				at = i
			} else if c.Start > pos && !stopOK {
				stop, stopOK = c.Start, true
			}
		}
		var c StoredCycle
		if at >= 0 {
			c = stored[at]
		} else {
			c = StoredCycle{Index: index, Start: pos, Spec: s}
			c.End = pos + integer(seed, origin, family, index, "cycle_length", s.CycleDays-s.VariationDays, s.CycleDays+s.VariationDays)
			c.Luteal = integer(seed, origin, family, index, "luteal_length", s.LutealMinDays, s.LutealMaxDays)
			c.Lag = integer(seed, origin, family, index, "implantation_lag", implantMin, implantMax)
			c.Viability, _ = random(seed, origin, family, index, "cycle_viability", 0)
			if stopOK && c.End > stop {
				c.End = stop // a stored cycle starts here
			}
		}
		// a cycle over before the day has no part in it (its ovulation is
		// before the day): it is drawn for the ones after it, not kept
		if c.End > day {
			cycles = append(cycles, c)
		}
		pos, index = c.End, c.Index+1
	}
	return cycles
}

// CycleOn is the cycle on day now as the model draws it from a period that
// started on day ref (the same cycles the pregnancy model reads): one cycle
// day, whether a period is under way, the fertile window and ovulation of
// the cycle now, and the day the next period starts.
func CycleOn(s Spec, seed, origin string, ref int, stored []StoredCycle, now int) Cycle {
	out := Cycle{Status: "unknown"}
	if !s.Valid() {
		out.Reason = "cycle_parameters_unresolved"
		return out
	}
	if now < ref {
		out.Reason = "story_date_or_reference_unresolved"
		return out
	}
	for _, c := range drawCycles(s, seed, origin, ref, stored, now) {
		if now < c.Start || now >= c.End {
			continue
		}
		day := now - c.Start + 1
		ovulation := max(c.Start, c.End-c.Luteal)
		period := c.Spec.PeriodDays
		out.Status = "estimated"
		out.CycleDay = Range{day, day}
		out.PeriodDays = period
		out.Menstruation = day <= period
		out.MenstruationPossible = out.Menstruation
		out.Ovulation = Range{ovulation, ovulation}
		out.NextPeriod = Range{c.End, c.End}
		// the fertile window not over yet: this cycle's, or once its
		// ovulation is past, the next one's, which in a short cycle begins
		// before this one ends (the pregnancy model reads both)
		out.Fertile = Range{ovulation + supportStart, ovulation + supportEnd}
		if now > out.Fertile.Max {
			for _, n := range drawCycles(s, seed, origin, ref, stored, c.End) {
				if n.Start == c.End {
					next := max(n.Start, n.End-n.Luteal)
					out.Fertile = Range{next + supportStart, next + supportEnd}
				}
			}
		}
		out.FertilePossible = now >= out.Fertile.Min && now <= out.Fertile.Max
		return out
	}
	out.Reason = "cycle_position_unresolved"
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
	// the cycle the exposure fell in and the settings it was drawn with, and
	// every cycle that took part in the result (the one it fell in and one
	// whose fertile window reached it), as drawn: a stored cycle is a fact of
	// the model, never drawn again (AC1 keeps each evaluated cycle; a later
	// settings change applies to later cycles)
	Ref        int           `json:"reference_day,omitempty"`
	CycleIndex int           `json:"cycle_index,omitempty"`
	CycleStart int           `json:"cycle_start,omitempty"`
	CycleEnd   int           `json:"cycle_end,omitempty"`
	Spec       Spec          `json:"cycle_spec"` // zero (not Valid) in results from before 1.1
	Cycles     []StoredCycle `json:"cycles,omitempty"`
	// the contraception the exposure was drawn with and its factor (1.1)
	Contraception       string  `json:"contraception,omitempty"`
	ContraceptionFactor float64 `json:"contraception_factor,omitempty"`
	// EmergencyDraw is drawn with the result and kept with it: whether
	// emergency contraception would stop this pregnancy is the same however
	// the result is read later (a branch, a past attached, names merged).
	// nil in results drawn before 1.1, which are never stopped
	EmergencyDraw *float64 `json:"emergency_draw,omitempty"`
}

// EmergencyStops reports whether emergency contraception on day taken (on
// the result's days) stops the pregnancy this result draws: taken within
// EmergencyDays after the exposure and before the ovulation, it stops it
// EmergencyBlock of the time, by the draw kept with the result. It is read
// with the result, never frozen into it: removing the fact brings the
// pregnancy back.
func (e Exposure) EmergencyStops(taken int) bool {
	if !e.Implanted || e.EmergencyDraw == nil || taken < e.ExposureDay || taken > e.ExposureDay+EmergencyDays || taken >= e.OvulationDay {
		return false
	}
	return *e.EmergencyDraw < EmergencyBlock
}

// StoredCycle is one cycle of the model as drawn, on the days its result
// counts (ExposureDay's), with the settings it was drawn with.
type StoredCycle struct {
	Index     int     `json:"index"`
	Start     int     `json:"start"`
	End       int     `json:"end"`
	Luteal    int     `json:"luteal"`
	Lag       int     `json:"lag"`
	Viability float64 `json:"viability"`
	Spec      Spec    `json:"spec"`
}

// ovulationSpec is the settings of the stored cycle the modeled ovulation
// fell in, else those of the exposure's cycle.
func (e Exposure) ovulationSpec() Spec {
	for _, c := range e.Cycles {
		if c.Start <= e.OvulationDay && e.OvulationDay < c.End {
			return c.Spec
		}
	}
	return e.Spec
}

// storedCycles are the cycles stored with evaluated results of the same
// reference, moved to the story's days now and sorted by day. Where two
// overlap (results of two names merged) the one earlier in the story is
// kept: what the story established first stands.
func storedCycles(recs []Record, ref, day Day) []StoredCycle {
	var out []StoredCycle
	for _, r := range recs {
		x := r.Result
		if x == nil || x.Status != "evaluated" || !r.Day.Same(day) {
			continue
		}
		shift := r.Day.N - x.ExposureDay
		if x.Ref+shift != ref.N {
			continue
		}
	next:
		for _, c := range x.Cycles {
			c.Start, c.End = c.Start+shift, c.End+shift
			if c.End <= c.Start || !c.Spec.Valid() {
				continue
			}
			for _, o := range out {
				if c.Start < o.End && o.Start < c.End {
					continue next
				}
			}
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b StoredCycle) int { return a.Start - b.Start })
	return out
}

// ExposureInput is what an exposure event states (AC1 exposure fields).
type ExposureInput struct {
	Day            int
	Classification string // potentially_conceiving | not_potentially_conceiving | unknown
	Compatibility  string // compatible | incompatible | unknown
	Contraception  string // none | present | unknown
	// Method is the contraception the text names when present (one of
	// ContraceptionFactors; 1.1)
	Method string
}

// Redrawn are the reasons a result was kept without a draw that are read
// again: skipped for the model's pregnancy under way (it may be stopped
// since: emergency contraception), and not drawn by the rules before 1.1
// (a partner, a kind of act or contraception the text left unsaid). Read
// again, a result is kept as drawn then.
var Redrawn = map[string]bool{"modeled_pregnancy_ongoing": true, "exposure_context_not_modeled": true, "contraception_context_not_modeled": true}

// neverConceives reports an exposure whose contraception stops conception
// always (a factor of 0: magic).
func (in ExposureInput) neverConceives() bool {
	f, ok := ContraceptionFactors[in.Method]
	return in.Contraception == "present" && ok && f == 0
}

// ContraceptionFactors scale a day's chance of conception by the method used
// (typical-use first-year failure against no method, about 85%; magic stops
// it always). A method not named here is read as other.
var ContraceptionFactors = map[string]float64{"condom": 0.15, "pill": 0.08, "withdrawal": 0.25,
	"iud": 0.01, "implant": 0.001, "injection": 0.05, "patch": 0.08, "magic": 0, "other": 0.15}

// DefaultMethod is the method of contraception the text names without
// saying which.
const DefaultMethod = "condom"

// EmergencyBlock is the share of conceptions emergency contraception stops
// when taken within EmergencyDays after the act and before that cycle's
// ovulation.
const (
	EmergencyBlock = 0.85
	EmergencyDays  = 5
)

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
	return conceiveWith(s, seed, origin, ref, nil, in)
}

// conceiveWith is Conceive around stored cycles (storedCycles): a stored
// cycle is used as stored, with its settings; the days between are drawn
// with the settings now, continuing the numbering of the stored cycle
// before, and a drawn cycle ends where a stored one starts. Whether the
// person can conceive, and the rest of the rules, are the settings now.
func conceiveWith(s Spec, seed, origin string, ref int, stored []StoredCycle, in ExposureInput) Exposure {
	out := Exposure{Version: ModelVersion, Status: "unknown", ExposureDay: in.Day}
	if !s.CanConceive || in.Classification == "not_potentially_conceiving" || in.Compatibility == "incompatible" {
		out.Status, out.Reason = "not_applicable", "explicit_exposure_or_world_setting"
		return out
	}
	factor := 1.0
	if in.Contraception == "present" {
		method := in.Method
		if method == "" {
			// contraception said, its method not (a condom by default)
			method = DefaultMethod
		}
		f, ok := ContraceptionFactors[method]
		if !ok {
			// a method the list does not have is other (every
			// contraception the story names is drawn)
			method, f = "other", ContraceptionFactors["other"]
		}
		factor = f
		out.Contraception, out.ContraceptionFactor = method, f
	}
	// An exposure the critic recorded is drawn on the person's own body
	// (her cycle decides, not facts about the partner; a partner or a
	// kind of act the text leaves unsaid stopped every exposure).
	// Contraception the text does not name is none. Only a story that
	// says it cannot conceive (above) is not drawn.
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
	family := cycleFamily(origin, ref)
	dayKey, _ := json.Marshal([]any{"day", in.Day})
	cycles := drawCycles(s, seed, origin, ref, stored, in.Day)
	day := float64(in.Day)
	found := false
	var best Exposure
	var bestSpec Spec
	for _, c := range cycles {
		inside := c.Start <= in.Day && in.Day < c.End
		if inside {
			out.CycleIndex, out.CycleStart, out.CycleEnd, out.Spec = c.Index, c.Start, c.End, c.Spec
		}
		ovulation := float64(max(c.Start, c.End-c.Luteal))
		q := conditional(c.Spec, day-ovulation)
		if !inside && q <= 0 {
			continue
		}
		q *= factor
		out.Cycles = append(out.Cycles, c)
		draw, _ := random(seed, origin, family, c.Index, "exposure_day:"+string(dayKey), 0)
		if p := c.Spec.Viability * q; p > out.DayProbability {
			out.DayProbability = p
		}
		implantation := int(ovulation) + c.Lag
		if c.Viability < c.Spec.Viability && draw < q && (!found || int(ovulation) < best.OvulationDay ||
			(int(ovulation) == best.OvulationDay && implantation < best.ImplantationDay)) {
			found, bestSpec = true, c.Spec
			best = Exposure{OvulationDay: int(ovulation), ImplantationDay: implantation}
		}
	}
	out.Status = "evaluated"
	out.Ref = ref
	if found {
		draw, _ := random(seed, origin, family, out.CycleIndex, fmt.Sprintf("emergency_contraception:%d", in.Day), 0)
		out.EmergencyDraw = &draw
	}
	if found {
		out.Implanted, out.OvulationDay, out.ImplantationDay = true, best.OvulationDay, best.ImplantationDay
		// AC1 term: the modeled birth counts gestation from the modeled ovulation
		out.BirthDay = best.OvulationDay + bestSpec.GestationDays
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
	// WaitLatest: a request waits (at most WaitMax) for the latest reply's
	// memory, so its days and body events count (off by default: the critic
	// takes tens of seconds)
	WaitLatest bool `json:"wait_for_latest_reply"`
}

// Any reports whether body tracking is on.
func (t Toggles) Any() bool { return t.Cycle || t.Pregnancy }
