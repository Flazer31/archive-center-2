package body

import (
	"slices"
	"strings"
	"testing"
)

// Fixes to the 1.1 body tracking change: earlier partners of the fertile window, the gap after a term, a
// confirmed pregnancy's kept term, and a parent link taken from the model.

// An exposure of the fertile window before the one that implanted adds its
// partner too, and an earlier one naming no partner keeps the parent open
// (AC1 bodyTrackingModelPaternity reads the whole record).
func TestEarlierWindowPartnersJoin(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	failed := &Exposure{Version: ModelVersion, Status: "evaluated", ExposureDay: 10}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(10), Exposure: poss, Partners: []string{"NA"}, Result: failed},
		{ID: 3, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"NB"}, Result: implanted(11, 12, 20, 278)}}
	st, _, _ := Assess(on, "seed", p, recs, day(30))
	if pg := st.Pregnancy; pg == nil || pg.Paternity != "candidates" || !slices.Equal(pg.Candidates, []string{"NA", "NB"}) {
		t.Fatalf("both partners of the window: %+v", pg)
	}
	recs[1].Partners = nil
	st, _, _ = Assess(on, "seed", p, recs, day(30))
	if pg := st.Pregnancy; pg == nil || pg.Paternity != "candidates" || !slices.Equal(pg.Candidates, []string{"NB"}) {
		t.Fatalf("an earlier exposure without a partner keeps the parent open: %+v", pg)
	}
	// an exposure before a stated end is not of the new pregnancy's window
	recs = []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(10), Exposure: poss, Partners: []string{"NA"}, Result: failed},
		{ID: 4, Kind: "pregnancy_ended", Day: day(10)},
		{ID: 3, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"NB"}, Result: implanted(11, 12, 20, 278)}}
	st, _, _ = Assess(on, "seed", p, recs, day(30))
	if pg := st.Pregnancy; pg == nil || pg.Paternity != "modeled_link" || !slices.Equal(pg.Candidates, []string{"NB"}) {
		t.Fatalf("an exposure before the end joined: %+v", pg)
	}
}

// After a confirmed pregnancy's term, a new modeled pregnancy before its
// implantation does not bring back the cycle counted from before the
// pregnancy (AC1 cycle_reference_after_pregnancy_unresolved).
func TestNoCycleFromBeforeAnElapsedTerm(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"N2"}},
		{ID: 3, Kind: "conception_exposure", Day: day(320), Exposure: poss, Result: implanted(320, 322, 330, 588)}}
	for _, d := range []int{320, 325, 329} {
		if st, _, _ := Assess(on, "seed", p, recs, day(d)); st.Cycle != nil {
			t.Fatalf("Day %d: a cycle from before the pregnancy: %+v", d, st.Cycle)
		}
	}
	recs = append(recs, Record{ID: 4, Kind: "pregnancy_ended", Day: day(340)}, Record{ID: 5, Kind: "period_start", Day: day(360)})
	if st, _, _ := Assess(on, "seed", p, recs, day(365)); st.Cycle == nil || st.CycleRef != day(360) {
		t.Fatalf("a period after the end restarts the cycle: %+v", st.Cycle)
	}
}

// A confirmed pregnancy's term is kept as first read: a later gestation
// setting does not move it (AC1 keeps modeled_birth_time).
func TestConfirmedTermKept(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)}, {ID: 2, Kind: "pregnancy_confirmed", Day: day(40)}}
	_, _, frozen := Assess(on, "seed", p, recs, day(50))
	kept, ok := frozen[2]
	if !ok || kept.Status != TermStatus || kept.Spec.GestationDays != p.Spec.GestationDays {
		t.Fatalf("the term is kept: %+v", frozen)
	}
	recs[1].Result = &kept
	p.Spec.GestationDays = 100
	st, _, again := Assess(on, "seed", p, recs, day(150))
	if pg := st.Pregnancy; pg == nil || pg.Stage != "observed" || pg.BirthDay != day(40+DefaultSpec().GestationDays) || len(again) != 0 {
		t.Fatalf("a settings change moved the kept term: %+v %v", pg, again)
	}
}

// A confirmed pregnancy that took its parents from the model says they are
// the model's; a parent the story states replaces them.
func TestModeledParentStaysModeled(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"N2"}, Result: implanted(11, 12, 20, 278)},
		{ID: 3, Kind: "conception_exposure", Day: day(12), Exposure: poss, Partners: []string{"N3"}, Result: &Exposure{Version: ModelVersion, Status: "evaluated", ExposureDay: 12}},
		{ID: 4, Kind: "pregnancy_confirmed", Day: day(40), Visibility: "public"}}
	name := func(id string) string { return id }
	st, _, _ := Assess(on, "seed", p, recs, day(60))
	l := Lines([]State{st}, name)
	if len(l) == 0 || !st.Pregnancy.ModeledParent || !strings.Contains(l[0], "modeled parent candidates: N2, N3 (modeled, not known to anyone") || strings.Contains(l[0], "; parent candidates") {
		t.Fatalf("the model's candidates under a story fact: %v", l)
	}
	recs = append(recs, Record{ID: 5, Kind: "pregnancy_confirmed", Day: day(50), Paternity: "confirmed", Candidates: []string{"N3"}})
	st, _, _ = Assess(on, "seed", p, recs, day(60))
	if l := Lines([]State{st}, name); st.Pregnancy.ModeledParent || !strings.Contains(l[0], "parent established: N3") {
		t.Fatalf("a stated parent: %v", l)
	}
}
