package generate

import (
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
)

// Rerolls and reply choices as the hosts do them. A turn is made when the
// user goes on (the next input), so every choice before it costs no call:
// the reply kept is made once.
//
// PocketRisu (DefaultChatScreen.svelte:493-584): a reroll takes the last
// reply off before the request; a failure puts the old messages back; a
// success writes the new reply as a new message holding swipes [A, B], and
// the arrows change that message's data only.
// RisuAI (DefaultChatScreen.svelte:219-301): the arrows put back the cached
// earlier message (its id and text), or, for the replies of one request
// (Prereroll), change the same message's data.

func rerollScript(t *testing.T, steps func(s *watched)) (*fakeModel, string, *lifecycle.Session) {
	t.Helper()
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := &watched{Session: lifecycle.NewSession("chat:A"), t: t, w: w, m: m}
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나")))
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "답 가")))
	steps(s)
	savedRun(t, w, s.Session)
	return m, a28Bodies(t, db, s.Session), s.Session
}

// watched runs the worker after each observation before the next input, as
// the backend does on every change: no choice is made before the user goes
// on (ASTRA-RECHECK-53 4.1).
type watched struct {
	*lifecycle.Session
	t *testing.T
	w *Worker
	m *fakeModel
}

func (s *watched) Apply(sn lifecycle.Snapshot) {
	s.Session.Apply(sn)
	savedRun(s.t, s.w, s.Session)
	if s.m.calls() != 0 {
		s.t.Fatalf("a reply was made before the next input (%s): %d calls", sn.Source, s.m.calls())
	}
}

func TestPocketRisuFailedRerollMakesTheOldReplyOnce(t *testing.T) {
	m, got, s := rerollScript(t, func(s *watched) {
		s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나")))
		s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "답 가")))
		goOn(s.Session, row("u1", "user", "", "하나"), row("a1", "char", "g1", "답 가"))
	})
	turns := s.ActiveTurns()
	if m.calls() != 1 || strings.Count(got, "답 가 기록") != 1 || turns[0].ID != 1 || len(turns[0].Responses[0].Revisions) != 1 {
		t.Fatalf("calls %d memory %q turns %+v", m.calls(), got, turns)
	}
}

func TestPocketRisuSwipeBackMakesTheChosenReplyOnce(t *testing.T) {
	m, got, _ := rerollScript(t, func(s *watched) {
		s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나")))
		s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a2", "char", "g2", "답 나")))
		// the left arrow: the new message's data back to A (its generation id stays)
		s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a2", "char", "g2", "답 가")))
		goOn(s.Session, row("u1", "user", "", "하나"), row("a2", "char", "g2", "답 가"))
	})
	if m.calls() != 1 || strings.Count(got, "답 가 기록") != 1 || strings.Contains(got, "답 나") {
		t.Fatalf("calls %d memory %q", m.calls(), got)
	}
}

func TestRisuAIRerollBackMakesTheChosenReplyOnce(t *testing.T) {
	m, got, _ := rerollScript(t, func(s *watched) {
		s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나")))
		s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a2", "char", "g2", "답 나")))
		// unReroll: the cached first message comes back whole
		s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "답 가")))
		goOn(s.Session, row("u1", "user", "", "하나"), row("a1", "char", "g1", "답 가"))
	})
	if m.calls() != 1 || strings.Count(got, "답 가 기록") != 1 || strings.Contains(got, "답 나") {
		t.Fatalf("calls %d memory %q", m.calls(), got)
	}
}

func TestRisuAIOneRequestRepliesMakeTheChosenReplyOnce(t *testing.T) {
	m, got, _ := rerollScript(t, func(s *watched) {
		// Prereroll / PreUnreroll: the same message's data, A -> B -> A
		s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "답 나")))
		s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "답 가")))
		goOn(s.Session, row("u1", "user", "", "하나"), row("a1", "char", "g1", "답 가"))
	})
	if m.calls() != 1 || strings.Count(got, "답 가 기록") != 1 || strings.Contains(got, "답 나") {
		t.Fatalf("calls %d memory %q", m.calls(), got)
	}
}
