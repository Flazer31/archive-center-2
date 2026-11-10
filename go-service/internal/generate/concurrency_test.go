package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"archivecenter2/goservice/internal/embed"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Only the provider is replaced. SQL, source ownership, KNOWN assembly,
// attempts, retries and dispatch all use the production implementations.
type concurrentModel struct {
	mu                sync.Mutex
	want              map[string]string // reply -> chat without prefix; unexpected input fails
	gates             map[string]<-chan struct{}
	fail              map[string]int
	prompts           map[string][]string
	active, peak      int
	perChat, peakChat map[string]int
}

func concurrencyModel(want map[string]string) *concurrentModel {
	return &concurrentModel{want: want, gates: map[string]<-chan struct{}{}, fail: map[string]int{}, prompts: map[string][]string{}, perChat: map[string]int{}, peakChat: map[string]int{}}
}

func (m *concurrentModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	line := turnLine.FindStringSubmatch(prompt)
	if len(line) != 2 {
		return provider.Result{}, errors.New("unexpected prompt without REPLY")
	}
	reply, chat := line[1], provider.SessionOf(ctx)
	m.mu.Lock()
	if want, ok := m.want[reply]; !ok || want != chat {
		m.mu.Unlock()
		return provider.Result{}, fmt.Errorf("unexpected provider input: chat=%q reply=%q", chat, reply)
	}
	m.prompts[reply] = append(m.prompts[reply], prompt)
	m.active++
	m.perChat[chat]++
	m.peak = max(m.peak, m.active)
	m.peakChat[chat] = max(m.peakChat[chat], m.perChat[chat])
	gate := m.gates[reply]
	fail := m.fail[reply] > 0
	if fail {
		m.fail[reply]--
	}
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.active--; m.perChat[chat]--; m.mu.Unlock() }()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
	}
	if fail {
		return provider.Result{}, errors.New("synthetic provider unavailable")
	}
	return provider.Result{Content: fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","body":%q}]}`, reply+" 기록")}, nil
}

func (m *concurrentModel) count(reply string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts[reply])
}

func concurrencySession(key string, replies ...string) *lifecycle.Session {
	s := lifecycle.NewSession("chat:" + key)
	var rows []lifecycle.Row
	for i, reply := range replies {
		// The next input explicitly recalls the preceding reply, so the
		// existing selector has relevant stored memory to put into KNOWN.
		rows = append(rows, row(fmt.Sprintf("u%d", i), "user", "", "input "+strings.Join(replies[:i+1], " ")+" 기록"), row(fmt.Sprintf("a%d", i), "char", fmt.Sprintf("g%d", i), reply))
		s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, rows...))
		next := append(append([]lifecycle.Row{}, rows...), row(fmt.Sprintf("u%d", i+1), "user", "", "next"))
		s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, next...))
	}
	return s
}

func concurrencySource(s *lifecycle.Session, turn int) memory.Source {
	return memory.Own(s)[turn].Source
}

func TestConcurrentChatsAndKnownOrder(t *testing.T) {
	db := testDB(t)
	m := concurrencyModel(map[string]string{"A-first": "A", "A-second": "A", "B-first": "B"})
	aGate, bGate := make(chan struct{}), make(chan struct{})
	m.gates["A-first"], m.gates["B-first"] = aGate, bGate
	w := NewWorker(db, m, "synthetic", time.Minute)
	runFor(t, w)
	a, b := concurrencySession("A", "A-first", "A-second"), concurrencySession("B", "B-first")
	notify(w, a)
	waitUntil(t, "blocked A call", func() bool { return m.count("A-first") == 1 })
	notify(w, b)
	waitUntil(t, "B while A is blocked", func() bool { return m.count("B-first") == 1 })
	notify(w, a) // queued same-chat notice cannot start a second runner
	d := w.Diagnostics()
	if d["generation_running_chats"] != 2 || fmt.Sprint(d["generation_running_sessions"]) != "[chat:A chat:B]" || !w.Chat(a.Key).Running || !w.Chat(b.Key).Running {
		t.Fatalf("active chats missing: %v", d)
	}
	if m.count("A-second") != 0 {
		t.Fatal("same chat's next turn ran before the first ended")
	}
	close(bGate)
	waitUntil(t, "B saved without A", func() bool { return a57Done(db, concurrencySource(b, 0)) })
	if a57Done(db, concurrencySource(a, 0)) {
		t.Fatal("test did not keep A blocked")
	}
	close(aGate)
	waitUntil(t, "A second saved", func() bool { return a57Done(db, concurrencySource(a, 1)) })
	waitUntil(t, "idle runners removed", func() bool { return w.Diagnostics()["generation_running_chats"] == 0 })
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.peak != 2 || m.peakChat["A"] != 1 || m.peakChat["B"] != 1 || len(m.prompts["A-first"]) != 1 || len(m.prompts["A-second"]) != 1 {
		t.Fatalf("calls/parallelism: peak=%d per_chat=%v prompts=%v", m.peak, m.peakChat, m.prompts)
	}
	if !strings.Contains(m.prompts["A-second"][0], "A-first 기록") {
		t.Fatalf("N+1 did not read stored N memory:\n%s", m.prompts["A-second"][0])
	}
	t.Logf("provider peak=%d; per-chat peak=%v; N+1 read stored N", m.peak, m.peakChat)
}

func TestConcurrentIndexAndBacklog(t *testing.T) {
	db := testDB(t)
	m := concurrencyModel(map[string]string{"A-first": "A", "A-second": "A", "B-first": "B"})
	aSecond := make(chan struct{})
	m.gates["A-second"] = aSecond
	w := NewWorker(db, m, "synthetic", time.Minute)
	a, b := concurrencySession("A", "A-first", "A-second"), concurrencySession("B", "B-first")
	first := concurrencySource(a, 0)
	j, err := db.JobFor(first)
	if err != nil {
		t.Fatal(err)
	}
	ja2, err := db.JobFor(concurrencySource(a, 1))
	if err != nil {
		t.Fatal(err)
	}
	jb, err := db.JobFor(concurrencySource(b, 0))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[int64]bool{j.ID: true, ja2.ID: true, jb.ID: true}
	indexGate, indexStarted := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	active, peak := 0, 0
	var calls [][]int64
	w.Index = func(ctx context.Context, jobs []int64) error {
		if len(jobs) != 1 || !allowed[jobs[0]] {
			t.Errorf("unexpected index jobs %v", jobs)
			return fmt.Errorf("unexpected index jobs %v", jobs)
		}
		if jobs[0] != j.ID {
			return nil
		}
		mu.Lock()
		active++
		peak = max(peak, active)
		calls = append(calls, append([]int64{}, jobs...))
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		once.Do(func() { close(indexStarted) })
		select {
		case <-indexGate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	runFor(t, w)
	notify(w, a)
	select {
	case <-indexStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("post-generation index did not start")
	}
	waitUntil(t, "same chat generation while index blocked", func() bool { return m.count("A-second") == 1 })
	notify(w, b)
	waitUntil(t, "other chat generation while index blocked", func() bool { return a57Done(db, concurrencySource(b, 0)) })
	w.Reindex(a.Key)
	if w.Diagnostics()["index_running_chats"].(int) < 1 {
		t.Fatal("index lane absent")
	}
	close(indexGate)
	waitUntil(t, "backlog index during A generation", func() bool { mu.Lock(); defer mu.Unlock(); return len(calls) == 2 })
	mu.Lock()
	if peak != 1 {
		t.Errorf("same job indexed concurrently: %d", peak)
	}
	mu.Unlock()
	close(aSecond)
	waitUntil(t, "A generation complete", func() bool { return a57Done(db, concurrencySource(a, 1)) })
}

func TestConcurrencyFiveLifecycleCasesThroughRun(t *testing.T) {
	db := testDB(t)
	m := concurrencyModel(map[string]string{"original": "A", "rerolled": "A", "edited": "A", "identical-row-new": "A", "normal-new": "A"})
	w := NewWorker(db, m, "synthetic", time.Minute)
	runFor(t, w)
	s := lifecycle.NewSession("chat:A")
	u := row("u1", "user", "", "same text")
	a := row("a1", "char", "g1", "original")
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, u, a))
	next := goOn(s, u, a)
	check := func(reply string, items int) {
		t.Helper()
		notify(w, s)
		waitUntil(t, reply+" stored", func() bool {
			got := bodies(t, db, s)
			if len(got) != items {
				return false
			}
			for _, list := range got {
				for _, body := range list {
					if body == reply+" 기록" {
						return true
					}
				}
			}
			return false
		})
		waitUntil(t, "chat idle", func() bool { return !w.Chat(s.Key).Running && !w.Chat(s.Key).Waiting })
	}
	check("original", 1)
	firstID := s.ActiveTurns()[0].ID
	// Same request retried: stable job, no additional provider attempt.
	s.Apply(next)
	notify(w, s)
	waitUntil(t, "retry drained", func() bool { return !w.Chat(s.Key).Running && !w.Chat(s.Key).Waiting })
	if m.count("original") != 1 || s.ActiveTurns()[0].ID != firstID {
		t.Fatal("same request appended or called again")
	}
	// Reroll of the same Host user row replaces its memory.
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, u))
	a = row("a2", "char", "g2", "rerolled")
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, u, a))
	goOn(s, u, a)
	check("rerolled", 1)
	if s.ActiveTurns()[0].ID != firstID {
		t.Fatal("reroll changed logical turn")
	}
	// Assistant deletion, edit same user row, then regenerate: replaces.
	u.Data = "edited input"
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, u))
	a = row("a3", "char", "g3", "edited")
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, u, a))
	goOn(s, u, a)
	check("edited", 1)
	if s.ActiveTurns()[0].ID != firstID {
		t.Fatal("edit/regenerate appended")
	}
	// Genuinely new Host user row with identical text appends.
	u2, a2 := row("u2", "user", "", u.Data), row("a4", "char", "g4", "identical-row-new")
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, u, a, u2, a2))
	goOn(s, u, a, u2, a2)
	check("identical-row-new", 2)
	// Normal new row appends and preserves both previous memories.
	u3, a3 := row("u3", "user", "", "different input"), row("a5", "char", "g5", "normal-new")
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, u, a, u2, a2, u3, a3))
	goOn(s, u, a, u2, a2, u3, a3)
	check("normal-new", 3)
	for reply := range m.want {
		if m.count(reply) != 1 {
			t.Fatalf("%s calls=%d", reply, m.count(reply))
		}
	}
	got := bodies(t, db, s)
	if got[firstID][0] != "edited 기록" {
		t.Fatalf("replacement was not retained: %v", got)
	}
}

func TestConcurrentScopedRetry(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 300*time.Millisecond)
	m := concurrencyModel(map[string]string{"failed": "A", "healthy": "B", "later": "C"})
	m.fail["failed"] = 1
	w := NewWorker(db, m, "synthetic", time.Minute)
	w.Retries = 0
	a, b, c := concurrencySession("A", "failed"), concurrencySession("B", "healthy"), concurrencySession("C", "later")
	runFor(t, w)
	notify(w, a)
	waitUntil(t, "A given up", func() bool { return w.Chat(a.Key).GivenUp == 1 && !w.Chat(a.Key).Running })
	notify(w, b)
	waitUntil(t, "B independent of A giveup", func() bool { return a57Done(db, concurrencySource(b, 0)) })
	var resets atomic.Int64
	w.RetryTurn(a.Key, concurrencySource(a, 0).Turn, func() error {
		if resets.Add(1) == 1 {
			return errors.New("synthetic scoped reset outage")
		}
		_, e := db.RearmFailedJobs()
		return e
	})
	waitUntil(t, "scoped reset first failure", func() bool { return resets.Load() == 1 })
	notify(w, c)
	waitUntil(t, "C during A scoped reset wait", func() bool { return a57Done(db, concurrencySource(c, 0)) })
	waitUntil(t, "A scoped reset recovery", func() bool { return a57Done(db, concurrencySource(a, 0)) })
	if m.count("failed") != 2 || m.count("healthy") != 1 || m.count("later") != 1 || resets.Load() != 2 {
		t.Fatal("scoped retry changed another chat or duplicated a call")
	}
}

func TestConcurrencyResetCancelsAndWaitsForEveryLane(t *testing.T) {
	db := testDB(t)
	m := concurrencyModel(map[string]string{"A": "A", "B": "B", "indexed": "I"})
	indexChat := concurrencySession("I", "indexed")
	w := NewWorker(db, m, "synthetic", time.Minute)
	indexStarted, indexCanceled, indexRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	w.Index = func(ctx context.Context, jobs []int64) error {
		if len(jobs) != 1 {
			return fmt.Errorf("unexpected jobs %v", jobs)
		}
		close(indexStarted)
		<-ctx.Done()
		close(indexCanceled)
		<-indexRelease
		return ctx.Err()
	}
	runFor(t, w)
	notify(w, indexChat)
	var releaseOnce sync.Once
	releaseIndex := func() { releaseOnce.Do(func() { close(indexRelease) }) }
	t.Cleanup(releaseIndex)
	select {
	case <-indexStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("index missing")
	}
	a, b := concurrencySession("A", "A"), concurrencySession("B", "B")
	gate := make(chan struct{})
	m.mu.Lock()
	m.gates["A"], m.gates["B"] = gate, gate
	m.mu.Unlock()
	notify(w, a)
	notify(w, b)
	waitUntil(t, "both generations active", func() bool { return m.count("A") == 1 && m.count("B") == 1 })
	resetDone := make(chan struct{})
	go func() { _ = w.Reset(func() error { w.Clear(); close(resetDone); return nil }) }()
	select {
	case <-indexCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not cancel index")
	}
	select {
	case <-resetDone:
		t.Fatal("reset ran before index stopped writing")
	case <-time.After(30 * time.Millisecond):
	}
	releaseIndex()
	select {
	case <-resetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not wait/cancel all generation runs")
	}
	d := w.Diagnostics()
	if d["generation_running_chats"] != 0 || d["index_running_chats"] != 0 || d["generation_waiting_chats"] != 0 {
		t.Fatalf("reset retained runtime: %v", d)
	}
}

func TestConcurrencyChatOperationWaitsForAllRunningChats(t *testing.T) {
	db := testDB(t)
	m := concurrencyModel(map[string]string{"A": "A", "B": "B"})
	aGate, bGate := make(chan struct{}), make(chan struct{})
	m.gates["A"], m.gates["B"] = aGate, bGate
	w := NewWorker(db, m, "synthetic", time.Minute)
	runFor(t, w)
	a, b := concurrencySession("A", "A"), concurrencySession("B", "B")
	notify(w, a)
	notify(w, b)
	waitUntil(t, "both calls", func() bool { return m.count("A") == 1 && m.count("B") == 1 })
	operated := make(chan struct{})
	go func() { _ = w.WithChatOperation(func() error { w.Forget(a.Key, nil); close(operated); return nil }) }()
	close(aGate)
	waitUntil(t, "A ended", func() bool { return !w.Chat(a.Key).Running })
	select {
	case <-operated:
		t.Fatal("chat operation overlapped other running chat")
	case <-time.After(30 * time.Millisecond):
	}
	close(bGate)
	select {
	case <-operated:
	case <-time.After(5 * time.Second):
		t.Fatal("chat operation never resumed")
	}
}

// Held answers must remain per source/job when another chat can progress.
type concurrentHeldStore struct {
	*store.DB
	mu      sync.Mutex
	blocked bool
}

func (s *concurrentHeldStore) KeepResponse(j store.Job, m, r, k, a string) error {
	s.mu.Lock()
	blocked := s.blocked && j.Session == "chat:A"
	s.mu.Unlock()
	if blocked {
		return errors.New("synthetic keep outage")
	}
	return s.DB.KeepResponse(j, m, r, k, a)
}
func (s *concurrentHeldStore) SaveGeneration(j store.Job, g store.Generation) ([]string, error) {
	s.mu.Lock()
	blocked := s.blocked && j.Session == "chat:A"
	s.mu.Unlock()
	if blocked {
		return nil, errors.New("synthetic save outage")
	}
	return s.DB.SaveGeneration(j, g)
}
func TestConcurrencyHeldAnswerRetryAndResume(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 150*time.Millisecond)
	st := &concurrentHeldStore{DB: db, blocked: true}
	m := concurrencyModel(map[string]string{"held": "A", "other": "B"})
	w := NewWorker(st, m, "synthetic", time.Minute)
	w.Retries = 0
	a, b := concurrencySession("A", "held"), concurrencySession("B", "other")
	save(w, a)
	save(w, b)
	if err := w.Resume(); err != nil {
		t.Fatal(err)
	}
	runFor(t, w)
	waitUntil(t, "held A and stored B", func() bool { return w.Chat(a.Key).RetryWaiting && a57Done(db, concurrencySource(b, 0)) })
	st.mu.Lock()
	st.blocked = false
	st.mu.Unlock()
	waitUntil(t, "held answer saved without new call", func() bool { return a57Done(db, concurrencySource(a, 0)) })
	if m.count("held") != 1 || m.count("other") != 1 {
		t.Fatal("held answer re-called or another chat duplicated")
	}
}

// Optional sample size changes only this synthetic workload. This is not a
// runtime setting, cap, or estimate of a long-session product memory peak.
func TestConcurrentRunnerMemory(t *testing.T) {
	n := 4
	if raw := os.Getenv("AC2_CONCURRENCY_MEASURE"); raw != "" {
		var err error
		n, err = strconv.Atoi(raw)
		if err != nil || n < 1 {
			t.Fatal("invalid test sample size")
		}
	}
	db := testDB(t)
	want := map[string]string{}
	var sessions []*lifecycle.Session
	gate := make(chan struct{})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("scale-%d", i)
		reply := fmt.Sprintf("reply-%d-", i) + strings.Repeat("synthetic continuity ", 100)
		want[reply] = key
		sessions = append(sessions, concurrencySession(key, reply))
	}
	m := concurrencyModel(want)
	for reply := range want {
		m.gates[reply] = gate
	}
	w := NewWorker(db, m, "synthetic", time.Minute)
	runFor(t, w)
	runtime.GC()
	var before, active, idle runtime.MemStats
	runtime.ReadMemStats(&before)
	g0 := runtime.NumGoroutine()
	for _, s := range sessions {
		notify(w, s)
	}
	waitUntil(t, "all active chats", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.active == n })
	runtime.GC()
	runtime.ReadMemStats(&active)
	g1 := runtime.NumGoroutine()
	close(gate)
	waitUntil(t, "all runner goroutines gone", func() bool { return w.Diagnostics()["generation_running_chats"] == 0 })
	runtime.GC()
	runtime.ReadMemStats(&idle)
	g2 := runtime.NumGoroutine()
	m.mu.Lock()
	peak := m.peak
	m.mu.Unlock()
	if peak != n {
		t.Fatalf("configured-independent calls capped: peak %d want %d", peak, n)
	}
	w.mu.Lock()
	retained := len(w.running) + len(w.indexing) + len(w.indexJobs) + len(w.indexQueue)
	w.mu.Unlock()
	if retained != 0 {
		t.Fatalf("idle execution retained %d", retained)
	}
	t.Logf("MEMORY chats=%d source_reply_bytes=%d heap_before=%d heap_active=%d heap_idle=%d stack_before=%d stack_active=%d stack_idle=%d goroutines_before=%d goroutines_active=%d goroutines_idle=%d peak_calls=%d", n, len(concurrencySource(sessions[0], 0).Responses[0].Data), before.HeapAlloc, active.HeapAlloc, idle.HeapAlloc, before.StackInuse, active.StackInuse, idle.StackInuse, g0, g1, g2, peak)
}

type sharedJobEmbedder struct {
	want    string
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (e *sharedJobEmbedder) Embed(ctx context.Context, texts []string, kind string) (provider.Embedding, error) {
	if kind != "document" || len(texts) != 1 || texts[0] != e.want {
		return provider.Embedding{}, fmt.Errorf("unexpected vector input %q %v", kind, texts)
	}
	if e.calls.Add(1) != 1 {
		return provider.Embedding{}, errors.New("duplicate embedding of shared job")
	}
	close(e.entered)
	select {
	case <-e.release:
	case <-ctx.Done():
		return provider.Embedding{}, ctx.Err()
	}
	return provider.Embedding{Vectors: [][]float32{{1, 0, 0}}}, nil
}
func (e *sharedJobEmbedder) EmbedGroups(context.Context, [][]string, string) (provider.GroupEmbedding, error) {
	return provider.GroupEmbedding{}, errors.New("unexpected contextual vector call")
}

func TestConcurrentSharedJobIndexReuse(t *testing.T) {
	db := testDB(t)
	s := concurrencySession("P", "shared source")
	save(NewWorker(db, nil, "", time.Minute), s)
	j, err := db.JobFor(concurrencySource(s, 0))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"shared synthetic memory"}]}`)
	if _, err = db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
		t.Fatal(err)
	}
	k, err := db.KnownOf([]int64{j.ID})
	if err != nil || len(k.Memories) != 1 {
		t.Fatal("stored fixture missing", err)
	}
	e := &sharedJobEmbedder{want: memory.DocText(k.Memories[0], nil), entered: make(chan struct{}), release: make(chan struct{})}
	w := NewWorker(db, nil, "", time.Minute)
	var indexes atomic.Int64
	w.Index = func(ctx context.Context, jobs []int64) error {
		if len(jobs) != 1 || jobs[0] != j.ID {
			t.Errorf("unexpected jobs %v", jobs)
			return errors.New("unexpected job")
		}
		_, err := embed.Index(ctx, db, e, "synthetic-flat", jobs, 64)
		if err != nil {
			t.Error(err)
		}
		indexes.Add(1)
		return err
	}
	runFor(t, w)
	var once sync.Once
	release := func() { once.Do(func() { close(e.release) }) }
	t.Cleanup(release)
	w.queueIndex("chat:P", []int64{j.ID})
	select {
	case <-e.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first index missing")
	}
	w.queueIndex("chat:branch", []int64{j.ID})
	waitUntil(t, "both index lanes", func() bool { return w.Diagnostics()["index_running_chats"] == 2 })
	release()
	waitUntil(t, "both index requests complete", func() bool { return indexes.Load() == 2 })
	if e.calls.Load() != 1 {
		t.Fatal("shared inherited job embedded twice")
	}
	t.Logf("index requests=%d, synthetic embedding calls=%d", indexes.Load(), e.calls.Load())
}

func TestConcurrentIndexRetryIsIndependent(t *testing.T) {
	db := testDB(t)
	shortWaits(t, time.Second)
	m := concurrencyModel(map[string]string{"first": "A", "second": "A", "healthy": "B"})
	w := NewWorker(db, m, "synthetic", time.Minute)
	a, b := concurrencySession("A", "first", "second"), concurrencySession("B", "healthy")
	j, err := db.JobFor(concurrencySource(a, 0))
	if err != nil {
		t.Fatal(err)
	}
	j2, err := db.JobFor(concurrencySource(a, 1))
	if err != nil {
		t.Fatal(err)
	}
	jb, err := db.JobFor(concurrencySource(b, 0))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[int64]bool{j.ID: true, j2.ID: true, jb.ID: true}
	var firstCalls atomic.Int64
	w.Index = func(ctx context.Context, jobs []int64) error {
		containsFirst := false
		for _, id := range jobs {
			if !allowed[id] {
				t.Error("unexpected index job", id)
				return errors.New("unexpected job")
			}
			if id == j.ID {
				containsFirst = true
			}
		}
		if containsFirst && firstCalls.Add(1) == 1 {
			return errors.New("synthetic vector outage")
		}
		return nil
	}
	runFor(t, w)
	notify(w, a)
	waitUntil(t, "failed index waiting", func() bool { return w.Diagnostics()["index_retry_waiting_chats"] == 1 })
	notify(w, b)
	waitUntil(t, "generation independent of index wait", func() bool { return a57Done(db, concurrencySource(a, 1)) && a57Done(db, concurrencySource(b, 0)) })
	waitUntil(t, "failed index filled without notice", func() bool {
		return firstCalls.Load() == 2 && w.Diagnostics()["index_backlog_chats"] == 0 && w.Diagnostics()["index_running_chats"] == 0
	})
	if m.count("first") != 1 || m.count("second") != 1 || m.count("healthy") != 1 {
		t.Fatal("index retry re-generated memory")
	}
}

func TestConcurrentGlobalRearmWaits(t *testing.T) {
	db := testDB(t)
	old := concurrencyModel(map[string]string{"A": "A", "B": "B"})
	aGate, bGate := make(chan struct{}), make(chan struct{})
	old.gates["A"], old.gates["B"] = aGate, bGate
	old.fail["A"], old.fail["B"] = 1, 1
	w := NewWorker(db, old, "old", time.Minute)
	w.Retries = 0
	a, b := concurrencySession("A", "A"), concurrencySession("B", "B")
	runFor(t, w)
	notify(w, a)
	notify(w, b)
	waitUntil(t, "both old calls", func() bool { return old.count("A") == 1 && old.count("B") == 1 })
	fixed := concurrencyModel(old.want)
	w.Configure(fixed, "new", 0)
	w.Rearm()
	close(aGate)
	waitUntil(t, "old A finished", func() bool { return !w.Chat(a.Key).Running })
	ja, err := db.JobFor(concurrencySource(a, 0))
	if err != nil {
		t.Fatal(err)
	}
	if ja.Attempts != 1 || ja.State != "failed" || fixed.count("A") != 0 {
		t.Fatal("global rearm ran before B's attempt counted")
	}
	close(bGate)
	waitUntil(t, "both rearmed calls saved", func() bool { return a57Done(db, concurrencySource(a, 0)) && a57Done(db, concurrencySource(b, 0)) })
	if fixed.count("A") != 1 || fixed.count("B") != 1 {
		t.Fatal("global rearm did not re-run both chats once")
	}
}

func TestConcurrentBackfillChats(t *testing.T) {
	db := testDB(t)
	m := concurrencyModel(map[string]string{"history A": "A", "history B": "B"})
	gate := make(chan struct{})
	m.gates["history A"] = gate
	w := NewWorker(db, m, "synthetic", time.Minute)
	a := a30Session("chat:A", row("u1", "user", "", "input"), row("a1", "char", "g1", "history A"))
	b := a30Session("chat:B", row("u1", "user", "", "input"), row("a1", "char", "g1", "history B"))
	save(w, a)
	save(w, b)
	for _, s := range []*lifecycle.Session{a, b} {
		asked, err := w.Backfill(s.Key, memory.Own(s), 0, false)
		if err != nil || len(asked) != 1 {
			t.Fatal("backfill grant missing", asked, err)
		}
	}
	runFor(t, w)
	waitUntil(t, "B backfill without A", func() bool { return m.count("history A") == 1 && a57Done(db, concurrencySource(b, 0)) })
	close(gate)
	waitUntil(t, "A backfill complete", func() bool { return a57Done(db, concurrencySource(a, 0)) })
	waitUntil(t, "backfill runs finished after grant cleanup", func() bool { return !w.Chat(a.Key).Running && !w.Chat(b.Key).Running })
	for _, s := range []*lifecycle.Session{a, b} {
		grants, err := db.Grants(s.Key)
		if err != nil || len(grants) != 0 {
			t.Fatal("backfill grant retained", grants, err)
		}
	}
}
