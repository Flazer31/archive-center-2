package body

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// Recovery after a pregnancy ends (1.1): it follows the end by itself, by
// days and outcome, stretched by her recovery speed; after a loss no
// feeling is given, the story decides.

func endAt(day0 int, outcome string, speed int, now int) *Tendency {
	p := Person{Who: "N1", Spec: DefaultSpec(), Intensity: &Intensity{2, 2, 2, 2, speed}}
	recs := []Record{{ID: 1, Kind: "pregnancy_confirmed", Day: day(day0 - 200)}, {ID: 2, Kind: "pregnancy_ended", Day: day(day0), Outcome: outcome}}
	st, _, _ := Assess(on, "seed", p, recs, day(now))
	return st.Tendency
}

func TestRecoveryAfterBirthByDays(t *testing.T) {
	for now, want := range map[int]string{300: "bleeding", 313: "bleeding", 314: "still healing", 341: "still healing", 342: "strength coming back slowly", 383: "strength coming back slowly"} {
		tn := endAt(300, "birth", 2, now)
		if tn == nil || !strings.HasPrefix(tn.Phase, fmt.Sprintf("after the birth (day %d)", now-300+1)) || !strings.Contains(strings.Join(tn.Words, ","), want) {
			t.Fatalf("day %d: %+v, want %s", now-300, tn, want)
		}
	}
	if tn := endAt(300, "birth", 2, 384); tn != nil {
		t.Fatalf("twelve weeks on, still recovering: %+v", tn)
	}
}

// Her speed stretches the stages: slow still recovers when usual is over,
// fast is over sooner.
func TestRecoverySpeed(t *testing.T) {
	if tn := endAt(300, "birth", 3, 384); tn == nil || !strings.Contains(strings.Join(tn.Words, ","), "strength coming back") {
		t.Fatalf("slow at twelve weeks: %+v", tn)
	}
	if tn := endAt(300, "birth", 1, 360); tn != nil {
		t.Fatalf("fast at day 60: %+v", tn)
	}
	if tn := endAt(300, "birth", 1, 330); tn == nil || !strings.Contains(strings.Join(tn.Words, ","), "strength coming back") {
		t.Fatalf("fast at day 30: %+v", tn)
	}
}

// After a loss: the body's few weeks, and the feelings left to the story.
func TestRecoveryAfterLoss(t *testing.T) {
	tn := endAt(300, "loss", 2, 301)
	if tn == nil || !strings.HasPrefix(tn.Phase, "after the pregnancy loss (day 2)") || tn.Note != lossNote {
		t.Fatalf("loss: %+v", tn)
	}
	l := TendencyLines([]State{{Who: "N1", Tendency: tn}}, func(id string) string { return id })
	if len(l) != 2 || !strings.Contains(l[1], "("+lossNote+")") {
		t.Fatalf("line: %v", l)
	}
	for _, w := range []string{"grief", "sad", "relief", "guilt"} {
		if strings.Contains(strings.ToLower(l[1]), w) {
			t.Fatalf("a feeling given after a loss: %s", l[1])
		}
	}
	if tn := endAt(300, "loss", 2, 328); tn != nil {
		t.Fatalf("four weeks on: %+v", tn)
	}
}

// An end the story did not say the outcome of: the neutral recovery, six
// weeks; a recovery the story ended stops it.
func TestRecoveryUnknownAndEnded(t *testing.T) {
	if tn := endAt(300, "", 2, 310); tn == nil || !strings.HasPrefix(tn.Phase, "recovery after the pregnancy ended (day 11)") {
		t.Fatalf("unknown: %+v", tn)
	}
	if tn := endAt(300, "", 2, 342); tn != nil {
		t.Fatalf("unknown six weeks on: %+v", tn)
	}
	p := Person{Who: "N1", Spec: DefaultSpec(), Intensity: &Intensity{2, 2, 2, 2, 2}}
	recs := []Record{{ID: 2, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth"}, {ID: 3, Kind: "recovery_ended", Day: day(320)}}
	if st, _, _ := Assess(on, "seed", p, recs, day(325)); st.Tendency != nil {
		t.Fatalf("the story ended the recovery: %+v", st.Tendency)
	}
}

func TestRecoverySpeedDraw(t *testing.T) {
	const n = 8000
	var c [4]int
	for i := 0; i < n; i++ {
		c[DrawIntensity("seed", fmt.Sprintf("N%d", i)).Recovery]++
	}
	if c[0] != 0 {
		t.Fatalf("a speed not drawn: %v", c)
	}
	for i, want := range recoveryOdds {
		if got := 100 * float64(c[i+1]) / n; math.Abs(got-float64(want)) > 2 {
			t.Fatalf("speed %d: %.1f%%, table %d%%", i+1, got, want)
		}
	}
}

// The recovery follows the story's order, not only the dates: a recovery
// end closes it whatever its date; the outcome of the end it follows
// stays with it across date axes; a new pregnancy closes it even when
// its days cannot be compared.
func TestRecoveryInStoryOrder(t *testing.T) {
	p := Person{Who: "N1", Spec: DefaultSpec(), Intensity: &Intensity{2, 2, 2, 2, 2}}
	cal := Day{"calendar", 20000}
	for _, end := range []Day{cal, {}} {
		recs := []Record{{ID: 1, Kind: "pregnancy_ended", Day: day(10), Outcome: "birth"}, {ID: 2, Kind: "recovery_ended", Day: end}}
		if st, _, _ := Assess(on, "seed", p, recs, day(12)); st.Tendency != nil {
			t.Fatalf("recovery end on %+v ignored: %+v", end, st.Tendency)
		}
	}
	recs := []Record{{ID: 1, Kind: "pregnancy_ended", Day: cal, Outcome: "loss"}, {ID: 2, Kind: "recovery_started", Day: day(10)}}
	st, _, _ := Assess(on, "seed", p, recs, day(12))
	if st.Tendency == nil || st.Tendency.Note != lossNote || !strings.HasPrefix(st.Tendency.Phase, "after the pregnancy loss (day 3)") || strings.Contains(strings.Join(st.Tendency.Words, ","), "mood") {
		t.Fatalf("the loss across axes: %+v", st.Tendency)
	}
	recs = []Record{{ID: 1, Kind: "pregnancy_ended", Day: day(10), Outcome: "birth"}, {ID: 2, Kind: "pregnancy_confirmed", Day: cal}}
	if st, _, _ := Assess(on, "seed", p, recs, day(12)); st.Tendency != nil && strings.HasPrefix(st.Tendency.Phase, "after the birth") {
		t.Fatalf("a new pregnancy kept the earlier birth's recovery: %+v", st.Tendency)
	}
}
