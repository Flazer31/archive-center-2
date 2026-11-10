package body

import (
	"regexp"
	"strconv"
	"time"
)

// Day is a story day on one axis: a calendar date counted in days, or the
// chat's Day N when no date is known. Days on different axes are not
// compared (the model then says unknown).
type Day struct {
	Axis string // "calendar" or "day"; "" when unknown
	N    int
}

// Known reports whether the day is on an axis.
func (d Day) Known() bool { return d.Axis != "" }

// Same reports whether two days can be compared.
func (d Day) Same(o Day) bool { return d.Known() && d.Axis == o.Axis }

var (
	calendarDate = regexp.MustCompile(`(\d{4})[/.\-](\d{1,2})[/.\-](\d{1,2})`)
	dayN         = regexp.MustCompile(`\bDay (\d+)\b`)
)

// DayOf reads a recorded date as AC2 writes it ("name = 2026/01/02",
// "Day 12"): the first calendar date in it, else its Day N, else unknown.
func DayOf(text string) Day {
	if m := calendarDate.FindStringSubmatch(text); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
		if t.Year() == y && int(t.Month()) == mo && t.Day() == d {
			return Day{"calendar", int(t.Unix() / 86400)}
		}
	}
	if m := dayN.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		return Day{"day", n}
	}
	return Day{}
}

// Text writes a day back on its axis.
func (d Day) Text() string {
	switch d.Axis {
	case "calendar":
		return time.Unix(int64(d.N)*86400, 0).UTC().Format("2006/01/02")
	case "day":
		return "Day " + strconv.Itoa(d.N)
	}
	return ""
}
