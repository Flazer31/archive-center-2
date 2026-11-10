package memory

import (
	"reflect"
	"testing"
)

// C12: every memory reads a date — its own time, the time of the event it
// comes from, that event's recorded date, or its own recorded date.
func TestWhenOfGivesEveryDatedMemoryADate(t *testing.T) {
	mem := func(id, kind, at, time, date string) KnownMemory {
		return KnownMemory{ID: id, Date: date, Memory: Memory{Kind: kind, At: at, Time: time}}
	}
	got := WhenOf([]KnownMemory{
		mem("M1", "event", "", "1423년 봄", "Day 0"), // own time
		mem("M2", "fact", "M1", "", "Day 2"),       // the event's time
		mem("M3", "event", "", "", "Day 1"),        // no time: its recorded date
		mem("M4", "view", "M3", "", "Day 3"),       // the event's recorded date
		mem("M5", "fact", "", "", "2026/01/02"),    // no event: its own recorded date
		mem("M6", "fact", "M99", "", "Day 4"),      // an event not read: its own date
		mem("M7", "fact", "", "", ""),              // nothing known
	})
	want := map[string]When{
		"M1": {"1423년 봄", "time"}, "M2": {"1423년 봄", "at"}, "M3": {"Day 1", "turn"},
		"M4": {"Day 1", "turn"}, "M5": {"2026/01/02", "turn"}, "M6": {"Day 4", "turn"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
