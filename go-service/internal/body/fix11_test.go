package body

import (
	"reflect"
	"slices"
	"testing"
)

// 1.1 body tracking fixes (AC1 parity). Model results are given frozen, as
// stored, so the tests do not depend on a draw.

var poss = ExposureInput{Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "none"}

func implanted(exposure, ovulation, implantation, birth int) *Exposure {
	return &Exposure{Version: ModelVersion, Status: "evaluated", Implanted: true, ExposureDay: exposure, OvulationDay: ovulation, ImplantationDay: implantation, BirthDay: birth}
}

// 1. A confirmed pregnancy has a term: the confirmation day as its latest
// start plus the gestation days; after it the birth is complete in the
// model only and the cycle waits for a new period start (AC1).
func TestConfirmedPregnancyTermElapses(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)}, {ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"N2"}}}
	birth := 40 + p.Spec.GestationDays
	st, _, _ := Assess(on, "seed", p, recs, day(birth-1))
	if st.Pregnancy == nil || st.Pregnancy.Stage != "observed" || st.Pregnancy.BirthDay != day(birth) || st.Cycle != nil {
		t.Fatalf("before the term: %+v", st.Pregnancy)
	}
	st, _, _ = Assess(on, "seed", p, recs, day(birth+10))
	if st.Pregnancy == nil || st.Pregnancy.Stage != "modeled_birth_completed" || st.Pregnancy.Paternity != "confirmed" || st.Cycle != nil {
		t.Fatalf("after the term: %+v cycle %+v", st.Pregnancy, st.Cycle)
	}
	recs = append(recs, Record{ID: 3, Kind: "period_start", Day: day(birth + 40)})
	if st, _, _ := Assess(on, "seed", p, recs, day(birth+45)); st.Cycle == nil || st.CycleRef != day(birth+40) {
		t.Fatalf("a period after the term restarts the cycle: %+v", st.Cycle)
	}
	// a confirmation while the model has a pregnancy under way takes its
	// dates and, with no parent named, its link (AC1 existing_modeled_conception)
	recs = []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"N2"}, Result: implanted(11, 12, 20, 278)},
		{ID: 3, Kind: "pregnancy_confirmed", Day: day(40)}}
	st, _, _ = Assess(on, "seed", p, recs, day(60))
	if pg := st.Pregnancy; pg == nil || !pg.Observed || pg.BirthDay != day(278) || pg.Paternity != "modeled_link" || !slices.Equal(pg.Candidates, []string{"N2"}) {
		t.Fatalf("confirmation of the model's pregnancy: %+v", pg)
	}
}

// 2. The model starts a new pregnancy after the earlier one's term; an
// exposure in the same cycle adds its partner as a candidate.
func TestModelSecondPregnancyAndCycleCandidates(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"N2"}, Result: implanted(11, 12, 20, 278)},
		{ID: 3, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"N3"}, Result: &Exposure{Version: ModelVersion, Status: "evaluated", ExposureDay: 11}},
		{ID: 4, Kind: "conception_exposure", Day: day(300), Exposure: poss, Partners: []string{"N4"}, Result: implanted(300, 302, 310, 568)}}
	st, _, _ := Assess(on, "seed", p, recs, day(100))
	if pg := st.Pregnancy; pg == nil || pg.Since != day(20) || pg.Paternity != "candidates" || !slices.Equal(pg.Candidates, []string{"N2", "N3"}) {
		t.Fatalf("one pregnancy, both partners of its cycle: %+v", pg)
	}
	st, _, _ = Assess(on, "seed", p, recs, day(320))
	if pg := st.Pregnancy; pg == nil || pg.Since != day(310) || pg.Stage != "modeled_implanted_pregnancy" || !slices.Equal(pg.Candidates, []string{"N4"}) {
		t.Fatalf("after the first term a new exposure is a new pregnancy: %+v", pg)
	}
}

// 3. An end applies where it is in the story: a recorded end dated after
// now does not end the pregnancy now, and an end before an exposure on the
// same day does not end the pregnancy that exposure starts.
func TestEndAppliesInStoryOrder(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Result: implanted(11, 12, 20, 278)},
		{ID: 3, Kind: "pregnancy_ended", User: true, Day: day(100)}}
	if st, _, _ := Assess(on, "seed", p, recs, day(22)); st.Pregnancy == nil {
		t.Fatal("an end dated after now ended the pregnancy now")
	}
	if st, _, _ := Assess(on, "seed", p, recs, day(120)); st.Pregnancy != nil {
		t.Fatalf("the end once reached: %+v", st.Pregnancy)
	}
	same := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 3, Kind: "pregnancy_ended", Day: day(11)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Result: implanted(11, 12, 20, 278)}}
	if st, _, _ := Assess(on, "seed", p, same, day(30)); st.Pregnancy == nil {
		t.Fatal("an end before the exposure in the story ended its pregnancy")
	}
}

// 4. An exposure in a cycle already evaluated is drawn with that cycle's
// settings, not the settings changed since (AC1 keeps each cycle's).
func TestCycleSettingsKeptForEvaluatedCycle(t *testing.T) {
	old := DefaultSpec()
	first := Conceive(old, "seed", "N1", 0, ExposureInput{Day: 5, Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "none"})
	if first.Status != "evaluated" || first.CycleEnd <= first.CycleStart || !first.Spec.Valid() {
		t.Fatalf("an evaluated result keeps its cycle and settings: %+v", first)
	}
	changed := old
	changed.CycleDays = 40
	recs := []Record{{ID: 1, Kind: "conception_exposure", Day: day(5), Result: &first}}
	if got := conceiveWith(changed, "seed", "N1", 0, storedCycles(recs, day(0), day(first.CycleEnd-1)), exposureOn(first.CycleEnd-1)); got.Spec != old || got.CycleStart != first.CycleStart || got.CycleEnd != first.CycleEnd {
		t.Fatalf("the same cycle took the changed settings: %+v", got)
	}
	// a later cycle takes the changed settings from the end of the evaluated
	// one on: the evaluated cycle keeps its days (AC1 timeline segments)
	if got := conceiveWith(changed, "seed", "N1", 0, storedCycles(recs, day(0), day(first.CycleEnd+1)), exposureOn(first.CycleEnd+1)); got.Spec != changed || got.CycleIndex != first.CycleIndex+1 || got.CycleStart != first.CycleEnd {
		t.Fatalf("a later cycle: %+v", got)
	}
	p := Person{Who: "N1", Spec: changed}
	recs = append([]Record{{ID: 9, Kind: "period_start", Day: day(0)}}, recs...)
	recs = append(recs, Record{ID: 2, Kind: "conception_exposure", Day: day(first.CycleEnd - 1), Exposure: poss},
		Record{ID: 3, Kind: "conception_exposure", Day: day(first.CycleEnd + 1), Exposure: poss})
	_, _, frozen := Assess(on, "seed", p, recs, day(first.CycleEnd+2))
	if got := frozen[2]; got.Spec != old {
		t.Fatalf("the new exposure in the evaluated cycle was drawn with %+v", got.Spec)
	}
	if got := frozen[3]; got.Spec != changed || got.CycleStart != first.CycleEnd || got.CycleIndex != first.CycleIndex+1 {
		t.Fatalf("the next cycle overlaps the evaluated one: %+v", got)
	}
	// unchanged settings draw the same as counting from the reference
	in := ExposureInput{Day: first.CycleEnd + 1, Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "none"}
	if again, whole := conceiveWith(old, "seed", "N1", 0, first.Cycles, in), Conceive(old, "seed", "N1", 0, in); !reflect.DeepEqual(again, whole) {
		t.Fatalf("continuing from stored cycles changed the draw: %+v vs %+v", again, whole)
	}
	// stored cycles evaluated again give the result drawn first
	if again := conceiveWith(old, "seed", "N1", 0, first.Cycles, exposureOn(5)); !reflect.DeepEqual(again, first) {
		t.Fatalf("the stored cycles give another result: %+v vs %+v", again, first)
	}
}

// 6. An end on another date axis keeps the cycle unknown only until a
// period start on now's axis follows it in the story.
func TestUnplacedEndClearedByLaterPeriod(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "pregnancy_ended", Day: Day{"calendar", 20000}},
		{ID: 2, Kind: "period_start", Day: day(50)}}
	if st, _, _ := Assess(on, "seed", p, recs, day(55)); st.Cycle == nil || st.CycleRef != day(50) {
		t.Fatalf("the period after the unplaced end restarts the cycle: %+v", st.Cycle)
	}
	before := []Record{{ID: 2, Kind: "period_start", Day: day(50)}, {ID: 1, Kind: "pregnancy_ended", Day: Day{"calendar", 20000}}}
	if st, _, _ := Assess(on, "seed", p, before, day(55)); st.Cycle != nil {
		t.Fatalf("a period before the unplaced end does not: %+v", st.Cycle)
	}
}

// 7. The critic writes days as "day 3" (memory/request.go); they are read.
func TestLowercaseDayRead(t *testing.T) {
	if d := DayOf("day 14"); d != day(14) {
		t.Fatalf("day 14 read as %+v", d)
	}
	if d := DayOf("Day 14"); d != day(14) {
		t.Fatalf("Day 14 read as %+v", d)
	}
}

// The delivered line of a confirmed pregnancy names the model's term, and
// after it says the term is reached without an observed birth.
func TestObservedPregnancyLineNamesTerm(t *testing.T) {
	name := func(id string) string { return id }
	st := State{Who: "N1", Pregnancy: &Pregnancy{Observed: true, Stage: "observed", Since: day(40), BirthDay: day(306), Paternity: "unknown"}}
	if l := Lines([]State{st}, name); len(l) != 1 || !contains(l[0], "modeled term ends Day 306") {
		t.Fatalf("line: %v", l)
	}
	st.Pregnancy.Stage = "modeled_birth_completed"
	if l := Lines([]State{st}, name); len(l) != 1 || !contains(l[0], "modeled term reached Day 306 (no birth observed") {
		t.Fatalf("line: %v", l)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
