package body

import (
	"fmt"
	"strings"
)

// Body tendencies (1.1): what a person's body is typically like in her
// cycle phase or pregnancy, for the story. Go gives the phase and a few
// tendency words; the main model decides whether and how to show them.
// Modeled and typical, never an observed fact or a diagnosis.

// TendencyBudget is the tendency section's own ceiling (3,000 chars,
// apart from the body facts' Budget). A ceiling, not a target.
const TendencyBudget = 3000

// TendencyHeader opens the tendency lines.
const TendencyHeader = "[Archive Center: body tendencies — modeled, typical for the phase; the person feels them, others notice only what shows; show one only when it fits the scene, in your own words, never as a label or diagnosis]"

// TendencyEvery: the tendencies go with one turn in this many (a line
// sent every turn was shown every turn, and asking for less in the header
// did not change it — live check r2). The main model keeps what it showed
// from the conversation.
const TendencyEvery = 3

// TendencyTurn reports whether the tendencies go with the turn numbered
// turn (1, 4, 7, …): the same turn, rerolled, decides the same.
func TendencyTurn(turn int) bool { return turn%TendencyEvery == 1%TendencyEvery }

// Intensity is how strongly a person feels each kind of change: 0 none, 1
// mild, 2 moderate, 3 severe. Drawn once per person (random), kept, and
// editable on the body screen.
type Intensity struct {
	Cramps     int `json:"cramps"`
	PMS        int `json:"premenstrual"`
	Nausea     int `json:"nausea"`
	Discomfort int `json:"pregnancy_discomfort"`
	// Recovery is how fast she recovers after a pregnancy ends ("회복이 빠른 사람도
	// 있고 느린 사람도"): 1 fast, 2 usual, 3 slow; 0 not drawn yet (kept before
	// 1.1.0-test.6)
	Recovery int `json:"recovery_speed"`
}

// recoveryOdds are the chances (percent) of fast, usual and slow.
var recoveryOdds = [3]int{25, 50, 25}

// recoveryFactor stretches the recovery's days by its speed.
var recoveryFactor = map[int]float64{1: 0.7, 2: 1, 3: 1.4}

// intensityOdds are the chances (percent) of none, mild, moderate and
// severe for each kind (roughly following real ranges; a story model, not
// statistics).
var intensityOdds = map[string][4]int{
	"cramps":               {15, 40, 30, 15},
	"premenstrual":         {15, 50, 27, 8},
	"nausea":               {25, 35, 37, 3},
	"pregnancy_discomfort": {10, 40, 40, 10},
}

// DrawIntensity draws a person's intensity from the chat's seed: the same
// seed and person always give the same.
func DrawIntensity(seed, who string) Intensity {
	draw := func(kind string) int {
		u, _ := random(seed, who, "body_intensity.v1", 0, kind, 0)
		at := 0.0
		for level, odds := range intensityOdds[kind] {
			at += float64(odds) / 100
			if u < at {
				return level
			}
		}
		return 3
	}
	speed := 3
	u, _ := random(seed, who, "body_intensity.v1", 0, "recovery_speed", 0)
	at := 0.0
	for i, odds := range recoveryOdds {
		at += float64(odds) / 100
		if u < at {
			speed = i + 1
			break
		}
	}
	return Intensity{Cramps: draw("cramps"), PMS: draw("premenstrual"), Nausea: draw("nausea"), Discomfort: draw("pregnancy_discomfort"), Recovery: speed}
}

// Valid reports levels in 0..3.
func (i Intensity) Valid() bool {
	for _, v := range []int{i.Cramps, i.PMS, i.Nausea, i.Discomfort, i.Recovery} {
		if v < 0 || v > 3 {
			return false
		}
	}
	return true
}

// Tendency is a person's typical body changes on the day read.
type Tendency struct {
	Phase string   // e.g. "premenstrual (cycle day 24~26)"
	Words []string // the tendencies, intensity applied
	// Either: the cycle day may fall in more than one phase; each word is
	// one phase's group ("if luteal: …"), not felt together
	Either bool
	// CauseUnknown: a modeled pregnancy no one knows of; she feels the
	// changes without knowing they come from a pregnancy
	CauseUnknown bool
	// Note is a guide said after the words (after a loss: feelings follow
	// the story, never a word list)
	Note string
}

// a tendency word; kind names the intensity that scales it ("" always)
type tword struct{ text, kind string }

var (
	menstrualWords    = []tword{{"cramps", "cramps"}, {"lower back ache", "cramps"}, {"fatigue", ""}, {"low energy", ""}}
	follicularWords   = []tword{{"rising energy", ""}, {"brighter mood", ""}, {"more sociable", ""}}
	ovulatoryWords    = []tword{{"heightened libido", ""}, {"confidence", ""}, {"mild pelvic twinge", ""}}
	lutealWords       = []tword{{"calmer, lower energy", ""}, {"warmer body", ""}, {"appetite up", "premenstrual"}}
	premenstrualWords = []tword{{"bloating", "premenstrual"}, {"breast tenderness", "premenstrual"}, {"irritability", "premenstrual"}, {"cravings", "premenstrual"}, {"mood swings", "premenstrual"}}
	veryEarlyWords    = []tword{{"unusual tiredness", ""}, {"breast tenderness", ""}}
	earlyWords        = []tword{{"nausea", "nausea"}, {"food aversions", "nausea"}, {"fatigue", ""}, {"breast tenderness", ""}, {"frequent urination", ""}}
	midWords          = []tword{{"more energy", ""}, {"growing belly", ""}, {"round ligament aches", "pregnancy_discomfort"}, {"back ache", "pregnancy_discomfort"}}
	lateWords         = []tword{{"back pain", "pregnancy_discomfort"}, {"shortness of breath", "pregnancy_discomfort"}, {"swollen feet", "pregnancy_discomfort"}, {"poor sleep", "pregnancy_discomfort"}, {"frequent urination", ""}}
	recoveryWords     = []tword{{"soreness", ""}, {"exhaustion", ""}, {"mood swings", ""}}
	// after a birth, by days since it
	birthEarlyWords = []tword{{"bleeding", ""}, {"cramping as the womb shrinks", ""}, {"soreness where she is healing", ""}, {"deep exhaustion", ""}, {"hard to move far", ""}, {"breasts filling", ""}}
	birthMidWords   = []tword{{"still healing", ""}, {"broken sleep", ""}, {"tires quickly", ""}, {"mood swings", ""}}
	birthLateWords  = []tword{{"strength coming back slowly", ""}, {"tires more easily than before", ""}}
	// after a loss: the body only; how she feels is the story's
	lossEarlyWords = []tword{{"bleeding", ""}, {"cramps", ""}, {"tiredness", ""}}
	lossLateWords  = []tword{{"tiredness", ""}, {"body still settling", ""}}
)

// lossNote: feelings after a loss depend on what the pregnancy meant to her
// (unwanted, or wanted for ten years); no feeling is given.
const lossNote = "how she feels depends on what this pregnancy meant to her in the story; follow the story, assume nothing"

// recoveryOf is the open recovery (followed in story order: its outcome is
// the end it follows; a recovery end or a new pregnancy closed it) by days
// since it began, its stages stretched by her recovery speed; none once it
// is over or its days cannot be counted on now's axis. It counts from the
// end, or from a recovery the story started later; base is where.
func recoveryOf(st State, in Intensity) (*Tendency, Day) {
	ep := st.Recovering
	if ep == nil {
		return nil, Day{}
	}
	outcome := ep.Outcome
	base, ok := ep.Ended, ep.Ended.Same(st.Now) && ep.Ended.N <= st.Now.N
	if ep.Started.Same(st.Now) && ep.Started.N <= st.Now.N && (!ok || ep.Started.N > base.N) {
		base, ok = ep.Started, true
	}
	if !ok {
		return nil, base
	}
	f := recoveryFactor[in.Recovery]
	if f == 0 {
		f = 1
	}
	days := st.Now.N - base.N
	within := func(limit int) bool { return float64(days) < float64(limit)*f }
	t := &Tendency{}
	switch outcome {
	case "birth":
		switch {
		case within(14):
			t.Words = in.words(birthEarlyWords)
		case within(42):
			t.Words = in.words(birthMidWords)
		case within(84):
			t.Words = in.words(birthLateWords)
		default:
			return nil, base
		}
		t.Phase = fmt.Sprintf("after the birth (day %d)", days+1)
	case "loss":
		switch {
		case within(7):
			t.Words = in.words(lossEarlyWords)
		case within(28):
			t.Words = in.words(lossLateWords)
		default:
			return nil, base
		}
		t.Phase, t.Note = fmt.Sprintf("after the pregnancy loss (day %d)", days+1), lossNote
	default:
		if !within(42) {
			return nil, base
		}
		t.Phase, t.Words = fmt.Sprintf("recovery after the pregnancy ended (day %d)", days+1), in.words(recoveryWords)
	}
	return t, base
}

// noneText says a kind is not felt (intensity 0), so the main model does not
// fall back on the typical change (review: an omitted word is not "none").
var noneText = map[string]string{"cramps": "no cramps", "premenstrual": "few premenstrual changes", "nausea": "no nausea", "pregnancy_discomfort": "little discomfort"}

func (i Intensity) level(kind string) int {
	switch kind {
	case "cramps":
		return i.Cramps
	case "premenstrual":
		return i.PMS
	case "nausea":
		return i.Nausea
	case "pregnancy_discomfort":
		return i.Discomfort
	}
	return 2
}

// words applies the intensity: none drops a word and says the kind is not
// felt (once), mild and severe mark it.
func (i Intensity) words(ws []tword) []string {
	var out []string
	said := map[string]bool{}
	for _, w := range ws {
		switch i.level(w.kind) {
		case 0:
			if !said[w.kind] {
				said[w.kind] = true
				out = append(out, noneText[w.kind])
			}
			continue
		case 1:
			out = append(out, w.text+" (mild)")
		case 3:
			out = append(out, w.text+" (strong)")
		default:
			out = append(out, w.text)
		}
	}
	return out
}

// cyclePhase names the phase of a cycle day (1 = first day of a period):
// the expected ovulation day is the cycle length less the mean luteal
// length; the last five days are premenstrual.
var phaseWords = map[string][]tword{"menstrual": menstrualWords, "follicular": follicularWords, "ovulatory": ovulatoryWords, "luteal": lutealWords, "premenstrual": premenstrualWords}

func cyclePhase(s Spec, d int) (string, []tword) {
	return phaseOf(s.PeriodDays, s.CycleDays, s.CycleDays-(s.LutealMinDays+s.LutealMaxDays)/2+1, d)
}

// phaseOf names cycle day d of a cycle of the given length whose ovulation
// falls on cycle day ov.
func phaseOf(periodDays, length, ov, d int) (string, []tword) {
	switch {
	case d <= periodDays:
		return "menstrual", menstrualWords
	case d < ov-1:
		return "follicular", follicularWords
	case d <= ov+1:
		return "ovulatory", ovulatoryWords
	case d > length-5:
		return "premenstrual", premenstrualWords
	default:
		return "luteal", lutealWords
	}
}

// tendencyOf is a person's tendency on the day read: a pregnancy's by its
// time since it started, else her cycle phase, else a recovery's. A cycle
// day range across two phases names both; across more, none is given.
func tendencyOf(st State, p Person) *Tendency {
	in := Intensity{2, 2, 2, 2, 2} // moderate when none is kept
	if p.Intensity != nil {
		in = *p.Intensity
	}
	if pg := st.Pregnancy; pg != nil && pg.Stage != "modeled_birth_completed" && pg.start.Same(st.Now) {
		days := st.Now.N - pg.start.N
		var phase string
		var ws []tword
		switch {
		case days < 28:
			phase, ws = "very early pregnancy", veryEarlyWords
		case days < 84:
			phase, ws = "early pregnancy", earlyWords
		case days < 189: // late from week 28 (days/7+1), as designed
			phase, ws = "mid pregnancy", midWords
		default:
			phase, ws = "late pregnancy", lateWords
		}
		words := in.words(ws)
		if !pg.Observed && days > (p.Spec.LutealMinDays+p.Spec.LutealMaxDays)/2 && days < 84 {
			// the period expected a mean luteal phase after the conception has
			// not come: no one knows why (the model's own pregnancy)
			words = append(words, "late period")
		}
		// the time counts from the modeled conception; a pregnancy the story
		// confirmed without one counts from the confirmation, its latest start
		when := fmt.Sprintf("week %d since conception", days/7+1)
		if !pg.startModeled {
			when = fmt.Sprintf("modeled from the confirmation %d days ago; follow the story's timing", days)
		}
		return &Tendency{Phase: phase + " (" + when + ")", Words: words, CauseUnknown: !pg.Observed}
	}
	// a cycle read again from a period start observed on or after the
	// recovery's start comes first; else the recovery does (an earlier
	// period does not)
	rec, base := recoveryOf(st, in)
	recovery := func() *Tendency { return rec }
	if rec != nil && !(st.RefKind == "observed_period_start" && st.CycleRef.Same(base) && st.CycleRef.N >= base.N) {
		return rec
	}
	c := st.Cycle
	if c == nil || c.Status != "estimated" || !p.Spec.Valid() {
		return recovery()
	}
	s := p.Spec
	if d := c.CycleDay.Min; c.PeriodDays > 0 {
		// one drawn cycle: its own length and ovulation
		start := st.Now.N - d + 1
		name, words := phaseOf(c.PeriodDays, c.NextPeriod.Min-start, c.Ovulation.Min-start+1, d)
		return &Tendency{Phase: fmt.Sprintf("%s (cycle day %d)", name, d), Words: in.words(words)}
	}
	// the cycle days possible now, one range per possible number of cycles
	// since the reference (the estimate's CycleDay wraps them into one: a
	// day before the next period and the first days of it are apart)
	lo, hi := s.CycleDays-s.VariationDays, s.CycleDays+s.VariationDays
	e := st.Now.N - st.CycleRef.N
	found := map[string]bool{}
	var days []string
	for k := e / hi; k <= e/lo; k++ {
		a, b := max(0, e-k*hi), min(hi-1, e-k*lo)
		if a > b {
			continue
		}
		days = append(days, span(Range{a + 1, b + 1}))
		for d := a; d <= b; d++ {
			name, _ := cyclePhase(s, d+1)
			found[name] = true
		}
	}
	var names []string
	var groups [][]tword
	for _, name := range []string{"menstrual", "follicular", "ovulatory", "luteal", "premenstrual"} {
		if found[name] {
			names = append(names, name)
			groups = append(groups, phaseWords[name])
		}
	}
	if len(names) == 0 || len(names) > 3 {
		// every phase is possible: nothing typical to tell
		return recovery()
	}
	t := &Tendency{Phase: strings.Join(names, " or ") + " (cycle day " + strings.Join(days, " or ") + ")"}
	if len(names) == 1 {
		t.Words = in.words(groups[0])
		return t
	}
	t.Either = true
	for i, name := range names {
		t.Words = append(t.Words, "if "+name+": "+strings.Join(in.words(groups[i]), ", "))
	}
	return t
}

// TendencyLines writes each person's tendency as delivered lines, in the
// order given, up to TendencyBudget chars with the header.
func TendencyLines(states []State, name func(string) string) []string {
	var out []string
	used := len([]rune(TendencyHeader)) + 1
	for _, st := range states {
		t := st.Tendency
		if t == nil {
			continue
		}
		words := "few noticeable changes"
		if len(t.Words) > 0 {
			sep := ", "
			if t.Either {
				sep = "; "
			}
			words = strings.Join(t.Words, sep)
		}
		if t.Note != "" {
			words += " (" + t.Note + ")"
		}
		who := "felt by her"
		if t.CauseUnknown {
			who = "felt by her; modeled, no one knows it is a pregnancy, she only feels the changes"
		}
		line := fmt.Sprintf("- body tendency %s [%s]: %s — %s", name(st.Who), who, t.Phase, words)
		if used+len([]rune(line))+1 > TendencyBudget {
			break
		}
		if len(out) == 0 {
			out = append(out, TendencyHeader)
		}
		out = append(out, line)
		used += len([]rune(line)) + 1
	}
	return out
}
