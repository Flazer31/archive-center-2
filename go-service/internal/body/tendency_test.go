package body

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// Body tendencies (1.1): cycle phases, pregnancy time, intensity
// drawn per person, their own section and ceiling.

func TestIntensityDrawFollowsTheTable(t *testing.T) {
	if DrawIntensity("s", "N1") != DrawIntensity("s", "N1") {
		t.Fatal("not deterministic")
	}
	const n = 8000
	counts := map[string]*[4]int{"cramps": {}, "premenstrual": {}, "nausea": {}, "pregnancy_discomfort": {}}
	for i := 0; i < n; i++ {
		in := DrawIntensity("seed", fmt.Sprintf("N%d", i))
		if !in.Valid() {
			t.Fatalf("level out of range: %+v", in)
		}
		counts["cramps"][in.Cramps]++
		counts["premenstrual"][in.PMS]++
		counts["nausea"][in.Nausea]++
		counts["pregnancy_discomfort"][in.Discomfort]++
	}
	for kind, c := range counts {
		for level, want := range intensityOdds[kind] {
			got := 100 * float64(c[level]) / n
			if math.Abs(got-float64(want)) > 2 {
				t.Fatalf("%s level %d: %.1f%%, table %d%%", kind, level, got, want)
			}
		}
	}
}

// phaseOn is the tendency on day d after a period on Day 0, read without a
// seed: the range of every cycle length (the drawn cycle has its own tests)
func phaseOn(t *testing.T, d int, in *Intensity) *Tendency {
	t.Helper()
	p := Person{Who: "N1", Spec: DefaultSpec(), Intensity: in}
	st, _, _ := Assess(on, "", p, []Record{{ID: 1, Kind: "period_start", Day: day(0)}}, day(d))
	return st.Tendency
}

func TestCyclePhases(t *testing.T) {
	// default 28 days, luteal 10~16: ovulation expected on cycle day 16
	for d, want := range map[int]string{1: "menstrual", 8: "follicular", 15: "ovulatory", 19: "luteal", 25: "premenstrual"} {
		tn := phaseOn(t, d, nil)
		if tn == nil || !strings.HasPrefix(tn.Phase, want+" (cycle day ") {
			t.Fatalf("Day %d: %+v, want %s", d, tn, want)
		}
	}
	// without a seed the cycle is the range of every length: a cycle day
	// range over more than two phases gives none
	p := Person{Who: "N1", Spec: DefaultSpec()}
	st, _, _ := Assess(on, "", p, []Record{{ID: 1, Kind: "period_start", Day: day(0)}}, day(400))
	if st.Cycle == nil || st.Cycle.CycleDay.Max-st.Cycle.CycleDay.Min < 20 || st.Tendency != nil {
		t.Fatalf("a wide range names no phase: %+v %+v", st.Cycle, st.Tendency)
	}
	// with the seed it is one drawn cycle: one day and a phase
	st, _, _ = Assess(on, "seed", p, []Record{{ID: 1, Kind: "period_start", Day: day(0)}}, day(400))
	if st.Cycle == nil || st.Cycle.CycleDay.Min != st.Cycle.CycleDay.Max || st.Tendency == nil || !strings.Contains(st.Tendency.Phase, fmt.Sprintf("(cycle day %d)", st.Cycle.CycleDay.Min)) {
		t.Fatalf("a drawn cycle names its day and phase: %+v %+v", st.Cycle, st.Tendency)
	}
}

func TestIntensityScalesWords(t *testing.T) {
	none := phaseOn(t, 1, &Intensity{Cramps: 0, PMS: 2, Nausea: 2, Discomfort: 2})
	strong := phaseOn(t, 1, &Intensity{Cramps: 3, PMS: 2, Nausea: 2, Discomfort: 2})
	// none is said once, so the main model does not fall back on the typical
	// change; severe marks the words
	n := strings.Join(none.Words, ",")
	if !strings.Contains(n, "no cramps") || strings.Contains(n, "lower back ache") || strings.Count(n, "cramps") != 1 || !strings.Contains(strings.Join(strong.Words, ","), "cramps (strong)") {
		t.Fatalf("intensity not applied: %v / %v", none.Words, strong.Words)
	}
}

func TestPregnancyTendencies(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec(), Intensity: &Intensity{2, 2, 2, 2, 2}}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)}, {ID: 2, Kind: "pregnancy_confirmed", Day: day(40)}}
	for now, want := range map[int]string{50: "very early pregnancy", 100: "early pregnancy", 160: "mid pregnancy", 260: "late pregnancy"} {
		st, _, _ := Assess(on, "seed", p, recs, day(now))
		// a confirmation without a modeled conception counts from the
		// confirmation and says so
		if st.Tendency == nil || !strings.HasPrefix(st.Tendency.Phase, want+" (modeled from the confirmation ") || st.Tendency.CauseUnknown {
			t.Fatalf("Day %d: %+v, want %s", now, st.Tendency, want)
		}
	}
	st, _, _ := Assess(on, "seed", p, recs, day(100))
	if !strings.Contains(strings.Join(st.Tendency.Words, ","), "nausea") {
		t.Fatalf("early pregnancy words: %v", st.Tendency.Words)
	}
	// a modeled pregnancy: she feels it, no one knows it is a pregnancy
	recs = []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Result: implanted(11, 12, 20, 278)}}
	st, _, _ = Assess(on, "seed", p, recs, day(60))
	l := TendencyLines([]State{st}, func(id string) string { return id })
	if st.Tendency == nil || !st.Tendency.CauseUnknown || len(l) != 2 || !strings.Contains(l[1], "no one knows it is a pregnancy") ||
		!strings.Contains(st.Tendency.Phase, "since conception") || !strings.Contains(l[1], "late period") {
		t.Fatalf("modeled pregnancy tendency: %+v %v", st.Tendency, l)
	}
	// the period is not late before it was due: implantation day
	if st, _, _ := Assess(on, "seed", p, recs, day(20)); st.Tendency == nil || strings.Contains(strings.Join(st.Tendency.Words, ","), "late period") {
		t.Fatalf("late period before it was due: %+v", st.Tendency)
	}
	// the fact line and the tendency agree on the time
	st, _, _ = Assess(on, "seed", p, recs, day(210))
	if f := Lines([]State{st}, func(id string) string { return id }); strings.Contains(f[0], "early pregnancy") || !strings.HasPrefix(st.Tendency.Phase, "late pregnancy") {
		t.Fatalf("fact %v / tendency %+v", f, st.Tendency)
	}
	// a recovery after a pregnancy ended, not named a birth (a loss too)
	recs = []Record{{ID: 1, Kind: "recovery_started", Day: day(300)}}
	if st, _, _ := Assess(on, "seed", p, recs, day(305)); st.Tendency == nil || !strings.HasPrefix(st.Tendency.Phase, "recovery after the pregnancy ended (day ") ||
		strings.Contains(strings.Join(st.Tendency.Words, ","), "breast") {
		t.Fatalf("recovery: %+v", st.Tendency)
	}
	// a period after the end: the cycle comes first, also when no recovery
	// end was told
	recs = append(recs, Record{ID: 2, Kind: "pregnancy_ended", Day: day(299)}, Record{ID: 3, Kind: "period_start", Day: day(340)})
	if st, _, _ := Assess(on, "seed", p, recs, day(343)); st.Tendency == nil || !strings.HasPrefix(st.Tendency.Phase, "menstrual") {
		t.Fatalf("cycle after recovery: %+v", st.Tendency)
	}
}

func TestTendencyLinesCeiling(t *testing.T) {
	if l := TendencyLines(nil, func(id string) string { return id }); l != nil {
		t.Fatalf("no tendency, no header: %v", l)
	}
	var states []State
	for i := 0; i < 40; i++ {
		states = append(states, State{Who: fmt.Sprintf("N%d", i), Tendency: &Tendency{Phase: "premenstrual (cycle day 25)", Words: []string{"bloating", "breast tenderness", "irritability", "cravings", "mood swings"}}})
	}
	l := TendencyLines(states, func(id string) string { return id })
	used := 0
	for _, x := range l {
		used += len([]rune(x)) + 1
	}
	if l[0] != TendencyHeader || used > TendencyBudget || len(l) >= 41 || len(l) < 10 {
		t.Fatalf("ceiling: %d lines, %d chars", len(l), used)
	}
}

// A pregnancy under way: an exposure is not drawn (AC1); an exposure of
// the fertile window still names its partner.
func TestNoDrawDuringPregnancy(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40)},
		{ID: 3, Kind: "conception_exposure", Day: day(60), Exposure: poss}}
	_, _, frozen := Assess(on, "seed", p, recs, day(70))
	if got := frozen[3]; got.Status != "not_applicable" || got.Reason != "observed_pregnancy_ongoing" {
		t.Fatalf("observed: %+v", got)
	}
	recs = []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"NA"}, Result: implanted(11, 12, 20, 278)},
		{ID: 3, Kind: "conception_exposure", Day: day(12), Exposure: poss, Partners: []string{"NB"}},
		{ID: 4, Kind: "conception_exposure", Day: day(40), Exposure: poss, Partners: []string{"NC"}}}
	st, _, frozen := Assess(on, "seed", p, recs, day(50))
	// read as skipped, not kept: the modeled pregnancy may be stopped later
	// (emergency contraception, 1.1)
	read := map[int64]*Exposure{}
	for _, f := range st.Facts {
		read[f.ID] = f.Result
	}
	if read[3] == nil || read[4] == nil || read[3].Reason != "modeled_pregnancy_ongoing" || read[4].Reason != "modeled_pregnancy_ongoing" {
		t.Fatalf("modeled: %+v %+v", read[3], read[4])
	}
	if _, kept := frozen[3]; kept {
		t.Fatalf("a skip under the modeled pregnancy is kept: %+v", frozen[3])
	}
	if pg := st.Pregnancy; pg == nil || strings.Join(pg.Candidates, ",") != "NA,NB" {
		t.Fatalf("the window's partner still joins: %+v", pg)
	}
}
