package memory

import (
	"reflect"
	"testing"
)

// Dates the backend reads: numeric ones as written, Korean ones written
// as numbers, a day that does not exist is no date; the critic's
// story_date is read from a response, cut off or whole, and the date at
// the end of a long reply's parts is the turn's.
func TestKoreanAndCriticDates(t *testing.T) {
	for in, want := range map[string]string{
		"1423-5-15":                 "1423-5-15",
		"1423년 5월 15일":              "1423/05/15",
		"2026년 10월 31일 (토)":         "2026/10/31",
		"1423년5월15일 13:05":          "1423/05/15 13:05",
		"923년 1월 2일":                "923/01/02",
		"1423년 2월 30일":              "",
		"2026년 10월 31일 오후":          "",
		`{"d":"2026년 10월 31일 (토)"}`: "",
	} {
		got, ok := ReadDate(in)
		if want == "" && ok || want != "" && got != want {
			t.Errorf("ReadDate(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	if got := KoreanDates("어제는 1423년 5월 14일, 오늘은 1423년 5월 15일 (월)이다. 3년 2개월 전 일"); !reflect.DeepEqual(got, []string{"1423/05/14", "1423/05/15"}) {
		t.Fatalf("Korean dates in a text %v", got)
	}
	for in, want := range map[string]string{"1423/05/15": "1423/05/15", "1423-5-15": "1423/05/15", "1423년 5월 15일": "1423/05/15", "1423/02/30": "", "day 3": "", "": ""} {
		if got := StoryDateOf(in); got != want {
			t.Errorf("StoryDateOf(%q) = %q, want %q", in, got, want)
		}
	}
	out, _ := Parse(`{"elapsed_days":1,"story_date":"1423/05/16","memories":[{"id":"m1","kind":"event","body":"도착했다"}]}`)
	if out.StoryDate != "1423/05/16" || len(out.Memories) != 1 {
		t.Fatalf("whole response: %+v", out)
	}
	out, _ = Parse(`{"elapsed_days":1,"story_date":"1423/05/16","memories":[{"id":"m1","kind":"event","body":"도착했`)
	if out.StoryDate != "1423/05/16" {
		t.Fatalf("cut-off response lost the date: %+v", out)
	}
	if m := MergeParts([]Output{{StoryDate: "1423/05/15"}, {}, {StoryDate: "1423/05/16"}, {}}); m.StoryDate != "1423/05/16" {
		t.Fatalf("parts: %q", m.StoryDate)
	}
}

// A response holding only the date is a response and is not counted
// damaged (not asked again); a bad date in a later part does not wipe an
// earlier part's; a longer number is not read as a year.
func TestStoryDateReviewCases(t *testing.T) {
	out, notes := Parse(`{"story_date":"1423/05/16"}`)
	if out.StoryDate != "1423/05/16" || Unreadable(notes) || Damaged(out, notes) {
		t.Fatalf("a date-only response: %+v %v", out, notes)
	}
	out, notes = Parse(`{"elapsed_days":0,"story_date":"1423/05/16","memories":[{"id":"m1","kind":"event","body":"잘`)
	if out.StoryDate != "1423/05/16" || Damaged(out, notes) {
		t.Fatalf("a cut-off response with only its date: %+v %v", out, notes)
	}
	if m := MergeParts([]Output{{StoryDate: "1423/05/15"}, {StoryDate: "1423/02/30"}, {StoryDate: " "}}); m.StoryDate != "1423/05/15" {
		t.Fatalf("a later bad date wiped the earlier one: %q", m.StoryDate)
	}
	if got := KoreanDates("11423년 5월 15일"); len(got) != 0 {
		t.Fatalf("the end of a longer number read as a year: %v", got)
	}
}
