package body

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// Contraception by method: the day's chance is scaled by the
// method's factor, and a method never adds a pregnancy the same
// exposure without it would not have had.
func TestContraceptionScalesTheChance(t *testing.T) {
	s := DefaultSpec()
	for method, f := range map[string]float64{"condom": 0.15, "pill": 0.08, "withdrawal": 0.25, "iud": 0.01, "implant": 0.001, "injection": 0.05, "patch": 0.08, "magic": 0, "other": 0.15} {
		without, with := 0, 0
		for i := 0; i < 300; i++ {
			seed := fmt.Sprint("seed", i)
			for d := 8; d < 20; d++ {
				none := Conceive(s, seed, "N1", 0, exposureOn(d))
				in := exposureOn(d)
				in.Contraception, in.Method = "present", method
				x := Conceive(s, seed, "N1", 0, in)
				if x.Status != "evaluated" || x.Contraception != method || x.ContraceptionFactor != f ||
					math.Abs(x.DayProbability-none.DayProbability*f) > 1e-12 {
					t.Fatalf("%s seed %s Day %d: %+v (without %.4f)", method, seed, d, x, none.DayProbability)
				}
				if x.Implanted && !none.Implanted {
					t.Fatalf("%s seed %s Day %d: a pregnancy only with contraception", method, seed, d)
				}
				if none.Implanted {
					without++
				}
				if x.Implanted {
					with++
				}
			}
		}
		if without == 0 || with >= without {
			t.Fatalf("%s: %d pregnancies with, %d without", method, with, without)
		}
		// the share kept is about the factor (300 seeds × 12 days drawn)
		if r := float64(with) / float64(without); r > f*2.5 || (f >= 0.15 && r < f/2.5) {
			t.Fatalf("%s: kept %.3f of the pregnancies, factor %.2f", method, r, f)
		}
	}
}

// Emergency contraception stops a drawn pregnancy only within its days after
// the exposure and before the ovulation, about EmergencyBlock of the time;
// the same inputs give the same answer.
func TestEmergencyContraception(t *testing.T) {
	s := DefaultSpec()
	stopped, tried := 0, 0
	for i := 0; i < 2000 && tried < 300; i++ {
		seed := fmt.Sprint("seed", i)
		for d := 8; d < 18; d++ {
			x := Conceive(s, seed, "N1", 0, exposureOn(d))
			if !x.Implanted {
				continue
			}
			if x.EmergencyStops(d-1) || x.EmergencyStops(x.OvulationDay) || x.EmergencyStops(d+EmergencyDays+1) {
				t.Fatalf("seed %s: stopped before the exposure, on the ovulation or past its days: %+v", seed, x)
			}
			if x.OvulationDay > d {
				tried++
				got := x.EmergencyStops(d)
				// the draw is the result's: the same answer read again, and
				// read in another chat or under a merged name
				y := Conceive(s, seed, "N1", 0, exposureOn(d))
				if got != y.EmergencyStops(d) || x.EmergencyDraw == nil {
					t.Fatal("not the same answer")
				}
				if got {
					stopped++
				}
			}
			break
		}
	}
	if tried < 100 {
		t.Fatalf("only %d pregnancies with emergency contraception in time", tried)
	}
	if r := float64(stopped) / float64(tried); math.Abs(r-EmergencyBlock) > 0.08 {
		t.Fatalf("stopped %.3f of %d, want about %.2f", r, tried, EmergencyBlock)
	}
}

// Read with the result: a pregnancy emergency contraception stops is not
// read; the fact removed, it is read again.
func TestEmergencyContraceptionInTheState(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	for i := 0; i < 2000; i++ {
		seed := fmt.Sprint("seed", i)
		for d := 8; d < 18; d++ {
			x := Conceive(p.Spec, seed, "N1", 0, exposureOn(d))
			if !x.Implanted || x.OvulationDay <= d || !x.EmergencyStops(d+1) || d+1 >= x.OvulationDay {
				continue
			}
			recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
				{ID: 2, Kind: "conception_exposure", Day: day(d), Exposure: poss, Result: &x}}
			now := day(x.ImplantationDay + 5)
			if st, _, _ := Assess(on, seed, p, recs, now); st.Pregnancy == nil {
				t.Fatalf("seed %s: the drawn pregnancy is not read", seed)
			}
			withEC := append(append([]Record(nil), recs...), Record{ID: 3, Kind: "emergency_contraception", Day: day(d + 1)})
			if st, _, _ := Assess(on, seed, p, withEC, now); st.Pregnancy != nil {
				t.Fatalf("seed %s: emergency contraception the day after did not stop it: %+v", seed, st.Pregnancy)
			}
			// read by another chat (another seed) or under another name: the same
			if st, _, _ := Assess(on, "other seed", Person{Who: "N9", Spec: p.Spec}, withEC, now); st.Pregnancy != nil {
				t.Fatalf("seed %s: read elsewhere, the stop changed", seed)
			}
			// the fact removed, the pregnancy is read again
			if st, _, _ := Assess(on, seed, p, recs, now); st.Pregnancy == nil {
				t.Fatalf("seed %s: removing emergency contraception did not bring the pregnancy back", seed)
			}
			// a later exposure skipped for the pregnancy under way is read
			// again once the pregnancy is stopped: never frozen
			later := Record{ID: 4, Kind: "conception_exposure", Day: day(x.OvulationDay), Exposure: poss}
			st, _, frozen := Assess(on, seed, p, append(append([]Record(nil), recs...), later), now)
			if _, kept := frozen[4]; kept || st.Facts[2].Result == nil || st.Facts[2].Result.Reason != "modeled_pregnancy_ongoing" {
				t.Fatalf("seed %s: the later exposure under the pregnancy: frozen %v %+v", seed, kept, st.Facts[2].Result)
			}
			stored := later
			stored.Result = st.Facts[2].Result // as a store before 1.1 kept it
			st, _, frozen = Assess(on, seed, p, append(append([]Record(nil), withEC...), stored), now)
			var again *Exposure
			for _, f := range st.Facts {
				if f.ID == 4 {
					again = f.Result
				}
			}
			if again == nil || again.Reason == "modeled_pregnancy_ongoing" || again.Status != "evaluated" {
				t.Fatalf("seed %s: the later exposure after the stop: %+v", seed, again)
			}
			if _, kept := frozen[4]; !kept {
				t.Fatalf("seed %s: the later exposure drawn after the stop is kept", seed)
			}
			return
		}
	}
	t.Fatal("no pregnancy to stop found")
}

// Contraception the text names without its method is drawn as a
// condom: the same result as a condom named.
func TestContraceptionWithoutMethodIsACondom(t *testing.T) {
	s := DefaultSpec()
	for i := 0; i < 50; i++ {
		seed := fmt.Sprint("seed", i)
		for d := 8; d < 20; d++ {
			said := exposureOn(d)
			said.Contraception = "present"
			condom := said
			condom.Method = "condom"
			x, y := Conceive(s, seed, "N1", 0, said), Conceive(s, seed, "N1", 0, condom)
			if x.Status != "evaluated" || x.Contraception != "condom" || x.ContraceptionFactor != 0.15 || x.Implanted != y.Implanted || x.DayProbability != y.DayProbability {
				t.Fatalf("seed %s Day %d: %+v, a condom named: %+v", seed, d, x, y)
			}
		}
	}
}

// Every contraception the story names is drawn: magic never conceives,
// and a method the list does not have is other.
func TestEveryMethodIsDrawn(t *testing.T) {
	s := DefaultSpec()
	for i := 0; i < 300; i++ {
		seed := fmt.Sprint("seed", i)
		for d := 8; d < 20; d++ {
			in := exposureOn(d)
			in.Contraception, in.Method = "present", "magic"
			if x := Conceive(s, seed, "N1", 0, in); x.Status != "evaluated" || x.Implanted || x.DayProbability != 0 || x.Contraception != "magic" {
				t.Fatalf("magic seed %s Day %d: %+v", seed, d, x)
			}
			in.Method = "a herb no one names"
			other := in
			other.Method = "other"
			x, y := Conceive(s, seed, "N1", 0, in), Conceive(s, seed, "N1", 0, other)
			if x.Status != "evaluated" || x.Contraception != "other" || x.Implanted != y.Implanted || x.DayProbability != y.DayProbability {
				t.Fatalf("unlisted seed %s Day %d: %+v, other %+v", seed, d, x, y)
			}
		}
	}
}

// An exposure magic stops is no parent of a pregnancy another exposure of
// the same fertile window drew, before it or after it.
func TestMagicIsNoParent(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	magic := ExposureInput{Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "present", Method: "magic"}
	for name, recs := range map[string][]Record{
		"magic before": {{ID: 1, Kind: "period_start", Day: day(0)},
			{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: magic, Partners: []string{"NA"}},
			{ID: 3, Kind: "conception_exposure", Day: day(12), Exposure: poss, Partners: []string{"NB"}, Result: implanted(12, 12, 20, 278)}},
		"magic after": {{ID: 1, Kind: "period_start", Day: day(0)},
			{ID: 3, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"NB"}, Result: implanted(11, 12, 20, 278)},
			{ID: 2, Kind: "conception_exposure", Day: day(12), Exposure: magic, Partners: []string{"NA"}}},
	} {
		st, _, _ := Assess(on, "seed", p, recs, day(50))
		if pg := st.Pregnancy; pg == nil || strings.Join(pg.Candidates, ",") != "NB" {
			t.Fatalf("%s: %+v", name, st.Pregnancy)
		}
	}
	// a condom is still a possible parent: it lowers the chance, never to none
	condom := magic
	condom.Method = "condom"
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: condom, Partners: []string{"NA"}},
		{ID: 3, Kind: "conception_exposure", Day: day(12), Exposure: poss, Partners: []string{"NB"}, Result: implanted(12, 12, 20, 278)}}
	if st, _, _ := Assess(on, "seed", p, recs, day(50)); st.Pregnancy == nil || strings.Join(st.Pregnancy.Candidates, ",") != "NA,NB" {
		t.Fatalf("condom: %+v", st.Pregnancy)
	}
}

// The parents of a pregnancy are the exposures of its fertile window that
// could conceive: one whose kind or partner the text leaves unsaid is
// one. One the story says cannot conceive is not drawn.
func TestUnsaidExposureIsAParent(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	unsaid := ExposureInput{Classification: "unknown", Compatibility: "unknown", Contraception: "unknown"}
	never := ExposureInput{Classification: "potentially_conceiving", Compatibility: "incompatible", Contraception: "none"}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(10), Exposure: unsaid, Partners: []string{"NA"}},
		{ID: 3, Kind: "conception_exposure", Day: day(11), Exposure: never, Partners: []string{"NC"}},
		{ID: 4, Kind: "conception_exposure", Day: day(12), Exposure: poss, Partners: []string{"NB"}, Result: implanted(12, 12, 20, 278)}}
	st, _, frozen := Assess(on, "seed", p, recs, day(50))
	if pg := st.Pregnancy; pg == nil || !strings.HasPrefix(strings.Join(pg.Candidates, ","), "NA,") || !strings.HasSuffix(strings.Join(pg.Candidates, ","), "NB") {
		t.Fatalf("parents: %+v", st.Pregnancy)
	}
	if x := frozen[2]; x.Status != "evaluated" {
		t.Fatalf("an exposure whose kind and partner are unsaid is drawn: %+v", x)
	}
	if x := frozen[3]; x.Status != "not_applicable" {
		t.Fatalf("one the story says cannot conceive is not: %+v", x)
	}
}

// A result kept by the rules before 1.1 without a draw (a partner, a kind
// or contraception left unsaid) is read again and kept as drawn then; a
// drawn result is kept as it is.
func TestOldSkipsAreDrawnAgain(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	for _, reason := range []string{"exposure_context_not_modeled", "contraception_context_not_modeled"} {
		old := Exposure{Version: ModelVersion, Status: "unknown", Reason: reason, ExposureDay: 12}
		drawn := Conceive(p.Spec, "seed", "N1", 0, exposureOn(13))
		recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
			{ID: 2, Kind: "conception_exposure", Day: day(12), Exposure: ExposureInput{Classification: "unknown", Compatibility: "unknown", Contraception: "unknown"}, Result: &old},
			{ID: 3, Kind: "conception_exposure", Day: day(13), Exposure: poss, Result: &drawn}}
		_, _, frozen := Assess(on, "seed", p, recs, day(20))
		x, kept := frozen[2]
		if !kept || x.Status != "evaluated" || x.Reason != "" {
			t.Fatalf("%s: read again %v %+v", reason, kept, x)
		}
		if _, again := frozen[3]; again {
			t.Fatalf("%s: a drawn result was drawn again", reason)
		}
	}
}
