package generate

import (
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
)

func off(r lifecycle.Row, v string) lifecycle.Row { r.Disabled = v; return r }

// A turn the user turned off in the host (a one-turn OOC) is not made into
// memory; the turns around it are. "allBefore" does not turn turns off.
func TestDisabledTurnIsNotMade(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	t1 := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째 답")}
	t2 := []lifecycle.Row{off(row("u2", "user", "", "(OOC: 잠깐)"), "true"), off(row("a2", "char", "g2", "OOC 답"), "true")}
	t3 := []lifecycle.Row{off(row("u3", "user", "", "셋"), "allBefore"), row("a3", "char", "g3", "셋째 답")}
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, t1...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, append(append([]lifecycle.Row{}, t1...), t2[0])...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, append(append([]lifecycle.Row{}, t1...), t2...)...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, append(append(append([]lifecycle.Row{}, t1...), t2...), t3[0])...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, append(append(append([]lifecycle.Row{}, t1...), t2...), t3...)...))
	goOn(s, append(append(append([]lifecycle.Row{}, t1...), t2...), t3...)...)
	savedRun(t, w, s)
	got := a28Bodies(t, db, s)
	if m.calls() != 2 || strings.Contains(got, "OOC") || !strings.Contains(got, "첫째 답 기록") || !strings.Contains(got, "셋째 답 기록") {
		t.Fatalf("calls %d memory %q", m.calls(), got)
	}
	re, _, err := db.LoadSession(s.Key)
	if err != nil || !re.ActiveTurns()[1].Off || re.ActiveTurns()[2].Off {
		t.Fatalf("stored off marks lost: %v", err)
	}
}

// Turning off a turn that already has memory takes its memory out of the
// chat's current memory; turning it on again brings it back without
// making it again.
func TestTurningATurnOffAndOnAgain(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째 답")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째 답"))
	savedRun(t, w, s)
	if !strings.Contains(a28Bodies(t, db, s), "첫째 답 기록") {
		t.Fatal("not made")
	}
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), off(row("a1", "char", "g1", "첫째 답"), "true")))
	savedRun(t, w, s)
	if got := a28Bodies(t, db, s); strings.Contains(got, "첫째 답") {
		t.Fatalf("off turn still read: %q", got)
	}
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째 답")))
	savedRun(t, w, s)
	if got := a28Bodies(t, db, s); !strings.Contains(got, "첫째 답 기록") || m.calls() != 1 {
		t.Fatalf("on again: calls %d memory %q", m.calls(), got)
	}
}
