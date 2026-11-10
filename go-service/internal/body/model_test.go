package body

import "testing"

// The port's cycle estimate on the default model (28±2, period 5, luteal
// 10-16), derived from the formulas: day 0 is the first day of a period,
// day 5 is past the 5-day period, and the next period falls 26-30 days on.
func TestEstimateCycleDefault(t *testing.T) {
	s := DefaultSpec()
	c := EstimateCycle(s, 10, 10)
	if c.Status != "estimated" || c.CycleDay != (Range{1, 1}) || !c.Menstruation || c.FertilePossible {
		t.Fatalf("day 0: %+v", c)
	}
	if c.NextPeriod != (Range{36, 40}) || c.Ovulation != (Range{20, 30}) || c.Fertile != (Range{15, 30}) {
		t.Fatalf("day 0 ranges: %+v", c)
	}
	if c := EstimateCycle(s, 10, 15); c.MenstruationPossible || c.CycleDay != (Range{6, 6}) {
		t.Fatalf("day 5: %+v", c)
	}
	if c := EstimateCycle(s, 10, 22); !c.FertilePossible {
		t.Fatalf("day 12 is in the fertile envelope: %+v", c)
	}
	// later cycles widen: day 60 can be any of several cycle days
	if c := EstimateCycle(s, 0, 60); c.CycleDay.Min >= c.CycleDay.Max {
		t.Fatalf("day 60 has a range: %+v", c)
	}
	if c := EstimateCycle(s, 10, 9); c.Status != "unknown" || c.Reason != "story_date_or_reference_unresolved" {
		t.Fatalf("before the reference: %+v", c)
	}
	bad := s
	bad.LutealMaxDays = 40
	if c := EstimateCycle(bad, 0, 3); c.Status != "unknown" || c.Reason != "cycle_parameters_unresolved" {
		t.Fatalf("invalid settings: %+v", c)
	}
}

// AC1's rules for what the pregnancy model does not decide, and the same
// inputs always giving the same result.
func TestConceiveRules(t *testing.T) {
	s := DefaultSpec()
	yes := ExposureInput{Day: 14, Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "none"}
	cases := []struct {
		name   string
		spec   func(*Spec)
		in     func(*ExposureInput)
		seed   string
		status string
		reason string
	}{
		{"cannot conceive", func(s *Spec) { s.CanConceive = false }, nil, "x", "not_applicable", "explicit_exposure_or_world_setting"},
		{"not conceiving", nil, func(i *ExposureInput) { i.Classification = "not_potentially_conceiving" }, "x", "not_applicable", "explicit_exposure_or_world_setting"},
		{"incompatible", nil, func(i *ExposureInput) { i.Compatibility = "incompatible" }, "x", "not_applicable", "explicit_exposure_or_world_setting"},
		{"contraception", nil, func(i *ExposureInput) { i.Contraception = "present" }, "x", "unknown", "contraception_context_not_modeled"},
		{"unknown context", nil, func(i *ExposureInput) { i.Contraception = "unknown" }, "x", "unknown", "exposure_context_not_modeled"},
		{"no seed", nil, nil, "", "unknown", "simulation_seed_unavailable"},
		{"before the cycle start", nil, func(i *ExposureInput) { i.Day = -1 }, "x", "unknown", "exact_event_date_or_reference_unresolved"},
		{"author allowed unknown context", nil, func(i *ExposureInput) { i.Contraception, i.AuthorAllowed = "unknown", true }, "x", "evaluated", ""},
	}
	for _, c := range cases {
		sp, in := s, yes
		if c.spec != nil {
			c.spec(&sp)
		}
		if c.in != nil {
			c.in(&in)
		}
		if got := Conceive(sp, c.seed, "N1", 0, in); got.Status != c.status || got.Reason != c.reason {
			t.Fatalf("%s: %+v", c.name, got)
		}
	}
	// the same exposure is the same result; some seed implants within a month
	implanted := 0
	for _, seed := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		for d := 0; d < 30; d++ {
			in := yes
			in.Day = d
			x := Conceive(s, seed, "N1", 0, in)
			if again := Conceive(s, seed, "N1", 0, in); again != x {
				t.Fatalf("not deterministic: %+v %+v", x, again)
			}
			if x.Implanted {
				implanted++
				if x.ImplantationDay <= x.OvulationDay || x.BirthDay != x.OvulationDay+266 || d-x.OvulationDay < -5 || d-x.OvulationDay > 0 {
					t.Fatalf("implausible model: day %d %+v", d, x)
				}
			}
		}
	}
	if implanted == 0 {
		t.Fatal("no exposure ever implanted")
	}
}

func TestStage(t *testing.T) {
	x := Exposure{Implanted: true, OvulationDay: 10, ImplantationDay: 18, BirthDay: 276}
	for day, want := range map[int]string{9: "future_modeled_potential", 10: "modeled_pre_implantation", 18: "modeled_implanted_pregnancy", 276: "modeled_birth_completed"} {
		if got := x.Stage(day); got != want {
			t.Fatalf("day %d: %s want %s", day, got, want)
		}
	}
	if (Exposure{}).Stage(5) != "unknown" {
		t.Fatal("no pregnancy")
	}
}

func TestDayOf(t *testing.T) {
	for text, want := range map[string]string{"date = 2026/01/02": "2026/01/02", "body = 1423-3-5": "1423/03/05", "Day 12": "Day 12", "Day 0": "Day 0", "아침": "", "2026/02/30": ""} {
		if got := DayOf(text).Text(); got != want {
			t.Fatalf("%q: %q want %q", text, got, want)
		}
	}
	a, b := DayOf("2026/01/02"), DayOf("2026/01/30")
	if !a.Same(b) || b.N-a.N != 28 || a.Same(DayOf("Day 3")) {
		t.Fatalf("axes %+v %+v", a, b)
	}
}
