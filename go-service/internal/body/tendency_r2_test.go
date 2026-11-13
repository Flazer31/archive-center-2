package body

import (
	"strings"
	"testing"
)

// Before the next period and its first days are apart: the day the next
// period may have begun or not names both phases, each with its own words.
func TestPhaseAcrossThePeriodStart(t *testing.T) {
	for _, d := range []int{27, 29} {
		tn := phaseOn(t, d, &Intensity{2, 2, 2, 2, 2})
		if tn == nil || !strings.HasPrefix(tn.Phase, "menstrual or premenstrual (cycle day ") || !tn.Either ||
			len(tn.Words) != 2 || !strings.HasPrefix(tn.Words[0], "if menstrual: ") || !strings.HasPrefix(tn.Words[1], "if premenstrual: ") {
			t.Fatalf("Day %d: %+v", d, tn)
		}
		if l := TendencyLines([]State{{Who: "N1", Tendency: tn}}, func(id string) string { return id }); !strings.Contains(l[1], "; if premenstrual: ") {
			t.Fatalf("Day %d line: %v", d, l)
		}
	}
}

// The phase boundaries: ovulation expected on day CycleDays - mean luteal
// + 1 (16 by default) with a day either side, the last five days
// premenstrual, the period's days menstrual.
func TestPhaseBoundaries(t *testing.T) {
	s := DefaultSpec()
	for d, want := range map[int]string{5: "menstrual", 6: "follicular", 14: "follicular", 15: "ovulatory", 17: "ovulatory", 18: "luteal", 23: "luteal", 24: "premenstrual", 28: "premenstrual"} {
		if got, _ := cyclePhase(s, d); got != want {
			t.Fatalf("cycle day %d: %s, want %s", d, got, want)
		}
	}
}

// Pregnancy time boundaries in weeks since the start (days/7+1): very early
// to week 4, early to 12, mid to 27, late from 28 (as designed).
func TestPregnancyTimeBoundaries(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec(), Intensity: &Intensity{2, 2, 2, 2, 2}}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)}, {ID: 2, Kind: "pregnancy_confirmed", Day: day(10)}}
	for days, want := range map[int]string{27: "very early", 28: "early", 83: "early", 84: "mid", 188: "mid", 189: "late"} {
		st, _, _ := Assess(on, "seed", p, recs, day(10+days))
		if st.Tendency == nil || !strings.HasPrefix(st.Tendency.Phase, want+" pregnancy (") {
			t.Fatalf("%d days: %+v, want %s", days, st.Tendency, want)
		}
	}
	// a modeled pregnancy's period is late only after a mean luteal phase
	recs = []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Result: implanted(11, 12, 20, 278)}}
	mean := (p.Spec.LutealMinDays + p.Spec.LutealMaxDays) / 2
	for now, late := range map[int]bool{12 + mean: false, 12 + mean + 1: true} {
		st, _, _ := Assess(on, "seed", p, recs, day(now))
		if got := strings.Contains(strings.Join(st.Tendency.Words, ","), "late period"); got != late {
			t.Fatalf("Day %d late period %v: %+v", now, got, st.Tendency)
		}
	}
}

// An observed pregnancy's fact line names its confirmation, not a start.
func TestObservedLineSaysConfirmed(t *testing.T) {
	st := State{Who: "N1", Pregnancy: &Pregnancy{Observed: true, Stage: "observed", Since: day(40), BirthDay: day(306), Paternity: "unknown"}}
	if l := Lines([]State{st}, func(id string) string { return id }); len(l) != 1 || !strings.Contains(l[0], "pregnancy confirmed Day 40") || strings.Contains(l[0], "pregnant since") {
		t.Fatalf("line: %v", l)
	}
}

// The tendencies go with one turn in TendencyEvery, the same turn deciding
// the same (a reroll).
func TestTendencyTurns(t *testing.T) {
	var got []int
	for turn := 1; turn <= 7; turn++ {
		if TendencyTurn(turn) {
			got = append(got, turn)
		}
		if TendencyTurn(turn) != TendencyTurn(turn) {
			t.Fatal("not stable")
		}
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 4 || got[2] != 7 {
		t.Fatalf("turns with tendencies: %v", got)
	}
}

// A period recorded before the recovery began does not hide the
// recovery; one observed after it does.
func TestRecoveryBeforeEarlierPeriod(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec(), Intensity: &Intensity{2, 2, 2, 2, 2}}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)}, {ID: 2, Kind: "recovery_started", Day: day(20)}}
	if st, _, _ := Assess(on, "seed", p, recs, day(22)); st.Tendency == nil || !strings.HasPrefix(st.Tendency.Phase, "recovery after the pregnancy ended (day ") {
		t.Fatalf("an earlier period hid the recovery: %+v", st.Tendency)
	}
}

// Three possible phases are each given (allowing two only went
// unnoticed); late period stops at the end of the early weeks.
func TestThreePhasesAndLatePeriodEnd(t *testing.T) {
	found := false
	for d := 30; d < 90 && !found; d++ {
		if tn := phaseOn(t, d, nil); tn != nil && len(tn.Words) == 3 && tn.Either {
			found = strings.Count(tn.Phase, " or ") == 2
		}
	}
	if !found {
		t.Fatal("no day with three possible phases given")
	}
	p := Person{Who: "N1", Spec: DefaultSpec(), Intensity: &Intensity{2, 2, 2, 2, 2}}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Result: implanted(11, 12, 20, 278)}}
	for now, late := range map[int]bool{12 + 83: true, 12 + 84: false} {
		st, _, _ := Assess(on, "seed", p, recs, day(now))
		if got := strings.Contains(strings.Join(st.Tendency.Words, ","), "late period"); got != late {
			t.Fatalf("Day %d late period %v: %+v", now, got, st.Tendency)
		}
	}
}
