package body

import "testing"

// Body tracking reads the dates Archive Center stores (1.1): a year of one
// to four digits, read whole, so a short-year chat's days compare; the end
// of a longer number is not read as a year.
func TestDayOfReadsStoredYears(t *testing.T) {
	short := DayOf("story = 923/01/02")
	next := DayOf("date = 923/01/03")
	if short.Axis != "calendar" || !short.Same(next) || next.N-short.N != 1 {
		t.Fatalf("short year: %+v %+v", short, next)
	}
	if d := DayOf("clock = 1423/05/15"); d.Axis != "calendar" || d.Text() != "1423/05/15" {
		t.Fatalf("four-digit year: %+v", d)
	}
	if d := DayOf("11423/05/15"); d.Known() {
		t.Fatalf("the end of a longer number read as a date: %+v", d)
	}
	if d := EventDay("923/01/05", short); d.Axis != "calendar" || d.N-short.N != 3 {
		t.Fatalf("an event's own short-year date: %+v", d)
	}
}
