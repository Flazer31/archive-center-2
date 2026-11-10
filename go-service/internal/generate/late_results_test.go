package generate

import (
	"context"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// Step 5-5: the chat changes while the model is answering. A late result
// stays the history of the source it was made from; the current memory and
// the next generation input read the chat as it is now, and a notice made
// stale by a newer one does not call the model for turns the chat no
// longer has.

// running starts the worker and returns a stop function.
func running(t *testing.T, w *Worker) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() { w.Run(ctx); close(ended) }()
	return func() { cancel(); <-ended }
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func notify(w *Worker, s *lifecycle.Session) {
	save(w, s)
	w.Notify(s.Key, memory.Own(s))
}

// T1 and T2 are confirmed; while T1 is being made the user deletes T2. The
// worker does not call the model for T2 from the older notice.
func TestStaleNoticeDoesNotMakeADeletedTurn(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{hold: make(chan struct{})}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"), row("u3", "user", "", "셋")}
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, rows[:4]...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, rows...))
	if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil { // T1 asked, T2 confirmed
		t.Fatal(err)
	}
	stop := running(t, w)
	defer stop()
	notify(w, s)
	waitUntil(t, "the T1 call", func() bool { return m.calls() == 1 })
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, rows[0], rows[1], rows[4])) // T2 deleted
	notify(w, s)
	close(m.hold)
	waitUntil(t, "the runs to end", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.order) == 0 && len(w.pending) == 0
	})
	time.Sleep(50 * time.Millisecond)
	if m.calls() != 1 {
		t.Fatalf("calls %d: the deleted T2 was made from the older notice", m.calls())
	}
	if got := a28Bodies(t, db, s); got != "첫째 기록" {
		t.Fatalf("memory %q", got)
	}
}

// While T1's first reply is being made the user rerolls it and goes on.
// The late result is the first reply's history; the current memory is the
// new reply's, and T2's generation reads the new reply's memory.
func TestLateResultOfARerolledReplyStaysHistory(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{hold: make(chan struct{})}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "처음 답")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "처음 답"))
	first := memory.Own(s)[0].Source
	stop := running(t, w)
	defer stop()
	notify(w, s)
	waitUntil(t, "the first call", func() bool { return m.calls() == 1 })
	// the user deletes the next input, rerolls T1 and goes on
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나")))
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("b1", "char", "g2", "다시 쓴 답")))
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나"), row("b1", "char", "g2", "다시 쓴 답"), row("u2", "user", "", "둘")))
	notify(w, s)
	close(m.hold)
	waitUntil(t, "the reroll to be made", func() bool { return m.calls() == 2 })
	waitUntil(t, "the runs to end", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.order) == 0 && len(w.pending) == 0
	})
	time.Sleep(50 * time.Millisecond)
	if j, _ := db.JobFor(first); j.State != "done" {
		t.Fatalf("the late result was not kept as the first reply's history: %+v", j)
	}
	if got := a28Bodies(t, db, s); got != "다시 쓴 답 기록" {
		t.Fatalf("current memory %q", got)
	}
	stop()
	// T2's generation reads the current T1
	m.hold = nil
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("b1", "char", "g2", "다시 쓴 답"), row("u2", "user", "", "둘"), row("a2", "char", "g3", "다시 쓴 답의 기록을 떠올렸다")))
	goOn(s, row("u1", "user", "", "하나"), row("b1", "char", "g2", "다시 쓴 답"), row("u2", "user", "", "둘"), row("a2", "char", "g3", "다시 쓴 답의 기록을 떠올렸다"))
	savedRun(t, w, s)
	last := m.prompts[len(m.prompts)-1]
	if m.calls() != 3 || strings.Contains(last, "처음 답 기록") || !strings.Contains(last, "다시 쓴 답 기록") {
		t.Fatalf("calls %d, T2 input:\n%s", m.calls(), last)
	}
}

// Another chat's notice while a chat's turn is being made waits for its
// turn and is made from its own observation.
func TestOtherChatsNoticeDuringACall(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{hold: make(chan struct{})}
	w := NewWorker(db, m, "fake", time.Minute)
	a := lifecycle.NewSession("chat:A")
	a.Apply(a28Snap(a.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "A 답")))
	goOn(a, row("u1", "user", "", "하나"), row("a1", "char", "g1", "A 답"))
	b := lifecycle.NewSession("chat:B")
	b.Apply(a28Snap(b.Key, lifecycle.SourceOutputEvent, row("v1", "user", "", "하나"), row("b1", "char", "h1", "B 답")))
	goOn(b, row("v1", "user", "", "하나"), row("b1", "char", "h1", "B 답"))
	stop := running(t, w)
	defer stop()
	notify(w, a)
	waitUntil(t, "A's call", func() bool { return m.calls() == 1 })
	notify(w, b)
	close(m.hold)
	waitUntil(t, "B's call", func() bool { return m.calls() == 2 })
	waitUntil(t, "B's memory", func() bool { return a28Bodies(t, db, b) == "B 답 기록" })
	if got := a28Bodies(t, db, a); got != "A 답 기록" {
		t.Fatalf("A %q", got)
	}
}
