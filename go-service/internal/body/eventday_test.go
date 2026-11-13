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
