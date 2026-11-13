package body

import (
	"slices"
	"testing"
)

// An exposure in cycles already drawn draws
// them again where they were drawn, whatever moved since; the settings now
// decide only whether the person can conceive.

func fixedSpec() Spec {
	s := DefaultSpec()
	s.VariationDays = 0 // every cycle 28 days: boundaries are known
	return s
}

func exposureOn(d int) ExposureInput {
	return ExposureInput{Day: d, Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "none"}
}

// Days before 1970 count negative; the last evaluated cycle is still found.
func TestDrawContinuesOnNegativeDays(t *testing.T) {
	cal := func(n int) Day { return Day{"calendar", n} }
	ref := -25567 // 1900/01/01
	first := Conceive(fixedSpec(), "seed", "N1", ref, exposureOn(ref+5))
	recs := []Record{{ID: 1, Kind: "conception_exposure", Day: cal(ref + 5), Result: &first}}
	changed := fixedSpec()
	changed.CycleDays = 40
	in := exposureOn(first.CycleEnd + 1)
	if got := conceiveWith(changed, "seed", "N1", ref, storedCycles(recs, cal(ref), cal(first.CycleEnd+1)), in); got.CycleIndex != first.CycleIndex+1 || got.CycleStart != first.CycleEnd || got.Spec != changed {
		t.Fatalf("the next cycle starts at the end of the evaluated one: %+v (first %d..%d)", got, first.CycleStart, first.CycleEnd)
	}
}

// A past attached before the story moves its days: a new exposure in a
// cycle already evaluated gets the same cycle, moved with them.
func TestDrawKeepsCycleWhenDaysMove(t *testing.T) {
	spec := DefaultSpec() // varying lengths: a new draw would differ
	first := Conceive(spec, "seed", "N1", 0, exposureOn(5))
	const move = 1
	p := Person{Who: "N1", Spec: spec}
	recs := []Record{{ID: 9, Kind: "period_start", Day: day(0 + move)},
		{ID: 1, Kind: "conception_exposure", Day: day(5 + move), Exposure: poss, Result: &first},
		{ID: 2, Kind: "conception_exposure", Day: day(first.CycleEnd - 1 + move), Exposure: poss}}
	_, _, frozen := Assess(on, "seed", p, recs, day(first.CycleEnd+move))
	got := frozen[2]
	at := (first.CycleEnd - 1 + move) - got.ExposureDay
	if got.CycleStart+at != first.CycleStart+move || got.CycleEnd+at != first.CycleEnd+move {
		t.Fatalf("moved cycle [%d,%d) became [%d,%d)", first.CycleStart+move, first.CycleEnd+move, got.CycleStart+at, got.CycleEnd+at)
	}
}

// A result drawn under a name later merged keeps its cycle for the person
// it is now read as.
func TestDrawKeepsCycleAcrossMerge(t *testing.T) {
	spec := DefaultSpec()
	first := Conceive(spec, "seed", "N2", 0, exposureOn(5))
	p := Person{Who: "N1", Spec: spec}
	recs := []Record{{ID: 9, Kind: "period_start", Day: day(0)},
		{ID: 1, Kind: "conception_exposure", Day: day(5), Exposure: poss, Result: &first},
		{ID: 2, Kind: "conception_exposure", Day: day(first.CycleEnd - 1), Exposure: poss}}
	_, _, frozen := Assess(on, "seed", p, recs, day(first.CycleEnd))
	if got := frozen[2]; got.CycleStart != first.CycleStart || got.CycleEnd != first.CycleEnd {
		t.Fatalf("merged cycle [%d,%d) became [%d,%d)", first.CycleStart, first.CycleEnd, got.CycleStart, got.CycleEnd)
	}
}

// An exposure added between two evaluated cycles is drawn after the earlier
// one with the settings now and its cycle ends where the later one starts:
// neither stored cycle moves.
func TestMiddleExposureKeepsLaterCycle(t *testing.T) {
	old := fixedSpec()
	a := Conceive(old, "seed", "N1", 0, exposureOn(5))
	b := Conceive(old, "seed", "N1", 0, exposureOn(90))
	if a.CycleStart != 0 || a.CycleEnd != 28 || b.CycleStart != 84 || b.CycleEnd != 112 {
		t.Fatalf("fixture: 28-day cycles: %d..%d %d..%d", a.CycleStart, a.CycleEnd, b.CycleStart, b.CycleEnd)
	}
	changed := old
	changed.CycleDays = 40
	p := Person{Who: "N1", Spec: changed}
	recs := []Record{{ID: 9, Kind: "period_start", Day: day(0)},
		{ID: 1, Kind: "conception_exposure", Day: day(5), Exposure: poss, Result: &a},
		{ID: 2, Kind: "conception_exposure", Day: day(90), Exposure: poss, Result: &b},
		{ID: 3, Kind: "conception_exposure", Day: day(70), Exposure: poss}}
	_, _, frozen := Assess(on, "seed", p, recs, day(120))
	if got := frozen[3]; got.CycleStart != 68 || got.CycleEnd != 84 || got.Spec != changed {
		t.Fatalf("the middle exposure's cycle overlaps the later one: [%d,%d)", got.CycleStart, got.CycleEnd)
	}
}

// Turning conception off applies now, also in a cycle drawn when it was on.
func TestCanConceiveIsTheSettingNow(t *testing.T) {
	spec := DefaultSpec()
	first := Conceive(spec, "seed", "N1", 0, exposureOn(5))
	off := spec
	off.CanConceive = false
	p := Person{Who: "N1", Spec: off}
	recs := []Record{{ID: 9, Kind: "period_start", Day: day(0)},
		{ID: 1, Kind: "conception_exposure", Day: day(5), Exposure: poss, Result: &first},
		{ID: 2, Kind: "conception_exposure", Day: day(first.CycleEnd - 1), Exposure: poss}}
	_, _, frozen := Assess(on, "seed", p, recs, day(first.CycleEnd))
	if got := frozen[2]; got.Status != "not_applicable" {
		t.Fatalf("conception off now: %+v", got)
	}
}

// A stored cycle keeps its days when read under another chat's seed (an
// attached past) or for a person with stored cycles from two merged names.
func TestStoredCycleIsAFact(t *testing.T) {
	spec := DefaultSpec()
	past := Conceive(spec, "past-chat", "N2", 0, exposureOn(5))
	p := Person{Who: "N2", Spec: spec}
	const move = 17
	recs := []Record{{ID: 9, Kind: "period_start", Day: day(move)},
		{ID: 1, Kind: "conception_exposure", Day: day(5 + move), Exposure: poss, Result: &past},
		{ID: 2, Kind: "conception_exposure", Day: day(past.CycleEnd - 1 + move), Exposure: poss}}
	_, _, frozen := Assess(on, "current-chat", p, recs, day(past.CycleEnd+move))
	if got := frozen[2]; got.CycleStart != past.CycleStart+move || got.CycleEnd != past.CycleEnd+move {
		t.Fatalf("the past's cycle [%d,%d) became [%d,%d)", past.CycleStart+move, past.CycleEnd+move, got.CycleStart, got.CycleEnd)
	}
	// two names' stored cycles, merged: a day between them is drawn after
	// the first and ends where the second starts
	a := Conceive(spec, "seed", "N2", 0, exposureOn(5))
	b := Conceive(spec, "seed", "N1", 0, exposureOn(90))
	p = Person{Who: "N1", Spec: spec}
	recs = []Record{{ID: 9, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(90), Exposure: poss, Result: &b},
		{ID: 1, Kind: "conception_exposure", Day: day(5), Exposure: poss, Result: &a},
		{ID: 3, Kind: "conception_exposure", Day: day(a.CycleEnd), Exposure: poss}}
	_, _, frozen = Assess(on, "seed", p, recs, day(120))
	if got := frozen[3]; got.CycleStart != a.CycleEnd || got.CycleEnd > b.CycleStart {
		t.Fatalf("[%d,%d) between [%d,%d) and [%d,%d)", got.CycleStart, got.CycleEnd, a.CycleStart, a.CycleEnd, b.CycleStart, b.CycleEnd)
	}
}

// A short cycle whose next fertile window reaches back into it: the next
// cycle takes part in the result and is stored with it, and a stored cycle
// starting there is evaluated too.
func TestNextCycleWindowTakesPart(t *testing.T) {
	short := Spec{CycleDays: 18, VariationDays: 0, PeriodDays: 5, LutealMinDays: 16, LutealMaxDays: 16, Viability: 1, Peak: 1, GestationDays: 266, CanConceive: true, CycleEnabled: true}
	if !short.Valid() {
		t.Fatal("fixture: valid settings")
	}
	alone := Conceive(short, "b", "N1", 0, exposureOn(17))
	if alone.DayProbability == 0 || len(alone.Cycles) != 2 || alone.Cycles[1].Start != 18 {
		t.Fatalf("the next cycle's window takes part and is stored: %+v", alone)
	}
	// a stored cycle before it does not hide the next one's window
	first := Conceive(short, "b", "N1", 0, exposureOn(5))
	recs := []Record{{ID: 1, Kind: "conception_exposure", Day: day(5), Result: &first}}
	got := conceiveWith(short, "b", "N1", 0, storedCycles(recs, day(0), day(17)), exposureOn(17))
	if got.DayProbability != alone.DayProbability || got.Implanted != alone.Implanted || got.OvulationDay != alone.OvulationDay {
		t.Fatalf("a stored cycle changed the result: %+v vs %+v", got, alone)
	}
	// the next cycle stored with the result keeps its days after a change
	longer := short
	longer.CycleDays = 40
	recs = []Record{{ID: 1, Kind: "conception_exposure", Day: day(17), Result: &alone}}
	if got := conceiveWith(longer, "b", "N1", 0, storedCycles(recs, day(0), day(19)), exposureOn(19)); got.CycleStart != 18 || got.CycleEnd != 36 {
		t.Fatalf("the stored next cycle [18,36) became [%d,%d)", got.CycleStart, got.CycleEnd)
	}
}

// Overlapping stored cycles (two names merged): the one earlier in the
// story stands, whatever order the results were drawn in.
func TestOverlappingStoredCyclesFirstInStory(t *testing.T) {
	spec := DefaultSpec()
	a := Conceive(spec, "seed", "N2", 0, exposureOn(5))
	b := Conceive(spec, "seed", "N1", 0, exposureOn(6))
	if a.CycleEnd == b.CycleEnd {
		t.Skip("fixture: the two draws agree")
	}
	recs := []Record{{ID: 1, Kind: "conception_exposure", Day: day(5), Result: &a}, {ID: 2, Kind: "conception_exposure", Day: day(6), Result: &b}}
	apart := func(c []StoredCycle) bool {
		for i := 1; i < len(c); i++ {
			if c[i].Start < c[i-1].End {
				return false
			}
		}
		return true
	}
	if c := storedCycles(recs, day(0), day(10)); !apart(c) {
		t.Fatalf("overlapping cycles kept: %+v", c)
	}
	if c := storedCycles(recs, day(0), day(10)); c[0].End != a.CycleEnd {
		t.Fatalf("the earlier result's cycle stands: %+v", c)
	}
	recs[0], recs[1] = recs[1], recs[0]
	if c := storedCycles(recs, day(0), day(10)); c[0].End != b.CycleEnd {
		t.Fatalf("the earlier result's cycle stands: %+v", c)
	}
}

// A pregnancy that implanted in the cycle after the exposure's is read with
// that cycle's settings: an exposure of its fertile window adds its
// partner.
func TestPregnancyReadWithItsOvulationCycle(t *testing.T) {
	short := Spec{CycleDays: 18, VariationDays: 0, PeriodDays: 5, LutealMinDays: 16, LutealMaxDays: 16, Viability: 1, Peak: 0, GestationDays: 266, CanConceive: true, CycleEnabled: true}
	first := Conceive(short, "b", "N1", 0, exposureOn(5))
	now := short
	now.Peak = 1
	p := Person{Who: "N1", Spec: now}
	recs := []Record{{ID: 9, Kind: "period_start", Day: day(0)},
		{ID: 1, Kind: "conception_exposure", Day: day(5), Exposure: poss, Result: &first},
		{ID: 2, Kind: "conception_exposure", Day: day(17), Exposure: poss, Partners: []string{"P1"}},
		{ID: 3, Kind: "conception_exposure", Day: day(18), Exposure: poss, Partners: []string{"P2"}}}
	st, _, frozen := Assess(on, "b", p, recs, day(40))
	if x := frozen[2]; !x.Implanted || x.Spec.Peak != 0 || x.ovulationSpec().Peak != 1 {
		t.Fatalf("fixture: implanted in the next cycle, drawn under the stored cycle: %+v", x)
	}
	if pg := st.Pregnancy; pg == nil || pg.Paternity != "candidates" || !slices.Equal(pg.Candidates, []string{"P1", "P2"}) {
		t.Fatalf("both partners of the window: %+v", pg)
	}
}
