package body

import (
	"fmt"
	"strings"
	"testing"
)

// The cycle shown is the one the pregnancy model draws (a range of every
// possible length told nothing, and the pregnancy model read other cycles
// than the ones shown): one cycle day, the period, the fertile window,
// ovulation and the next period of that cycle.
func TestCycleOnIsTheDrawnCycle(t *testing.T) {
	s := DefaultSpec()
	for _, seed := range []string{"a", "b", "c"} {
		var prev Cycle
		for now := 0; now <= 200; now++ {
			c := CycleOn(s, seed, "N1", 0, nil, now)
			// day by day: the cycle day goes on by one, or a new cycle begins
			// on the day the one before named as the next period, its length
			// within the settings
			if now > 0 {
				switch {
				case c.CycleDay.Min == prev.CycleDay.Min+1 && c.NextPeriod == prev.NextPeriod:
				case c.CycleDay.Min == 1 && now == prev.NextPeriod.Min:
					if l := prev.CycleDay.Min; l < s.CycleDays-s.VariationDays || l > s.CycleDays+s.VariationDays {
						t.Fatalf("seed %s: a cycle of %d days", seed, l)
					}
				default:
					t.Fatalf("seed %s Day %d: %+v after %+v", seed, now, c, prev)
				}
			}
			prev = c
			var in *StoredCycle
			cycles := drawCycles(s, seed, "N1", 0, nil, now)
			for i := range cycles {
				if cycles[i].Start <= now && now < cycles[i].End {
					in = &cycles[i]
				}
			}
			if in == nil || c.Status != "estimated" {
				t.Fatalf("seed %s Day %d: %+v", seed, now, c)
			}
			ov := max(in.Start, in.End-in.Luteal)
			window := Range{ov - 5, ov}
			if now > ov {
				var n *StoredCycle
				later := drawCycles(s, seed, "N1", 0, nil, in.End)
				for i := range later {
					if later[i].Start == in.End {
						n = &later[i]
					}
				}
				nov := max(n.Start, n.End-n.Luteal)
				window = Range{nov - 5, nov}
			}
			if c.CycleDay != (Range{now - in.Start + 1, now - in.Start + 1}) || c.NextPeriod != (Range{in.End, in.End}) ||
				c.Ovulation != (Range{ov, ov}) || c.Menstruation != (now-in.Start < s.PeriodDays) ||
				c.Fertile != window || c.FertilePossible != (now >= window.Min && now <= window.Max) {
				t.Fatalf("seed %s Day %d: %+v, cycle %+v", seed, now, c, in)
			}
		}
	}
}

// A pregnancy the model draws ovulates on the day the cycle shown names.
func TestCycleOnAgreesWithThePregnancyModel(t *testing.T) {
	s := DefaultSpec()
	checked := 0
	for i := 0; i < 400 && checked < 5; i++ {
		seed := fmt.Sprint("seed", i)
		for d := 10; d < 20; d++ {
			x := Conceive(s, seed, "N1", 0, ExposureInput{Day: d, Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "none"})
			if !x.Implanted {
				continue
			}
			if c := CycleOn(s, seed, "N1", 0, nil, x.OvulationDay); c.Ovulation.Min != x.OvulationDay {
				t.Fatalf("seed %s: pregnancy ovulation Day %d, cycle shown ovulation %+v", seed, x.OvulationDay, c.Ovulation)
			}
			checked++
			break
		}
	}
	if checked == 0 {
		t.Fatal("no implantation drawn to compare")
	}
}

// The tendency names the drawn cycle's phase; the line its one day.
func TestDrawnCycleLineAndPhase(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec()}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)}}
	for d := 0; d < 60; d++ {
		st, _, _ := Assess(on, "seed", p, recs, day(d))
		c := st.Cycle
		if c == nil || c.CycleDay.Min != c.CycleDay.Max {
			t.Fatalf("Day %d: %+v", d, c)
		}
		cd := c.CycleDay.Min
		if st.Tendency == nil || !strings.HasSuffix(st.Tendency.Phase, fmt.Sprintf("(cycle day %d)", cd)) {
			t.Fatalf("Day %d: %+v", d, st.Tendency)
		}
		if menstrual := strings.HasPrefix(st.Tendency.Phase, "menstrual"); menstrual != c.Menstruation {
			t.Fatalf("Day %d: phase %s, period %v", d, st.Tendency.Phase, c.Menstruation)
		}
		l := Lines([]State{st}, func(id string) string { return id })
		want := fmt.Sprintf("cycle day %d (", cd)
		next := fmt.Sprintf("next period Day %d; fertile window Day %d~Day %d, ovulation Day %d", c.NextPeriod.Min, c.Fertile.Min, c.Fertile.Max, c.Fertile.Max)
		if len(l) != 1 || !strings.Contains(l[0], want) || !strings.Contains(l[0], next) {
			t.Fatalf("Day %d line: %v", d, l)
		}
	}
}

// A short cycle whose next fertile window begins before it ends: the day
// the pregnancy model gives a chance shows as fertile.
func TestCycleOnNextWindow(t *testing.T) {
	short := Spec{CycleDays: 18, VariationDays: 0, PeriodDays: 5, LutealMinDays: 16, LutealMaxDays: 16, Viability: 1, Peak: 1, GestationDays: 266, CanConceive: true, CycleEnabled: true}
	for d := 0; d < 54; d++ {
		x := Conceive(short, "b", "N1", 0, exposureOn(d))
		c := CycleOn(short, "b", "N1", 0, nil, d)
		if (x.DayProbability > 0) != c.FertilePossible {
			t.Fatalf("Day %d: pregnancy chance %.3f, fertile shown %v (%+v)", d, x.DayProbability, c.FertilePossible, c)
		}
	}
}
