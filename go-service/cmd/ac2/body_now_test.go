package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// The body is the scene's now: it follows the latest reply.

// bodyNowChat is a chat of three turns whose replies are the given texts,
// read as the host has it, with the first turn's memory made (리나's period
// starts) and the second's (a day passes).
func bodyNowChat(t *testing.T, db *store.DB, chat string, replies [3]string, second string) (*pipeline.Pipeline, []memory.ScopedTurn) {
	t.Helper()
	p := pipeline.New(db)
	if _, err := p.Process([]observe.Record{memEvent(1, "load", chat,
		memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", replies[0]),
		memRow(2, "user", "u2", "", "다음"), memRow(3, "char", "a2", "g2", replies[1]),
		memRow(4, "user", "u3", "", "그 다음"), memRow(5, "char", "a3", "g3", replies[2]))}); err != nil {
		t.Fatal(err)
	}
	var scope []memory.ScopedTurn
	p.WithSession("chat:"+chat, func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	if len(scope) != 3 {
		t.Fatalf("scope %d", len(scope))
	}
	u3Save(t, db, scope[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 왔다"}],"body_events":[{"kind":"period_start","who":"n1"}]}`)
	u3Save(t, db, scope[1].Source, second)
	db.SetBodyTracking(body.Toggles{Cycle: true})
	return p, scope
}

func bodyState(t *testing.T, db *store.DB, chat string, scope []memory.ScopedTurn) body.State {
	t.Helper()
	all, err := db.BodyStates("chat:"+chat, scope)
	if err != nil || len(all) != 1 {
		t.Fatalf("states %+v %v", all, err)
	}
	return all[0]
}

// A: before the latest reply's memory is made the lines say so; a date its
// text gives is read at once; once made, its days count and the note goes.
func TestBodyFollowsTheLatestReply(t *testing.T) {
	db := modelsDB(t)
	_, scope := bodyNowChat(t, db, "BN", [3]string{"리나가 왔다", "하루가 지났다", "또 이틀이 지났다"}, `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"하루가 지났다"}]}`)
	if _, err := db.JobFor(scope[2].Source); err != nil { // asked, not made yet
		t.Fatal(err)
	}
	st := bodyState(t, db, "BN", scope)
	lines := body.Lines([]body.State{st}, func(id string) string { return id })
	if !st.Pending || st.Now.Text() != "Day 1" || len(lines) < 2 || lines[0] != body.PendingLine {
		t.Fatalf("before the latest reply is made: pending %v now %s lines %v", st.Pending, st.Now.Text(), lines)
	}
	u3Save(t, db, scope[2].Source, `{"elapsed_days":2,"memories":[{"id":"m1","kind":"event","body":"이틀이 지났다"}]}`)
	st = bodyState(t, db, "BN", scope)
	lines = body.Lines([]body.State{st}, func(id string) string { return id })
	if st.Pending || st.Now.Text() != "Day 3" || strings.Contains(strings.Join(lines, "\n"), "body note") {
		t.Fatalf("made: pending %v now %s lines %v", st.Pending, st.Now.Text(), lines)
	}
	if c := st.Cycle; c == nil || c.CycleDay.Min != 4 || c.CycleDay.Max != 4 {
		t.Fatalf("Day 3 after a period on Day 0: %+v", c)
	}

	// a date in the latest reply's text counts before its memory is made
	_, scope = bodyNowChat(t, db, "BD", [3]string{"1423년 5월 15일, 리나가 왔다", "다음 날이다", "1423년 5월 20일 아침이다"}, `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"하루가 지났다"}]}`)
	if _, err := db.JobFor(scope[2].Source); err != nil {
		t.Fatal(err)
	}
	if st := bodyState(t, db, "BD", scope); st.Now.Text() != "1423/05/20" || !st.Pending {
		t.Fatalf("the latest reply's date: now %s pending %v", st.Now.Text(), st.Pending)
	}

	// a calendar date in a chat counted in Days is not read before the turn
	// is made: the chat's days and the model start stay
	_, scope = bodyNowChat(t, db, "BA", [3]string{"리나가 왔다", "하루가 지났다", "편지에는 1423년 5월 20일이라고 적혀 있었다"}, `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"하루가 지났다"}]}`)
	if st := bodyState(t, db, "BA", scope); st.Now.Text() != "Day 1" || !st.Pending || st.CycleRef.Text() != "Day 0" {
		t.Fatalf("another axis: now %s pending %v ref %s", st.Now.Text(), st.Pending, st.CycleRef.Text())
	}
}

// holdModel answers the latest turn's call only when released.
type holdModel struct {
	started chan struct{}
	release chan struct{}
}

func (m *holdModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	close(m.started)
	select {
	case <-m.release:
	case <-ctx.Done():
		return provider.Result{}, ctx.Err()
	}
	return provider.Result{Content: `{"elapsed_days":2,"memories":[{"id":"m1","kind":"event","body":"이틀이 지났다"}]}`}, nil
}

// B: with the switch on, the request waits for the latest reply's memory
// (and no longer than the limit); off, or with nothing making it, it does
// not wait.
func TestBodyWaitsForTheLatestReply(t *testing.T) {
	start := func(t *testing.T, chat string) (*store.DB, *generate.Worker, *holdModel, []memory.ScopedTurn) {
		db := modelsDB(t)
		_, scope := bodyNowChat(t, db, chat, [3]string{"리나가 왔다", "하루가 지났다", "또 이틀이 지났다"}, `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"하루가 지났다"}]}`)
		m := &holdModel{started: make(chan struct{}), release: make(chan struct{})}
		w := generate.NewWorker(db, m, "hold", time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { w.Run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		if _, err := w.Backfill("chat:"+chat, scope, 0, false); err != nil {
			t.Fatal(err)
		}
		select {
		case <-m.started:
		case <-time.After(5 * time.Second):
			t.Fatal("the latest reply's call did not start")
		}
		return db, w, m, scope
	}
	t.Run("on", func(t *testing.T) {
		db, w, m, scope := start(t, "BW")
		db.SetBodyTracking(body.Toggles{Cycle: true, WaitLatest: true})
		go func() { time.Sleep(300 * time.Millisecond); close(m.release) }()
		began := time.Now()
		waitLatestReply(context.Background(), db, w, "chat:BW", scope, 5*time.Second)
		if took := time.Since(began); took < 250*time.Millisecond || took > 4*time.Second {
			t.Fatalf("waited %v", took)
		}
		if st := bodyState(t, db, "BW", scope); st.Pending || st.Now.Text() != "Day 3" {
			t.Fatalf("after the wait: pending %v now %s", st.Pending, st.Now.Text())
		}
	})
	t.Run("limit", func(t *testing.T) {
		db, w, m, scope := start(t, "BL")
		defer close(m.release)
		db.SetBodyTracking(body.Toggles{Cycle: true, WaitLatest: true})
		began := time.Now()
		waitLatestReply(context.Background(), db, w, "chat:BL", scope, 400*time.Millisecond)
		if took := time.Since(began); took < 350*time.Millisecond || took > 2*time.Second {
			t.Fatalf("waited %v, the limit 400ms", took)
		}
		if st := bodyState(t, db, "BL", scope); !st.Pending || st.Now.Text() != "Day 1" {
			t.Fatalf("after the limit: pending %v now %s", st.Pending, st.Now.Text())
		}
	})
	t.Run("off", func(t *testing.T) {
		db, w, m, scope := start(t, "BO")
		defer close(m.release)
		began := time.Now()
		waitLatestReply(context.Background(), db, w, "chat:BO", scope, 5*time.Second)
		if took := time.Since(began); took > 200*time.Millisecond {
			t.Fatalf("waited %v with the switch off", took)
		}
	})
	t.Run("request deadline", func(t *testing.T) {
		// the request's own deadline comes first
		db, w, m, scope := start(t, "BC")
		defer close(m.release)
		db.SetBodyTracking(body.Toggles{Cycle: true, WaitLatest: true})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		began := time.Now()
		waitLatestReply(ctx, db, w, "chat:BC", scope, 5*time.Second)
		if took := time.Since(began); took < 250*time.Millisecond || took > 2*time.Second {
			t.Fatalf("waited %v, the request's deadline 300ms", took)
		}
	})
	t.Run("no critic, a notice waiting", func(t *testing.T) {
		// a notice kept for a critic to come is not being made
		db := modelsDB(t)
		_, scope := bodyNowChat(t, db, "BN2", [3]string{"리나가 왔다", "하루가 지났다", "또 이틀이 지났다"}, `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"하루가 지났다"}]}`)
		db.SetBodyTracking(body.Toggles{Cycle: true, WaitLatest: true})
		w := generate.NewWorker(db, nil, "", time.Minute)
		w.Notify("chat:BN2", scope)
		if !w.Chat("chat:BN2").Waiting {
			t.Fatal("the notice should wait")
		}
		began := time.Now()
		waitLatestReply(context.Background(), db, w, "chat:BN2", scope, 5*time.Second)
		if took := time.Since(began); took > 200*time.Millisecond {
			t.Fatalf("waited %v with no critic", took)
		}
	})
	t.Run("nothing making it", func(t *testing.T) {
		db := modelsDB(t)
		_, scope := bodyNowChat(t, db, "BX", [3]string{"리나가 왔다", "하루가 지났다", "또 이틀이 지났다"}, `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"하루가 지났다"}]}`)
		db.SetBodyTracking(body.Toggles{Cycle: true, WaitLatest: true})
		w := generate.NewWorker(db, nil, "", time.Minute) // no critic: nothing will make it
		began := time.Now()
		waitLatestReply(context.Background(), db, w, "chat:BX", scope, 5*time.Second)
		if took := time.Since(began); took > 200*time.Millisecond {
			t.Fatalf("waited %v with no critic", took)
		}
	})
}
