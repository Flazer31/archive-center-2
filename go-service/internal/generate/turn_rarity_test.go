package generate

import (
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
)

// A word the chat uses in many turns recalls nothing, even when only one
// stored memory has it; words used in few turns recall the memory that has
// them (KNOWN-DIAG-73: everyday words counted as rare over the stored
// memories pulled half of the generation input).
func TestRecallCountsWordsOverTheChatsTurns(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	// T1's memory is "마을 우물 기록"; T2's is "장터 흑요석 단검 기록". The
	// user writes 마을 in every turn, so the chat uses it often, while only
	// T1's memory has it.
	rows := []lifecycle.Row{
		row("u1", "user", "", "마을에 왔다"), row("a1", "char", "g1", "마을 우물"),
		row("u2", "user", "", "마을을 걸었다"), row("a2", "char", "g2", "장터 흑요석 단검"),
		row("u3", "user", "", "마을 어귀"), row("a3", "char", "g3", "흑요석 단검을 다시 꺼냈다"),
	}
	for i := 2; i <= len(rows); i += 2 {
		s.Apply(snap(lifecycle.SourceOutputEvent, rows[:i]...))
		goOn(s, rows[:i]...)
		run(w, s)
	}
	if m.calls() != 3 {
		t.Fatalf("%d calls", m.calls())
	}
	last := m.prompts[2]
	if strings.Contains(last, "마을 우물 기록") {
		t.Fatalf("a word of every turn recalled T1's memory:\n%s", last)
	}
	if !strings.Contains(last, "장터 흑요석 단검 기록") {
		t.Fatalf("words of few turns did not recall T2's memory:\n%s", last)
	}
}
