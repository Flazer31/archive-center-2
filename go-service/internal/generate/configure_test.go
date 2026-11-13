package generate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Without a model set (settings not made yet) a confirmed turn waits; once
// the settings screen sets one, the waiting turn is made by it, and a model
// set later takes the next job.
func TestWorkerWaitsForConfigureThenUsesTheModelSet(t *testing.T) {
	db := testDB(t)
	w := NewWorker(db, nil, "", time.Minute)
	defer running(t, w)()
	s := lifecycle.NewSession("chat:CFG")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	w.Notify(s.Key, memory.Own(s))
	time.Sleep(300 * time.Millisecond)
	if got := bodies(t, db, s); len(got) != 0 {
		t.Fatalf("made %v with no model set", got)
	}
	first := &fakeModel{}
	w.Configure(first, "first", 3)
	deadline := time.Now().Add(10 * time.Second)
	for len(bodies(t, db, s)) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the waiting turn was not made after Configure")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if first.calls() != 1 {
		t.Fatalf("first model calls %d", first.calls())
	}
	second := &fakeModel{}
	w.Configure(second, "second", 0)
	if p, m := w.active(); p != Provider(second) || m != "second" || w.retries() != 0 {
		t.Fatalf("Configure not taken: %v %q %d", p, m, w.retries())
	}
	goOn(s, row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"))
	w.Notify(s.Key, memory.Own(s))
	for second.calls() == 0 {
		if time.Now().After(deadline.Add(10 * time.Second)) {
			t.Fatal("the next job did not use the model set later")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if first.calls() != 1 {
		t.Fatalf("the earlier model was called again: %d", first.calls())
	}
}

// A turn whose calls ran out with one critic is tried again once another
// critic is set (the store resets its attempts, Rearm runs the chats), by
// the new critic, once.
func TestRearmAfterCriticChangeMakesGivenUpTurns(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 50*time.Millisecond)
	broken := &fakeModel{fail: true}
	w := NewWorker(db, broken, "broken", time.Minute)
	w.Retries = 0
	runFor(t, w)
	s := lifecycle.NewSession("chat:RE")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	if !waitFor(t, func() bool { return broken.calls() == 1 }) {
		t.Fatal("the broken critic was not called")
	}
	time.Sleep(300 * time.Millisecond)
	if broken.calls() != 1 || len(bodies(t, db, s)) != 0 {
		t.Fatalf("with no retries left: calls %d bodies %v", broken.calls(), bodies(t, db, s))
	}
	fixed := &fakeModel{}
	w.Configure(fixed, "fixed", 0)
	time.Sleep(300 * time.Millisecond)
	if fixed.calls() != 0 {
		t.Fatalf("a given-up turn was called again without a rearm: %d", fixed.calls())
	}
	w.Rearm() // the worker resets the attempts between chats
	if !waitFor(t, func() bool { return len(bodies(t, db, s)) == 1 }) {
		t.Fatalf("the given-up turn was not made after the change: %v", bodies(t, db, s))
	}
	time.Sleep(200 * time.Millisecond)
	if fixed.calls() != 1 || broken.calls() != 1 {
		t.Fatalf("calls: fixed %d broken %d", fixed.calls(), broken.calls())
	}
	if n, _ := db.RearmFailedJobs(); n != 0 {
		t.Fatalf("a made turn was rearmed: %d", n)
	}
}

// The status screen hears a failed call, a turn given up (once, however
// many runs find it so) and a made turn.
func TestWorkerEventsForTheStatusScreen(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 50*time.Millisecond)
	broken := &fakeModel{fail: true}
	w := NewWorker(db, broken, "broken", time.Minute)
	w.Retries = 0
	var mu sync.Mutex
	var got []string
	w.Event = func(kind, session string, turn int, err error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf("%s %s %d %v", kind, session, turn, err != nil))
	}
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
	runFor(t, w)
	s := lifecycle.NewSession("chat:EV")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	if !waitFor(t, func() bool { return len(seen()) >= 2 }) {
		t.Fatalf("events %v", seen())
	}
	for i := 0; i < 3; i++ { // later runs find the turn given up again
		run(w, s)
		time.Sleep(100 * time.Millisecond)
	}
	if e := seen(); strings.Join(e, ",") != "failed chat:EV 1 true,given_up chat:EV 1 false" {
		t.Fatalf("events after the failure %v", e)
	}
	w.Configure(&fakeModel{}, "fixed", 0)
	w.Rearm()
	if !waitFor(t, func() bool {
		e := seen()
		return len(e) == 4 && e[2] == "rearmed  1 false" && e[3] == "made chat:EV 1 false"
	}) {
		t.Fatalf("events after the change %v", seen())
	}
}

// turnsOff turns generation off during its own call (the settings screen
// saving "사용 안 함" while a turn is made).
type turnsOff struct {
	w *Worker
	f *fakeModel
}

func (o turnsOff) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	o.w.Configure(nil, "", 0)
	return o.f.Chat(ctx, prompt)
}

// Generation turned off in the middle of a chat's run: the call under way
// ends with its answer stored, the chat's next turn waits (no call to a
// missing critic) and is made once a critic is set again.
func TestOffMidRunKeepsTurnsWaiting(t *testing.T) {
	db := testDB(t)
	first := &fakeModel{}
	w := NewWorker(db, nil, "", time.Minute)
	w.Configure(turnsOff{w, first}, "first", 0)
	s := lifecycle.NewSession("chat:OFF")
	rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째")}
	s.Apply(snap(lifecycle.SourceOutputEvent, rows...))
	goOn(s, rows...)
	run(w, s) // before the fix: nil provider called for the second turn
	if first.calls() != 1 || len(bodies(t, db, s)) != 1 {
		t.Fatalf("calls %d bodies %v", first.calls(), bodies(t, db, s))
	}
	later := &fakeModel{}
	w.Configure(later, "later", 0)
	runFor(t, w)
	if !waitFor(t, func() bool { return len(bodies(t, db, s)) == 2 }) {
		t.Fatalf("the waiting turn was not made after a critic was set: %v", bodies(t, db, s))
	}
	if later.calls() != 1 {
		t.Fatalf("later calls %d", later.calls())
	}
}

// pauseStart holds the first StartAttempt until gate closes: the old critic
// is taken and its attempt not yet counted.
type pauseStart struct {
	*store.DB
	once    sync.Once
	entered chan struct{}
	gate    chan struct{}
}

func (p *pauseStart) StartAttempt(j store.Job) error {
	p.once.Do(func() { close(p.entered); <-p.gate })
	return p.DB.StartAttempt(j)
}

// Another critic saved while the old one's call is starting: the old call
// ends and counts first, then the turn gets its calls again with the new
// critic (the rearm waits for the end of the work under way).
func TestRearmWaitsForTheCallUnderWay(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 50*time.Millisecond)
	ps := &pauseStart{DB: db, entered: make(chan struct{}), gate: make(chan struct{})}
	hold := make(chan struct{})
	old := &fakeModel{fail: true, hold: hold}
	w := NewWorker(ps, old, "old", time.Minute)
	w.Retries = 0
	runFor(t, w)
	s := lifecycle.NewSession("chat:RW")
	rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")}
	s.Apply(snap(lifecycle.SourceOutputEvent, rows...))
	goOn(s, rows...)
	save(w, s)
	w.Notify(s.Key, memory.Own(s))
	select {
	case <-ps.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the old attempt did not start")
	}
	// the old critic is taken, its attempt not yet counted: the chat is
	// running, not waiting or done
	if c := w.Chat(s.Key); !c.Running || c.Waiting || w.Diagnostics()["generation_running"] != true {
		t.Fatalf("chat state during the call %+v", c)
	}
	fixed := &fakeModel{}
	w.Configure(fixed, "new", 0)
	w.Rearm()
	close(ps.gate)
	close(hold) // the old call fails, after the change
	if !waitFor(t, func() bool { return len(bodies(t, db, s)) == 1 }) {
		t.Fatalf("the turn was not made by the new critic: old %d new %d", old.calls(), fixed.calls())
	}
	if old.calls() != 1 || fixed.calls() != 1 {
		t.Fatalf("calls: old %d new %d", old.calls(), fixed.calls())
	}
}

// failRearm fails the first RearmFailedJobs calls (a store outage).
type failRearm struct {
	*store.DB
	mu    sync.Mutex
	fails int
	calls int
}

func (f *failRearm) RearmFailedJobs() (int64, error) {
	f.mu.Lock()
	f.calls++
	fail := f.calls <= f.fails
	f.mu.Unlock()
	if fail {
		return 0, fmt.Errorf("store down")
	}
	return f.DB.RearmFailedJobs()
}

func (f *failRearm) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// A rearm whose reset fails is tried again after a wait, without another
// save.
func TestFailedRearmIsTriedAgain(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 50*time.Millisecond)
	fr := &failRearm{DB: db, fails: 2}
	w := NewWorker(fr, &fakeModel{}, "m", time.Minute)
	runFor(t, w)
	w.Rearm()
	// the third call enters the store; the rearm is done when it returns
	done := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return !w.rearm
	}
	if !waitFor(t, done) || fr.count() != 3 {
		t.Fatalf("the rearm was not tried again to its end: %d calls, done %v", fr.count(), done())
	}
}

// Another critic saved while the first of two turns is starting: each turn
// gets the new critic's calls once (retries 0), not the later turn twice.
func TestRearmBeforeTheNextTurn(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 50*time.Millisecond)
	ps := &pauseStart{DB: db, entered: make(chan struct{}), gate: make(chan struct{})}
	old := &fakeModel{fail: true}
	w := NewWorker(ps, old, "old", time.Minute)
	w.Retries = 0
	runFor(t, w)
	s := lifecycle.NewSession("chat:R5")
	rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째")}
	s.Apply(snap(lifecycle.SourceOutputEvent, rows...))
	goOn(s, rows...)
	save(w, s)
	w.Notify(s.Key, memory.Own(s))
	select {
	case <-ps.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the old attempt did not start")
	}
	newer := &fakeModel{fail: true}
	w.Configure(newer, "new", 0)
	w.Rearm()
	close(ps.gate)
	if !waitFor(t, func() bool { return newer.calls() >= 2 }) {
		t.Fatalf("calls: old %d new %d", old.calls(), newer.calls())
	}
	time.Sleep(400 * time.Millisecond) // any further call would come now
	if old.calls() != 1 || newer.calls() != 2 {
		t.Fatalf("calls: old %d new %d", old.calls(), newer.calls())
	}
}

// With no critic set, a chat whose vectors are missing is indexed (an edit,
// a new embedding key); its notice stays for the generation to come.
func TestIndexWithoutACritic(t *testing.T) {
	db := testDB(t)
	w := NewWorker(db, nil, "", time.Minute)
	var mu sync.Mutex
	var indexed [][]int64
	w.Index = func(_ context.Context, jobs []int64) error {
		mu.Lock()
		defer mu.Unlock()
		indexed = append(indexed, jobs)
		return nil
	}
	s := lifecycle.NewSession("chat:IX")
	rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")}
	s.Apply(snap(lifecycle.SourceOutputEvent, rows...))
	goOn(s, rows...)
	save(w, s)
	j, err := db.JobFor(memory.Own(s)[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"하나의 기록"}]}`)
	if _, err := db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
		t.Fatal(err)
	}
	runFor(t, w)
	w.Reindex(s.Key)
	if !waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(indexed) == 1 }) {
		t.Fatal("no index without a critic")
	}
	mu.Lock()
	got := indexed[0]
	mu.Unlock()
	if len(got) != 1 || got[0] != j.ID {
		t.Fatalf("indexed %v, want job %d", got, j.ID)
	}
	time.Sleep(200 * time.Millisecond)
	if c := w.Chat(s.Key); !c.Waiting || w.Diagnostics()["index_backlog_chats"] != 0 {
		t.Fatalf("after the index: %+v backlog %v", c, w.Diagnostics()["index_backlog_chats"])
	}
	mu.Lock()
	n := len(indexed)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("indexed again and again: %d", n)
	}
}

// sessionModel records the chat each call was made for.
type sessionModel struct {
	fakeModel
	mu   sync.Mutex
	seen []string
}

func (m *sessionModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	m.mu.Lock()
	m.seen = append(m.seen, provider.SessionOf(ctx))
	m.mu.Unlock()
	return m.fakeModel.Chat(ctx, prompt)
}

// A turn's call carries its chat, as AC1 gives a provider the chat session
// id (OpenCode Go's x-opencode-session).
func TestCallsCarryTheirChat(t *testing.T) {
	db := testDB(t)
	m := &sessionModel{}
	w := NewWorker(db, m, "m", time.Minute)
	s := lifecycle.NewSession("chat:SESS")
	rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")}
	s.Apply(snap(lifecycle.SourceOutputEvent, rows...))
	goOn(s, rows...)
	run(w, s)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.seen) != 1 || m.seen[0] != "SESS" {
		t.Fatalf("sessions %v", m.seen)
	}
}

// A failed rearm reset waits its time even with a chat waiting to run: the
// chat would stop for the rearm before its next turn.
func TestFailedRearmWaitsWithAChatWaiting(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 400*time.Millisecond)
	fr := &failRearm{DB: db, fails: 6}
	w := NewWorker(fr, &fakeModel{}, "m", time.Minute)
	s := lifecycle.NewSession("chat:RW5")
	rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")}
	s.Apply(snap(lifecycle.SourceOutputEvent, rows...))
	goOn(s, rows...)
	save(w, s)
	w.Notify(s.Key, memory.Own(s))
	w.Rearm()
	runFor(t, w)
	time.Sleep(200 * time.Millisecond)
	if c := fr.count(); c != 1 {
		t.Fatalf("resets within the wait: %d", c)
	}
	if !waitFor(t, func() bool { return fr.count() >= 2 }) {
		t.Fatalf("not tried again after the wait: %d", fr.count())
	}
}

// An index request made while the chat is being indexed (a memory edited
// then) is not taken by that run: the chat is indexed again (F02).
func TestIndexAskedDuringAnIndexRunsAgain(t *testing.T) {
	for _, critic := range []bool{false, true} {
		t.Run(fmt.Sprint("critic ", critic), func(t *testing.T) {
			db := testDB(t)
			var p Provider
			if critic {
				p = &fakeModel{}
			}
			w := NewWorker(db, p, "m", time.Minute)
			entered, release := make(chan struct{}), make(chan struct{})
			var mu sync.Mutex
			runs := 0
			w.Index = func(context.Context, []int64) error {
				mu.Lock()
				runs++
				first := runs == 1
				mu.Unlock()
				if first {
					close(entered)
					<-release
				}
				return nil
			}
			s := lifecycle.NewSession("chat:IA")
			rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")}
			s.Apply(snap(lifecycle.SourceOutputEvent, rows...))
			save(w, s)
			j, _ := db.JobFor(memory.Own(s)[0].Source)
			out, _ := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"하나의 기록"}]}`)
			if _, err := db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
				t.Fatal(err)
			}
			runFor(t, w)
			w.Reindex(s.Key)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("no index")
			}
			w.Reindex(s.Key) // an edit while the index runs
			close(release)
			if !waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return runs == 2 }) {
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("index runs %d", runs)
			}
		})
	}
}

// An index asked before the run (an edit) is made even when another turn's
// generation fails in that run and waits for its retry.
func TestAskedIndexDoesNotWaitForAFailedTurn(t *testing.T) {
	db := testDB(t)
	shortWaits(t, time.Hour) // the failed turn's retry is far away
	m := &fakeModel{fail: true}
	w := NewWorker(db, m, "m", time.Minute)
	w.Retries = 3
	var mu sync.Mutex
	runs := 0
	w.Index = func(context.Context, []int64) error {
		mu.Lock()
		defer mu.Unlock()
		runs++
		return nil
	}
	s := lifecycle.NewSession("chat:R61")
	first := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")}
	s.Apply(snap(lifecycle.SourceOutputEvent, first...))
	save(w, s)
	j, _ := db.JobFor(memory.Own(s)[0].Source)
	out, _ := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"하나의 기록"}]}`)
	if _, err := db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
		t.Fatal(err)
	}
	both := append(first, row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"))
	s.Apply(snap(lifecycle.SourceOutputEvent, both...))
	goOn(s, both...) // the second turn is ready to be made, and fails
	save(w, s)
	w.Reindex(s.Key) // a memory of the first turn was edited
	chatNow(w, s.Key, notice{turns: memory.Own(s)})
	mu.Lock()
	defer mu.Unlock()
	if m.calls() == 0 || runs != 1 {
		t.Fatalf("model calls %d, index runs %d", m.calls(), runs)
	}
	if w.Chat(s.Key).RetryWaiting != true || w.Diagnostics()["index_backlog_chats"] != 0 {
		t.Fatalf("after the run: %+v backlog %v", w.Chat(s.Key), w.Diagnostics()["index_backlog_chats"])
	}
}
