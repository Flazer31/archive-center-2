package generate

import (
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
)

// A reply the host put back from a server-side job (PocketRisu
// jobRecovery.ts:294-329 sets the message without the output listener) is
// seen first by a load. The next input confirms it like any reply: the
// turn the user went on from is made.
func TestRecoveredReplyIsMadeAtTheNextInput(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	t1 := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째 답")}
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, t1...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, append(append([]lifecycle.Row{}, t1...), row("u2", "user", "", "둘"))...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, append(append([]lifecycle.Row{}, t1...), row("u2", "user", "", "둘"), row("a2", "char", "g2", "복구된 답"))...))
	savedRun(t, w, s)
	if m.calls() != 1 {
		t.Fatalf("calls before the next input %d (only T1)", m.calls())
	}
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, append(append([]lifecycle.Row{}, t1...), row("u2", "user", "", "둘"), row("a2", "char", "g2", "복구된 답"), row("u3", "user", "", "셋"))...))
	savedRun(t, w, s)
	if got := a28Bodies(t, db, s); m.calls() != 2 || strings.Count(got, "복구된 답 기록") != 1 {
		t.Fatalf("calls %d memory %q", m.calls(), got)
	}
}

// The first turn of a chat followed live, its reply recovered without an
// output event, is made at the next input too (no earlier confirmed turn
// is needed).
func TestFirstRecoveredReplyIsMadeAtTheNextInput(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나")))
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "복구된 첫 답")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "복구된 첫 답"))
	savedRun(t, w, s)
	if got := a28Bodies(t, db, s); m.calls() != 1 || !strings.Contains(got, "복구된 첫 답 기록") {
		t.Fatalf("calls %d memory %q", m.calls(), got)
	}
}

// A chat opened with earlier turns: only the turn the user goes on from is
// made by the next input; the older history waits for the user to ask.
func TestOpenedHistoryIsNotMadeByTheNextInput(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	hist := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "예전 답"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "마지막 답")}
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, hist...))
	goOn(s, hist...)
	savedRun(t, w, s)
	if got := a28Bodies(t, db, s); m.calls() != 1 || strings.Contains(got, "예전 답") || !strings.Contains(got, "마지막 답 기록") {
		t.Fatalf("calls %d memory %q", m.calls(), got)
	}
}
