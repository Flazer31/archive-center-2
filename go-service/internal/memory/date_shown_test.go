package memory

import "testing"

// A delivered line shows a turn's date as yyyy/mm/dd whatever the chat's
// variable is called or how it writes the date; names stay only when the
// chat's date variables differ.
func TestDateShown(t *testing.T) {
	for in, want := range map[string]string{
		"body = 1423/05/15":                     "1423/05/15",
		"clock = 2900-9-24":                     "2900/09/24",
		"date = 2900.08.15":                     "2900/08/15",
		"body = 1423/05/15 9:05":                "1423/05/15 09:05",
		"현재 = 2026-01-02T13:40:00":              "2026/01/02 13:40",
		"a = 2026/1/2; b = 2026-01-02":          "2026/01/02",
		"birthday = 1400/3/1; now = 1423/05/15": "birthday 1400/03/01; now 1423/05/15",
		"Day 3":                                 "Day 3",
		"":                                      "",
	} {
		if got := DateShown(in); got != want {
			t.Errorf("DateShown(%q) = %q, want %q", in, got, want)
		}
	}
	// the stored text keeps its own form (the critic and the date readers use it)
	if got := DateText(map[string]string{"clock": "2900-9-24"}); got != "clock = 2900-9-24" {
		t.Fatalf("stored text %q", got)
	}
}
