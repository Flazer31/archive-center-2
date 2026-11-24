package body

import "testing"

// A body event's day is on the days Archive Center keeps: the turn's
// stored day, a calendar date the critic gave, or days before or after the
// turn; the story's own day number is not Archive Center's.
func TestEventDay(t *testing.T) {
	turn := day(30)
	cases := []struct {
		when string
		want Day
	}{
		{"", day(30)},
		{"day 100", day(30)},
		{"Day 100", day(30)},
		{"3 days before", day(27)},
		{"3 days ago", day(27)},
		{"2 days after", day(32)},
		{"1 day later", day(31)},
		{"2026/01/02", DayOf("2026/01/02")},
		{"the night before", day(30)},
	}
	for _, c := range cases {
		if got := EventDay(c.when, turn); got != c.want {
			t.Fatalf("%q: %+v, want %+v", c.when, got, c.want)
		}
	}
	// on a calendar turn the days move on the calendar
	cal := DayOf("2026/03/10")
	if got := EventDay("2 days before", cal); got != DayOf("2026/03/08") {
		t.Fatalf("calendar turn: %+v", got)
	}
	// a turn without a day: only a calendar date is known
	if got := EventDay("3 days before", Day{}); got.Known() {
		t.Fatalf("no turn day: %+v", got)
	}
}

func TestImplausibleStoryDays(t *testing.T) {
	turn := day(30)
	for _, digits := range []string{"1000000000", "9999999999999999999999999999"} {
		if got := DayOf("Day " + digits); got.Known() {
			t.Errorf("Day %s: %+v", digits, got)
		}
		for _, suffix := range []string{" days after", " days earlier", "일 후"} {
			if got := EventDay(digits+suffix, turn); got.Known() {
				t.Errorf("%s%s: %+v", digits, suffix, got)
			}
		}
	}
	if got := DayOf("Day 36500"); got != day(36500) {
		t.Errorf("century-long story: %+v", got)
	}
	if got := EventDay("36500 days before", turn); got != day(turn.N-36500) {
		t.Errorf("century-long interval: %+v", got)
	}
	if t.Failed() {
		return // do not run the cycle model with a known billion-day input
	}
	record := Record{ID: 1, Kind: "conception_exposure", Day: EventDay("1000000000 days after", turn)}
	st, _, frozen := Assess(on, "seed", Person{Who: "N1", Spec: DefaultSpec(), Ref: turn}, []Record{record}, turn)
	if len(st.Facts) != 1 || st.Facts[0].Day.Known() || len(frozen) != 0 {
		t.Fatalf("undated exposure should remain a fact without a cycle draw: %+v %v", st.Facts, frozen)
	}
}
