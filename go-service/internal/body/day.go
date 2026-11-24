package body

import (
	"regexp"
	"strconv"
	"strings"
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
	// a year of one to four digits, as a stored date may have (1.1: "923/01/02"),
	// read whole (not the end of a longer number)
	calendarDate = regexp.MustCompile(`(?:^|\D)(\d{1,4})[/.\-](\d{1,2})[/.\-](\d{1,2})`)
	// the critic writes "day 3" (memory/request.go), AC2's dates "Day 3"
	dayN = regexp.MustCompile(`(?i)\bday (\d+)\b`)
)

// Allow a century of story days, including leap years. Cycle draws walk
// from the reference day, so a model-supplied count must be finite in practice.
const maxStoryDays = 100 * 366

func storyDays(digits string) (int, bool) {
	n, err := strconv.Atoi(digits)
	return n, err == nil && n <= maxStoryDays
}

// DayOf reads a recorded date as AC2 writes it ("name = 2026/01/02",
// "Day 12"): the first calendar date in it, else its Day N, else unknown.
func DayOf(text string) Day {
	if m := calendarDate.FindStringSubmatch(text); m != nil {
		year := strings.Repeat("0", 4-len(m[1])) + m[1]
		if t, err := time.Parse("2006/1/2", year+"/"+m[2]+"/"+m[3]); err == nil {
			return Day{"calendar", int(t.Unix() / 86400)}
		}
	}
	if m := dayN.FindStringSubmatch(text); m != nil {
		if n, ok := storyDays(m[1]); ok {
			return Day{"day", n}
		}
	}
	return Day{}
}

var relativeDays = regexp.MustCompile(`(?i)\b(\d+)\s+days?\s+(before|earlier|ago|after|later)\b`)
var koreanRelativeDays = regexp.MustCompile(`\b(\d+)\s*일\s*(?:전|후)`)

// EventDay is the day of a body event the critic recorded in a turn, on the
// days Archive Center keeps (the story's dates must match the stored ones):
// the turn's stored day, or a calendar date the critic gave, or a number of
// days before or after the turn. A day number the story itself uses ("day
// 100") is not Archive Center's Day N, so it is not read.
func EventDay(when string, turn Day) Day {
	if m := calendarDate.FindStringSubmatch(when); m != nil {
		if d := DayOf(m[1] + "/" + m[2] + "/" + m[3]); d.Known() {
			return d
		}
	}
	if m := relativeDays.FindStringSubmatch(when); m != nil && turn.Known() {
		n, ok := storyDays(m[1])
		if !ok {
			return Day{}
		}
		switch m[2] = strings.ToLower(m[2]); m[2] {
		case "before", "earlier", "ago":
			return Day{turn.Axis, turn.N - n}
		default:
			return Day{turn.Axis, turn.N + n}
		}
	}
	// A Korean interval is outside the critic's date format. Preserve its
	// turn-day fallback unless the stated interval is itself implausible.
	if m := koreanRelativeDays.FindStringSubmatch(when); m != nil {
		if _, ok := storyDays(m[1]); !ok {
			return Day{}
		}
	}
	return turn
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
