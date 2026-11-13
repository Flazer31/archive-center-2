package generate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

func threeTurns(key string) *lifecycle.Session {
	s := lifecycle.NewSession(key)
	s.Apply(snap(lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"), row("u3", "user", "", "셋"), row("a3", "char", "g3", "셋째")))
	return s
}

// Bug report 11-12: after a failed call (a 429) every later turn of the
// chat was called at once and failed too. A failed call ends the chat's
// run: the later turns wait with it, the chat shows the wait and its time,
// and a turn given up leaves the next one a wait of its own.
func TestFailedCallStopsTheChatRun(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		db := testDB(t)
		shortWaits(t, time.Minute)
		broken := &fakeModel{fail: true}
		w := NewWorker(db, broken, "broken", time.Minute)
		w.Retries = 2
		runFor(t, w)
		s := threeTurns("chat:FCwait")
		save(w, s)
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return w.Chat(s.Key).RetryWaiting }) {
			t.Fatalf("no wait after the failed call: %+v", w.Chat(s.Key))
		}
		time.Sleep(300 * time.Millisecond)
		cs := w.Chat(s.Key)
		if n := broken.calls(); n != 1 {
			t.Fatalf("calls after one failed call: %d, want 1 (the later turns wait)", n)
		}
		if cs.Running || cs.RetryAt.IsZero() || cs.RetryAt.Before(time.Now().Add(30*time.Second)) {
			t.Fatalf("chat state %+v: waiting about a minute", cs)
		}
		// the critic is changed: the wait is dropped and every turn is made
		fixed := &fakeModel{}
		w.Configure(fixed, "fixed", 2)
		w.Rearm()
		if !waitFor(t, func() bool { return len(bodies(t, db, s)) == 3 }) {
			t.Fatalf("after the critic change: fixed calls %d, made %v", fixed.calls(), bodies(t, db, s))
		}
	})
	t.Run("newer or restart", func(t *testing.T) {
		// the later turns keep waiting when a newer notice runs the chat
		// again during the wait, and after a restart
		db := testDB(t)
		shortWaits(t, time.Minute)
		broken := &fakeModel{fail: true}
		w := NewWorker(db, broken, "broken", time.Minute)
		w.Retries = 2
		runFor(t, w)
		s := threeTurns("chat:FCnew")
		save(w, s)
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return w.Chat(s.Key).RetryWaiting }) {
			t.Fatalf("no wait after the failed call: %+v", w.Chat(s.Key))
		}
		w.Notify(s.Key, memory.Own(s))
		time.Sleep(300 * time.Millisecond)
		if n := broken.calls(); n != 1 {
			t.Fatalf("calls after a newer notice during the wait: %d, want 1", n)
		}
		if cs := w.Chat(s.Key); !cs.RetryWaiting || cs.RetryAt.Before(time.Now().Add(30*time.Second)) {
			t.Fatalf("wait after a newer notice: %+v", cs)
		}
		restarted := &fakeModel{fail: true}
		w2 := NewWorker(db, restarted, "broken", time.Minute)
		w2.Retries = 2
		runFor(t, w2)
		if err := w2.Resume(); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return w2.Chat(s.Key).RetryWaiting }) {
			t.Fatalf("no wait after a restart: %+v", w2.Chat(s.Key))
		}
		time.Sleep(300 * time.Millisecond)
		if n := restarted.calls(); n != 0 {
			t.Fatalf("calls after a restart during the wait: %d, want 0", n)
		}
	})
	t.Run("given up", func(t *testing.T) {
		db := testDB(t)
		shortWaits(t, 200*time.Millisecond)
		broken := &fakeModel{fail: true}
		w := NewWorker(db, broken, "broken", time.Minute)
		w.Retries = 0
		runFor(t, w)
		s := threeTurns("chat:FCgone")
		save(w, s)
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return broken.calls() >= 1 }) {
			t.Fatal("no call")
		}
		time.Sleep(80 * time.Millisecond)
		if n := broken.calls(); n != 1 {
			t.Fatalf("calls right after a turn was given up: %d, want 1 (the next one waits)", n)
		}
		// each turn is still called once, one wait apart
		if !waitFor(t, func() bool { return broken.calls() == 3 && w.Chat(s.Key).GivenUp == 3 }) {
			t.Fatalf("turns given up: calls %d, state %+v", broken.calls(), w.Chat(s.Key))
		}
		time.Sleep(500 * time.Millisecond)
		if n := broken.calls(); n != 3 {
			t.Fatalf("calls after every turn was given up: %d, want 3", n)
		}
		if cs := w.Chat(s.Key); cs.RetryWaiting {
			t.Fatalf("a wait left with no turn to call: %+v", cs)
		}
	})
	t.Run("broken answer goes on", func(t *testing.T) {
		// an answer that came broken is no failed call: the later turns go on
		db := testDB(t)
		shortWaits(t, time.Minute)
		m := &brokenFirst{}
		w := NewWorker(db, m, "m", time.Minute)
		w.Retries = 2
		runFor(t, w)
		s := threeTurns("chat:FCbad")
		save(w, s)
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return len(bodies(t, db, s)) == 2 }) {
			t.Fatalf("made %v, calls %d", bodies(t, db, s), m.calls())
		}
	})
}

// brokenFirst answers the first turn's text with no response object and the
// others well.
type brokenFirst struct{ fakeModel }

func (b *brokenFirst) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	if strings.Contains(turnLine.FindString(prompt), "첫째") {
		b.fakeModel.mu.Lock()
		b.fakeModel.prompts = append(b.fakeModel.prompts, prompt)
		b.fakeModel.mu.Unlock()
		return provider.Result{Content: "읽을 수 없는 답"}, nil
	}
	return b.fakeModel.Chat(ctx, prompt)
}

// The hold is read from the stored jobs: a turn given up after its failed
// call holds the later calls also after a newer notice or a restart; a part
// answer that came broken twice holds nothing; an answer already received
// is stored without a call while the hold lasts.
func TestHoldFromStore(t *testing.T) {
	// turn 1's job as a failed call left it: its calls used up (Retries 0),
	// its wait until a minute from now, with the given reason
	failTurn1 := func(t *testing.T, db *store.DB, s *lifecycle.Session, reason string) {
		t.Helper()
		failCalls(t, db, memory.Own(s)[0].Source, 1, reason)
	}
	start := func(t *testing.T, key string, m Provider) (*store.DB, *Worker, *lifecycle.Session) {
		db := testDB(t)
		w := NewWorker(db, m, "m", time.Minute)
		w.Retries = 0
		s := threeTurns(key)
		save(w, s)
		return db, w, s
	}
	t.Run("given up then newer", func(t *testing.T) {
		m := &fakeModel{}
		db, w, s := start(t, "chat:HSgone", m)
		failTurn1(t, db, s, "provider 429: rate limited")
		runFor(t, w) // a restart: the worker knows nothing of the failure
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return w.Chat(s.Key).RetryWaiting }) {
			t.Fatalf("no wait: %+v", w.Chat(s.Key))
		}
		w.Notify(s.Key, memory.Own(s))
		time.Sleep(300 * time.Millisecond)
		if n := m.calls(); n != 0 {
			t.Fatalf("calls while turn 1's failed call holds them: %d, want 0", n)
		}
	})
	t.Run("broken part answer", func(t *testing.T) {
		m := &fakeModel{}
		db, w, s := start(t, "chat:HSpart", m)
		failTurn1(t, db, s, ErrUnreadable.Error()+" (part 2 of 2)")
		runFor(t, w)
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return len(bodies(t, db, s)) == 2 }) {
			t.Fatalf("later turns after a broken answer: made %v, calls %d", bodies(t, db, s), m.calls())
		}
	})
	t.Run("named broken", func(t *testing.T) {
		m := &fakeModel{}
		db, w, s := start(t, "chat:HSname", m)
		failTurn1(t, db, s, "provider 429: "+ErrUnreadable.Error())
		runFor(t, w)
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return w.Chat(s.Key).RetryWaiting }) {
			t.Fatalf("no wait: %+v", w.Chat(s.Key))
		}
		time.Sleep(200 * time.Millisecond)
		if n := m.calls(); n != 0 {
			t.Fatalf("calls while a failed call holds them: %d, want 0", n)
		}
	})
	t.Run("long give up", func(t *testing.T) {
		// the real waits (1, 5, 30 minutes): a turn given up after its
		// second call holds the next one about a minute, not five
		m := &fakeModel{}
		db, w, s := start(t, "chat:HSlong", m)
		w.Retries = 1
		failCalls(t, db, memory.Own(s)[0].Source, 2, "provider 429: rate limited")
		runFor(t, w)
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return w.Chat(s.Key).RetryWaiting }) {
			t.Fatalf("no wait: %+v", w.Chat(s.Key))
		}
		if at := w.Chat(s.Key).RetryAt; at.After(time.Now().Add(90*time.Second)) || at.Before(time.Now().Add(30*time.Second)) {
			t.Fatalf("the next turn waits until %v (%v from now), want about a minute", at, time.Until(at))
		}
		if n := m.calls(); n != 0 {
			t.Fatalf("calls while held: %d", n)
		}
	})
	t.Run("received answer", func(t *testing.T) {
		m := &fakeModel{fail: true}
		db, w, s := start(t, "chat:HSrecv", m)
		failTurn1(t, db, s, "provider 429: rate limited")
		j2, err := db.JobFor(memory.Own(s)[1].Source)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.KeepResponse(j2, "m", "request", "", `{"memories":[{"id":"m1","kind":"fact","body":"둘째 기록"}]}`); err != nil {
			t.Fatal(err)
		}
		runFor(t, w)
		if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, func() bool { return len(bodies(t, db, s)) == 1 }) {
			t.Fatalf("the received answer was not stored: made %v, calls %d", bodies(t, db, s), m.calls())
		}
		time.Sleep(200 * time.Millisecond)
		if n := m.calls(); n != 0 {
			t.Fatalf("calls while held: %d, want 0", n)
		}
	})
}

// failCalls leaves a job as its calls left it: calls started, each failed
// with the reason, its next call after the wait the worker gives it.
func failCalls(t *testing.T, db *store.DB, src memory.Source, calls int, reason string) {
	t.Helper()
	for i := 0; i < calls; i++ {
		j, err := db.JobFor(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.StartAttempt(j); err != nil {
			t.Fatal(err)
		}
		if err := db.FailJob(j, "m", "request", "", reason, time.Now().Add(RetryDelay(j.Attempts+1))); err != nil {
			t.Fatal(err)
		}
	}
}

// failOnce fails the first record of a failed call (the store was down
// for a moment).
type failOnce struct {
	*store.DB
	mu     sync.Mutex
	failed bool
}

func (f *failOnce) FailJob(j store.Job, model, request, readItems, reason string, retryAt time.Time) error {
	f.mu.Lock()
	first := !f.failed
	f.failed = true
	f.mu.Unlock()
	if first {
		return errors.New("store down")
	}
	return f.DB.FailJob(j, model, request, readItems, reason, retryAt)
}

// A failed call whose record failed holds the later turns too.
func TestFailedCallHoldWithoutItsRecord(t *testing.T) {
	db := testDB(t)
	shortWaits(t, time.Minute)
	broken := &fakeModel{fail: true}
	w := NewWorker(&failOnce{DB: db}, broken, "broken", time.Minute)
	w.Retries = 2
	runFor(t, w)
	s := threeTurns("chat:FCrec")
	save(w, s)
	if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool { return broken.calls() >= 1 && w.Chat(s.Key).RetryWaiting }) {
		t.Fatalf("no wait: calls %d %+v", broken.calls(), w.Chat(s.Key))
	}
	time.Sleep(300 * time.Millisecond)
	if n := broken.calls(); n != 1 {
		t.Fatalf("calls after a failed call whose record failed: %d, want 1", n)
	}
}
