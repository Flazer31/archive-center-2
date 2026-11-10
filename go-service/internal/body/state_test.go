package body

import "testing"

func day(n int) Day { return Day{"day", n} }

var on = Toggles{Cycle: true, Pregnancy: true}

// A seen period start is the cycle's reference; flashbacks and plans never
// are; with none, the model's own start is chosen once from the seed.
func TestAssessCycleReference(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(10)}, {ID: 2, Kind: "period_start", Scope: "flashback", Day: day(20)}}
	st, newRef, _ := Assess(on, "seed", p, recs, day(15))
	if st.Cycle == nil || st.RefKind != "observed_period_start" || st.CycleRef != day(10) || st.Cycle.CycleDay != (Range{6, 6}) || newRef.Known() {
		t.Fatalf("observed reference: %+v %+v", st, newRef)
	}
	st, newRef, _ = Assess(on, "seed", p, nil, day(15))
	if st.RefKind != "model_initialization" || !newRef.Known() || newRef.N > 15 || newRef.N < 15-27 {
		t.Fatalf("model start: %+v %+v", st, newRef)
	}
	again, _, _ := Assess(on, "seed", Person{Who: "N1", Spec: DefaultSpec(), Ref: newRef}, nil, day(40))
	if again.CycleRef != newRef {
		t.Fatalf("a kept model start is reused: %+v", again)
	}
	if st, _, _ := Assess(Toggles{Pregnancy: true}, "seed", p, recs, day(15)); st.Cycle != nil {
		t.Fatal("cycle tracking off")
	}
}

// The story's pregnancy wins over the model; it ends with a stated end;
// the cycle is unknown during it and after it until a new period start.
func TestAssessObservedPregnancy(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"N2"}}}
	st, _, _ := Assess(on, "seed", p, recs, day(50))
	if st.Pregnancy == nil || !st.Pregnancy.Observed || st.Pregnancy.Paternity != "confirmed" || st.Cycle != nil {
		t.Fatalf("observed: %+v", st)
	}
	recs = append(recs, Record{ID: 3, Kind: "pregnancy_ended", Day: day(300)})
	if st, _, _ := Assess(on, "seed", p, recs, day(310)); st.Pregnancy != nil || st.Cycle != nil {
		t.Fatalf("after the end, before a period: %+v", st)
	}
	recs = append(recs, Record{ID: 4, Kind: "period_start", Day: day(320)})
	if st, _, _ := Assess(on, "seed", p, recs, day(325)); st.Cycle == nil || st.CycleRef != day(320) {
		t.Fatalf("a new period after it: %+v", st)
	}
	// confirmed without a named parent reads as unknown
	recs = []Record{{ID: 5, Kind: "pregnancy_confirmed", Day: day(5), Paternity: "confirmed"}}
	if st, _, _ := Assess(on, "seed", p, recs, day(6)); st.Pregnancy.Paternity != "unknown" {
		t.Fatalf("paternity without a candidate: %+v", st.Pregnancy)
	}
}

// Exposures are evaluated once (frozen) and a frozen result is reused; the
// model's pregnancy shows only from implantation, with its partner.
func TestAssessModeledPregnancy(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	in := ExposureInput{Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "none"}
	var seed string
	var day0 int
	var x Exposure
	for _, s := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		for d := 8; d < 20; d++ {
			e := in
			e.Day = d
			if got := Conceive(p.Spec, s, "N1", 0, e); got.Implanted {
				seed, day0, x = s, d, got
				break
			}
		}
		if seed != "" {
			break
		}
	}
	if seed == "" {
		t.Fatal("no implanting exposure to test with")
	}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)}, {ID: 2, Kind: "conception_exposure", Day: day(day0), Exposure: in, Partners: []string{"N2"}}}
	st, _, frozen := Assess(on, seed, p, recs, day(x.ImplantationDay+1))
	if got := frozen[2]; got != x {
		t.Fatalf("frozen %+v want %+v", got, x)
	}
	if st.Pregnancy == nil || st.Pregnancy.Observed || st.Pregnancy.Stage != "modeled_implanted_pregnancy" || st.Pregnancy.Paternity != "modeled_link" || st.Cycle != nil {
		t.Fatalf("modeled: %+v", st.Pregnancy)
	}
	if st, _, _ := Assess(on, seed, p, recs, day(x.OvulationDay)); st.Pregnancy != nil {
		t.Fatalf("before implantation the model is not a pregnancy: %+v", st.Pregnancy)
	}
	// a frozen result is not computed again, whatever the settings now
	recs[1].Result = &Exposure{Version: ModelVersion, Status: "evaluated"}
	if st, _, frozen := Assess(on, seed, p, recs, day(x.ImplantationDay+1)); len(frozen) != 0 || st.Pregnancy != nil {
		t.Fatalf("frozen result reused: %+v %+v", st.Pregnancy, frozen)
	}
	if _, _, frozen := Assess(Toggles{Cycle: true}, seed, p, recs[:1], day(30)); len(frozen) != 0 {
		t.Fatal("pregnancy model off")
	}
}
