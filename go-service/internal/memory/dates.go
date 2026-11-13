package memory

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Story dates a chat may write (dates the backend did not read, "1423년 5월
// 15일", are read too): year, month and day as numbers ("1423-5-15",
// "1423/05/15", "1423.5.15") or in Korean ("1423년 5월 15일", a weekday in
// brackets after it), with an optional time (HH:MM).
const (
	numericDate = `(\d{1,4})[-/.](\d{1,2})[-/.](\d{1,2})(?:[ T](\d{1,2}):(\d{2})(?::\d{2})?)?`
	koreanDate  = `(\d{1,4})\s*년\s*(\d{1,2})\s*월\s*(\d{1,2})\s*일(?:\s*\(\s*[월화수목금토일]\s*\))?(?:\s*(\d{1,2}):(\d{2}))?`
)

var (
	wholeNumericDate = regexp.MustCompile(`^` + numericDate + `$`)
	wholeKoreanDate  = regexp.MustCompile(`^` + koreanDate + `$`)
	anyKoreanDate    = regexp.MustCompile(`(?:^|\D)` + koreanDate) // the year read whole
)

// normal writes a matched date as year/mm/dd (and HH:MM); false when the
// day does not exist.
func normal(m []string) (string, bool) {
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		return "", false
	}
	s := fmt.Sprintf("%s/%02d/%02d", m[1], mo, d)
	if m[4] != "" {
		h, _ := strconv.Atoi(m[4])
		s += fmt.Sprintf(" %02d:%s", h, m[5])
	}
	return s, true
}

// ReadDate reads a value that is a date as a whole: a numeric one is kept
// as written, a Korean one is written as numbers (year/mm/dd) so every
// reader of stored dates reads it. false when the value is no date.
func ReadDate(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if wholeNumericDate.MatchString(v) {
		return v, true
	}
	if m := wholeKoreanDate.FindStringSubmatch(v); m != nil {
		return normal(m)
	}
	return "", false
}

// KoreanDates are the Korean dates in a text, written as numbers, in order.
func KoreanDates(text string) []string {
	var out []string
	for _, m := range anyKoreanDate.FindAllStringSubmatch(text, -1) {
		if d, ok := normal(m); ok {
			out = append(out, d)
		}
	}
	return out
}

// StoryDateOf reads the date the critic wrote for a turn ("story_date"):
// one that names a day that exists, written as year/mm/dd; "" otherwise.
func StoryDateOf(v string) string {
	v = strings.TrimSpace(v)
	m := wholeNumericDate.FindStringSubmatch(v)
	if m == nil {
		m = wholeKoreanDate.FindStringSubmatch(v)
	}
	if m == nil {
		return ""
	}
	d, ok := normal(m)
	if !ok {
		return ""
	}
	return d
}
