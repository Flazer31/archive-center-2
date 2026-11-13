package body

import (
	"strings"
	"testing"
)

func birthLines(t *testing.T, recs []Record, now int) (State, string) {
	t.Helper()
	st, _, _ := Assess(on, "seed", Person{Who: "N1", Spec: DefaultSpec()}, recs, day(now))
	return st, strings.Join(Lines([]State{st}, func(id string) string { return id }), "\n")
}

// After a birth the mother and the father stay: the birth line names the
// mother, the child and the father as the pregnancy had him. A father the
// model gave is a secret known to no one, the mother included.
func TestBirthKeepsTheParents(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(11), Exposure: poss, Partners: []string{"NB"}, Result: implanted(11, 12, 20, 278)},
		{ID: 3, Kind: "pregnancy_ended", Day: day(278), Outcome: "birth", Child: "NC"}}
	before, _ := birthLines(t, recs[:2], 100)
	if before.Pregnancy == nil || strings.Join(before.Pregnancy.Candidates, ",") != "NB" {
		t.Fatalf("the pregnancy: %+v", before.Pregnancy)
	}
	st, lines := birthLines(t, recs, 280)
	if st.Pregnancy != nil || len(st.Births) != 1 {
		t.Fatalf("after the birth: pregnancy %+v births %+v", st.Pregnancy, st.Births)
	}
	b := st.Births[0]
	if b.Child != "NC" || strings.Join(b.Candidates, ",") != "NB" || !b.ModeledParent || b.Day != day(278) {
		t.Fatalf("birth %+v", b)
	}
	if !strings.Contains(lines, "gave birth Day 278; child NC; mother N1; father NB (modeled, a secret known to no one, N1 included") {
		t.Fatalf("lines:\n%s", lines)
	}
}

// A father the story establishes but keeps from the mother is her secret,
// in the pregnancy line and after the birth; a birth that names the father
// itself replaces what the pregnancy had.
func TestFatherTheMotherDoesNotKnow(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}, Visibility: "private"}}
	_, during := birthLines(t, recs, 60)
	if !strings.Contains(during, "parent established: NB (secret: N1 do not know who the father is)") {
		t.Fatalf("pregnancy line:\n%s", during)
	}
	recs = append(recs, Record{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC", Visibility: "public"})
	_, after := birthLines(t, recs, 301)
	if !strings.Contains(after, "[story fact, public]: gave birth Day 300; child NC; mother N1; father NB (secret: N1 do not know who the father is)") {
		t.Fatalf("birth line:\n%s", after)
	}
	// the birth names the father, openly
	named := append(recs[:2:2], Record{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Paternity: "confirmed", Candidates: []string{"ND"}})
	if _, l := birthLines(t, named, 301); !strings.Contains(l, "father ND") || strings.Contains(l, "secret") {
		t.Fatalf("a birth naming the father:\n%s", l)
	}
	// a loss is no birth
	lost := append(recs[:2:2], Record{ID: 3, Kind: "pregnancy_ended", Day: day(100), Outcome: "loss"})
	if st, l := birthLines(t, lost, 101); len(st.Births) != 0 || strings.Contains(l, "gave birth") {
		t.Fatalf("a loss: %+v\n%s", st.Births, l)
	}
}

// Every name a state uses is in its refs, for the names shown.
func TestStateRefsHaveTheBirthNames(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"NU"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC"}}
	st, _ := birthLines(t, recs, 301)
	refs := strings.Join(st.Refs(), ",")
	for _, id := range []string{"N1", "NB", "NU", "NC"} {
		if !strings.Contains(refs, id) {
			t.Fatalf("refs %s lack %s", refs, id)
		}
	}
}

// The same father named again keeps his secret; the secret ends only for
// the people the text shows learning it.
func TestFatherNamedAgainKeepsTheSecret(t *testing.T) {
	confirmed := Record{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1", "NX"}}
	again := Record{ID: 3, Kind: "pregnancy_confirmed", Day: day(50), Paternity: "confirmed", Candidates: []string{"NB"}}
	birth := Record{ID: 4, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Paternity: "confirmed", Candidates: []string{"NB"}}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)}, confirmed, again, birth}
	st, _ := birthLines(t, recs, 301)
	if b := st.Births[0]; strings.Join(b.Unaware, ",") != "N1,NX" {
		t.Fatalf("named again at the birth, the secret: %+v", b)
	}
	learned := append(append([]Record(nil), recs...), Record{ID: 5, Kind: "paternity_learned", Day: day(320), Learns: []string{"N1"}})
	st, l := birthLines(t, learned, 321)
	if b := st.Births[0]; strings.Join(b.Unaware, ",") != "NX" || !strings.Contains(l, "father NB (secret: NX do not know who the father is)") {
		t.Fatalf("after N1 learns: %+v\n%s", b, l)
	}
}

// Twins recorded one birth each: both children have the parents; a child
// named after the birth is joined to that birth, not made another.
func TestTwinsAndALaterName(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC"},
		{ID: 4, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth"},
		{ID: 5, Kind: "child_named", Day: day(303), Child: "ND"}}
	st, l := birthLines(t, recs, 310)
	if len(st.Births) != 2 {
		t.Fatalf("births %+v", st.Births)
	}
	for i, want := range []string{"NC", "ND"} {
		b := st.Births[i]
		if b.Child != want || strings.Join(b.Candidates, ",") != "NB" || strings.Join(b.Unaware, ",") != "N1" {
			t.Fatalf("birth %d: %+v", i, b)
		}
	}
	if strings.Count(l, "gave birth Day 300") != 2 || !strings.Contains(l, "child ND; mother N1; father NB (secret") {
		t.Fatalf("lines:\n%s", l)
	}
}

// Twins born either side of midnight are one pregnancy's children: the second
// has the parents too.
func TestTwinsAcrossMidnight(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC"},
		{ID: 4, Kind: "pregnancy_ended", Day: day(301), Outcome: "birth", Child: "ND"}}
	st, _ := birthLines(t, recs, 310)
	if len(st.Births) != 2 {
		t.Fatalf("births %+v", st.Births)
	}
	for i, b := range st.Births {
		if strings.Join(b.Candidates, ",") != "NB" || strings.Join(b.Unaware, ",") != "N1" {
			t.Fatalf("birth %d lost the parents: %+v", i, b)
		}
	}
}

// Learning the father of a child already born, during the next pregnancy,
// changes that child (and its twin), not the pregnancy under way.
func TestLearningAChildsFatherDuringTheNextPregnancy(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC"},
		{ID: 4, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "ND"},
		{ID: 5, Kind: "pregnancy_confirmed", Day: day(500), Paternity: "confirmed", Candidates: []string{"NE"}, Unaware: []string{"N1"}},
		{ID: 6, Kind: "paternity_learned", Day: day(520), Child: "NC", Learns: []string{"N1"}}}
	st, _ := birthLines(t, recs, 530)
	if st.Pregnancy == nil || strings.Join(st.Pregnancy.Unaware, ",") != "N1" || strings.Join(st.Pregnancy.Candidates, ",") != "NE" {
		t.Fatalf("the pregnancy under way changed: %+v", st.Pregnancy)
	}
	for i, b := range st.Births {
		if len(b.Unaware) != 0 || strings.Join(b.Candidates, ",") != "NB" {
			t.Fatalf("birth %d after N1 learns: %+v", i, b)
		}
	}
	// with no child named, the pregnancy under way
	recs[5].Child = ""
	st, _ = birthLines(t, recs, 530)
	if len(st.Pregnancy.Unaware) != 0 || strings.Join(st.Births[0].Unaware, ",") != "N1" {
		t.Fatalf("no child: pregnancy %+v births %+v", st.Pregnancy, st.Births)
	}
	// with no child and no pregnancy, every child of the latest birth
	twins := append(append([]Record(nil), recs[:4]...), Record{ID: 6, Kind: "paternity_learned", Day: day(320), Learns: []string{"N1"}})
	st, _ = birthLines(t, twins, 330)
	for i, b := range st.Births {
		if len(b.Unaware) != 0 {
			t.Fatalf("twin %d after N1 learns: %+v", i, b)
		}
	}
}

// A name goes to the unnamed birth of the day it gives; a child already
// joined to a birth is not joined again.
func TestNamingTheRightChild(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth"},
		{ID: 4, Kind: "pregnancy_confirmed", Day: day(500), Paternity: "confirmed", Candidates: []string{"NE"}},
		{ID: 5, Kind: "pregnancy_ended", Day: day(760), Outcome: "birth"},
		{ID: 6, Kind: "child_named", Day: day(300), Child: "NC"},
		{ID: 7, Kind: "child_named", Day: day(770), Child: "NC"}}
	st, _ := birthLines(t, recs, 780)
	if st.Births[0].Child != "NC" || st.Births[1].Child != "" {
		t.Fatalf("births %+v", st.Births)
	}
}

// Twins both unnamed: a name given for their birth day goes to the first.
func TestNamingTwinsInOrder(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth"},
		{ID: 4, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth"},
		{ID: 5, Kind: "child_named", Day: day(300), Child: "NC"},
		{ID: 6, Kind: "child_named", Day: day(300), Child: "ND"}}
	st, _ := birthLines(t, recs, 310)
	if st.Births[0].Child != "NC" || st.Births[1].Child != "ND" {
		t.Fatalf("births %+v", st.Births)
	}
}

// Learning the father of an older child changes that child, not the younger
// one born of another pregnancy.
func TestLearningAnOlderChildsFather(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC"},
		{ID: 4, Kind: "pregnancy_confirmed", Day: day(500), Paternity: "confirmed", Candidates: []string{"NE"}, Unaware: []string{"N1"}},
		{ID: 5, Kind: "pregnancy_ended", Day: day(760), Outcome: "birth", Child: "ND"},
		{ID: 6, Kind: "paternity_learned", Day: day(800), Child: "NC", Learns: []string{"N1"}}}
	st, _ := birthLines(t, recs, 810)
	if len(st.Births[0].Unaware) != 0 || strings.Join(st.Births[1].Unaware, ",") != "N1" {
		t.Fatalf("births %+v", st.Births)
	}
}

// A birth long after another, its pregnancy never recorded, is not a twin:
// it does not take the earlier child's father, and learning about it leaves
// the earlier child alone.
func TestALaterUnrecordedBirthIsNoTwin(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC"},
		{ID: 4, Kind: "pregnancy_ended", Day: day(1000), Outcome: "birth", Child: "ND"},
		{ID: 5, Kind: "paternity_learned", Day: day(1010), Paternity: "confirmed", Candidates: []string{"NE"}}}
	st, _ := birthLines(t, recs, 1020)
	if len(st.Births) != 2 || strings.Join(st.Births[0].Candidates, ",") != "NB" || strings.Join(st.Births[0].Unaware, ",") != "N1" {
		t.Fatalf("the first child: %+v", st.Births)
	}
	if strings.Join(st.Births[1].Candidates, ",") != "NE" || len(st.Births[1].Unaware) != 0 {
		t.Fatalf("the later child: %+v", st.Births[1])
	}
	st, _ = birthLines(t, recs[:4], 1020)
	if len(st.Births[1].Candidates) != 0 {
		t.Fatalf("the later child took the father: %+v", st.Births[1])
	}
}

// The name and the father told in one reply, the learning first: the
// learning waits for the child and the end is the same in either order.
func TestLearningBeforeTheName(t *testing.T) {
	base := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth"}}
	learned := Record{ID: 4, Kind: "paternity_learned", Day: day(320), Child: "NC", Learns: []string{"N1"}}
	named := Record{ID: 5, Kind: "child_named", Day: day(320), Child: "NC"}
	for _, order := range [][]Record{{learned, named}, {named, learned}} {
		st, _ := birthLines(t, append(append([]Record(nil), base...), order...), 330)
		if b := st.Births[0]; b.Child != "NC" || len(b.Unaware) != 0 || strings.Join(b.Candidates, ",") != "NB" {
			t.Fatalf("order %s first: %+v", order[0].Kind, b)
		}
	}
}

// A child named after the learnings about it: the learning reaches it in its
// place in the story, and a later correction of the twins' father stays.
func TestALateNameKeepsTheLaterCorrection(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth"},
		{ID: 4, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "ND"},
		{ID: 5, Kind: "paternity_learned", Day: day(310), Child: "NC", Learns: []string{"N1"}, Paternity: "confirmed", Candidates: []string{"NB"}},
		{ID: 6, Kind: "paternity_learned", Day: day(320), Child: "ND", Paternity: "confirmed", Candidates: []string{"NE"}, Unaware: []string{"NX"}},
		{ID: 7, Kind: "child_named", Day: day(300), Child: "NC"}}
	// story order: the naming's when is the birth day, read after the rest
	st, _ := birthLines(t, recs, 330)
	for i, b := range st.Births {
		if strings.Join(b.Candidates, ",") != "NE" || strings.Join(b.Unaware, ",") != "NX" {
			t.Fatalf("birth %d: %+v", i, b)
		}
	}
	if st.Births[0].Child != "NC" {
		t.Fatalf("the name: %+v", st.Births)
	}
}

// The name given after the learnings: the end is as with the name first, a
// later father with no secret leaves none.
func TestTheNameLateOrEarlyEndsTheSame(t *testing.T) {
	base := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth"},
		{ID: 4, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "ND"}}
	learned := Record{ID: 5, Kind: "paternity_learned", Day: day(310), Child: "NC", Learns: []string{"N1"}, Unaware: []string{"NX"}}
	fixed := Record{ID: 6, Kind: "paternity_learned", Day: day(320), Child: "ND", Paternity: "confirmed", Candidates: []string{"NE"}}
	name := Record{ID: 7, Kind: "child_named", Day: day(300), Child: "NC"}
	early, _ := birthLines(t, append(append([]Record(nil), base...), name, learned, fixed), 330)
	late, _ := birthLines(t, append(append([]Record(nil), base...), learned, fixed, name), 330)
	for i := range early.Births {
		e, l := early.Births[i], late.Births[i]
		if strings.Join(e.Candidates, ",") != "NE" || len(e.Unaware) != 0 || e.Child != l.Child ||
			strings.Join(l.Candidates, ",") != "NE" || len(l.Unaware) != 0 {
			t.Fatalf("birth %d: name first %+v, name last %+v", i, e, l)
		}
	}
}

// A learning that names the child before it is born changes the pregnancy
// it is born of, and so the child.
func TestLearningAboutAChildYetToBeBorn(t *testing.T) {
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "paternity_learned", Day: day(100), Child: "NC", Learns: []string{"N1"}},
		{ID: 4, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC"}}
	st, _ := birthLines(t, recs, 310)
	if b := st.Births[0]; len(b.Unaware) != 0 || strings.Join(b.Candidates, ",") != "NB" {
		t.Fatalf("birth %+v", b)
	}
	// a child no birth comes to have changes nothing
	recs[2].Child = "NZ"
	st, _ = birthLines(t, recs, 310)
	if b := st.Births[0]; strings.Join(b.Unaware, ",") != "N1" {
		t.Fatalf("an unknown child: %+v", b)
	}
}

// A learning about the second twin told between the two births reaches both
// twins, as when it is told after the second.
func TestLearningBetweenTheTwinsBirths(t *testing.T) {
	base := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "pregnancy_confirmed", Day: day(40), Paternity: "confirmed", Candidates: []string{"NB"}, Unaware: []string{"N1"}},
		{ID: 3, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "NC"}}
	learned := Record{ID: 4, Kind: "paternity_learned", Day: day(300), Child: "ND", Learns: []string{"N1"}, Paternity: "confirmed", Candidates: []string{"NE"}}
	second := Record{ID: 5, Kind: "pregnancy_ended", Day: day(300), Outcome: "birth", Child: "ND"}
	for _, order := range [][]Record{{learned, second}, {second, learned}} {
		st, _ := birthLines(t, append(append([]Record(nil), base...), order...), 310)
		for i, b := range st.Births {
			if strings.Join(b.Candidates, ",") != "NE" || len(b.Unaware) != 0 {
				t.Fatalf("%s first, birth %d: %+v", order[0].Kind, i, b)
			}
		}
	}
}
