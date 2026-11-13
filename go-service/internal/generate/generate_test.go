package generate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Needs a disposable MariaDB: AC2_TEST_MARIADB_DSN=user@tcp(127.0.0.1:port)/
func testDB(t *testing.T) *store.DB {
	t.Helper()
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, err := sql.Open("mysql", base)
	if err != nil {
		t.Fatal(err)
	}
	name := "ac2_test_gen_" + regexp.MustCompile(`\W`).ReplaceAllString(t.Name(), "_")
	boot.Exec("DROP DATABASE IF EXISTS " + name)
	if _, err := boot.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE " + name); boot.Close() })
	db, err := store.Open(base + name + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeModel answers each prompt with one fact named after the turn text, and
// records what it was asked. It fails while fail is set and waits while
// hold is set.
type fakeModel struct {
	mu      sync.Mutex
	prompts []string
	fail    bool
	hold    chan struct{}
}

var turnLine = regexp.MustCompile(`(?m)^REPLY: (.*)$`)

func (f *fakeModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	fail, hold := f.fail, f.hold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if fail {
		return provider.Result{}, errors.New("provider down")
	}
	reply := turnLine.FindStringSubmatch(prompt)[1]
	return provider.Result{Content: fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","body":"%s 기록"}]}`, reply)}, nil
}

func (f *fakeModel) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func row(id, role, gen, data string) lifecycle.Row {
	return lifecycle.Row{ChatID: id, Role: role, GenerationID: gen, Data: data}
}

func snap(src lifecycle.Source, rows ...lifecycle.Row) lifecycle.Snapshot {
	for i := range rows {
		rows[i].Index = i
	}
	return lifecycle.Snapshot{SessionKey: "chat:A", Source: src, Rows: rows, TargetIndex: -1}
}

// bodies are the current memories of the chat's active turns, by turn.
func bodies(t *testing.T, db *store.DB, s *lifecycle.Session) map[int][]string {
	t.Helper()
	var srcs []memory.Source
	for _, p := range memory.Own(s) {
		srcs = append(srcs, p.Source)
	}
	jobs, err := db.CurrentJobs(srcs)
	if err != nil {
		t.Fatal(err)
	}
	k, err := db.KnownOf(jobs)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int][]string{}
	for _, m := range k.Memories {
		out[m.Turn] = append(out[m.Turn], m.Body)
	}
	return out
}

// run saves the chat as the observation path does before it notifies (a
// retry or a resume reads it from the store), then runs it.
func run(w *Worker, s *lifecycle.Session) {
	save(w, s)
	chatNow(w, s.Key, notice{turns: memory.Own(s)})
}

// chatNow runs the chat as Run does after taking its notice: a notice
// still waiting for the chat (from Notify or Backfill) is the one taken.
func chatNow(w *Worker, key string, n notice) {
	// Direct turn tests drain the production index queue before/after the
	// generation call. Concurrency tests below run the actual dispatcher.
	if w.takeBacklog(key) {
		w.mu.Lock()
		delete(w.indexRetry, key)
		w.mu.Unlock()
		w.indexScope(context.Background(), key, n)
	}
	drainIndexes(w)
	w.mu.Lock()
	if p, ok := w.pending[key]; ok {
		n = p
		delete(w.pending, key)
	}
	w.mu.Unlock()
	w.chat(context.Background(), key, n)
	drainIndexes(w)
}

func drainIndexes(w *Worker) {
	for {
		s, n, jobs, ok := w.nextIndex()
		if !ok {
			return
		}
		if jobs == nil {
			w.indexScope(context.Background(), s, n)
		} else {
			w.index(context.Background(), s, jobs)
		}
	}
}

func save(w *Worker, s *lifecycle.Session) {
	recordBranch(w.st, s)
	if st, ok := w.st.(interface {
		SaveSession(*lifecycle.Session, []lifecycle.Event, []lifecycle.Rename) error
	}); ok {
		if err := st.SaveSession(s, nil, nil); err != nil {
			panic(err)
		}
	}
}

// recordBranch records a branch's copies as the pipeline does after an
// observation (memory.RecordCopies), reading the parent from the store.
func recordBranch(st any, s *lifecycle.Session) {
	l, ok := st.(interface {
		LoadSession(string) (*lifecycle.Session, bool, error)
	})
	if !ok || s.BranchOf[0] == "" || s.CopiesOf == s.BranchOf {
		return
	}
	_, err := memory.RecordCopies(s, l.LoadSession)
	if err != nil {
		panic(err)
	}
}

// The five request-identity cases, at the generation level: a provider
// retry makes nothing new; a reroll of the same user row and an edit after
// deleting the reply both replace the turn's memory (the earlier job leaves
// current reads); a new user row with the same text and a normal new turn
// add a turn's memory and keep the earlier one.
func TestTurnChangesReplaceOrAppendMemory(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")

	// normal new turn
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "입력"), row("a1", "char", "g1", "첫 응답")))
	next := goOn(s, row("u1", "user", "", "입력"), row("a1", "char", "g1", "첫 응답"))
	run(w, s)
	if got := bodies(t, db, s); m.calls() != 1 || len(got) != 1 || got[1][0] != "첫 응답 기록" {
		t.Fatalf("new turn: calls %d %v", m.calls(), got)
	}
	// same-request provider retry: the same beforeRequest again (the same
	// next user row), no new turn, revision or call
	turns := len(s.ActiveTurns())
	s.Apply(next)
	run(w, s)
	if len(s.ActiveTurns()) != turns {
		t.Fatalf("retry added a turn")
	}
	if m.calls() != 1 {
		t.Fatalf("retry generated again: %d calls", m.calls())
	}
	// reroll of the same user row
	s.Apply(snap(lifecycle.SourceBeforeRequest, row("u1", "user", "", "입력")))
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "입력"), row("a2", "char", "g2", "다시 쓴 응답")))
	goOn(s, row("u1", "user", "", "입력"), row("a2", "char", "g2", "다시 쓴 응답"))
	run(w, s)
	if got := bodies(t, db, s); m.calls() != 2 || len(got) != 1 || len(got[1]) != 1 || got[1][0] != "다시 쓴 응답 기록" {
		t.Fatalf("reroll: calls %d %v", m.calls(), got)
	}
	// edit of the user row after deleting the reply, then regenerate
	s.Apply(snap(lifecycle.SourceBeforeRequest, row("u1", "user", "", "고친 입력")))
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "고친 입력"), row("a3", "char", "g3", "고친 뒤 응답")))
	goOn(s, row("u1", "user", "", "고친 입력"), row("a3", "char", "g3", "고친 뒤 응답"))
	run(w, s)
	if got := bodies(t, db, s); m.calls() != 3 || len(got) != 1 || len(got[1]) != 1 || got[1][0] != "고친 뒤 응답 기록" {
		t.Fatalf("edit+regenerate: calls %d %v", m.calls(), got)
	}
	// a new user row with the same text: a new turn
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "고친 입력"), row("a3", "char", "g3", "고친 뒤 응답"),
		row("u2", "user", "", "고친 입력"), row("a4", "char", "g4", "새 턴 응답")))
	goOn(s, row("u1", "user", "", "고친 입력"), row("a3", "char", "g3", "고친 뒤 응답"), row("u2", "user", "", "고친 입력"), row("a4", "char", "g4", "새 턴 응답"))
	run(w, s)
	got := bodies(t, db, s)
	second := s.ActiveTurns()[1].ID // the next inputs of the steps above used ids too
	if m.calls() != 4 || len(got) != 2 || got[1][0] != "고친 뒤 응답 기록" || len(got[second]) != 1 || got[second][0] != "새 턴 응답 기록" {
		t.Fatalf("same text new row: calls %d %v", m.calls(), got)
	}
}

// A turn not ready (no reply yet) is passed over and made when ready; a
// failed call is recorded on the job and made on a later run.
func TestNotReadyAndFailedTurnsAreMadeLater(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{fail: true}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	s.Apply(snap(lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘")))
	run(w, s)
	if m.calls() != 1 || w.failed != 1 {
		t.Fatalf("calls %d failed %d", m.calls(), w.failed)
	}
	src := memory.Own(s)[0].Source
	j, _ := db.JobFor(src)
	if j.State != "failed" {
		t.Fatalf("failed call left job %q", j.State)
	}
	m.fail = false
	// the next call of the failed turn waits RetryDelay(1); the clock is past it
	w.now = func() time.Time { return time.Now().Add(RetryDelay(1) + time.Second) }
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"))
	run(w, s)
	if got := bodies(t, db, s); m.calls() != 3 || len(got) != 2 {
		t.Fatalf("after recovery: calls %d %v", m.calls(), got)
	}
	if j2, _ := db.JobFor(src); j2.ID != j.ID || j2.State != "done" {
		t.Fatalf("retry did not reuse the job: %+v %+v", j, j2)
	}
}

// Notify returns at once while the model is still answering, so the
// observation path never waits for generation; the latest notice of a chat
// replaces one still waiting.
func TestNotifyDoesNotWaitForTheModel(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{hold: make(chan struct{})}
	w := NewWorker(db, m, "fake", time.Minute)
	defer running(t, w)()
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	w.Notify(s.Key, memory.Own(s))
	for m.calls() == 0 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	for i := 0; i < 3; i++ {
		w.Notify(s.Key, memory.Own(s))
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Notify waited %v while the model was answering", d)
	}
	w.mu.Lock()
	waiting := len(w.order)
	w.mu.Unlock()
	if waiting != 1 {
		t.Fatalf("notices of one chat queued %d times", waiting)
	}
	close(m.hold)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := bodies(t, db, s); len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn not made")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.calls() != 1 {
		t.Fatalf("the turn was generated %d times", m.calls())
	}
}

// A branch reads its parent's memory up to the branch point and makes only
// its own turns: its unchanged copy of a parent turn is not made again, the
// parent's turns after the branch point are not its past, and a copy it
// edited is its own turn.
func TestBranchInheritsParentUpToBranchPoint(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	at := func(key string, src lifecycle.Source, rows ...lifecycle.Row) lifecycle.Snapshot {
		sn := snap(src, rows...)
		sn.SessionKey = key
		return sn
	}
	parent := lifecycle.NewSession("chat:A")
	parent.Apply(at("chat:A", lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째")))
	parent.Apply(at("chat:A", lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"),
		row("u2", "user", "", "둘"), row("a2", "char", "g2", "부모 둘째")))
	parent.Apply(at("chat:A", lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"),
		row("u2", "user", "", "둘"), row("a2", "char", "g2", "부모 둘째"), row("u9", "user", "", "다음"))) // the next input
	if err := db.SaveSession(parent, parent.TakeEvents(), parent.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	run(w, parent)
	if m.calls() != 2 {
		t.Fatalf("parent: %d calls", m.calls())
	}
	marker := lifecycle.Row{ChatID: "mk", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::A::이름::a1::}}"}
	child := lifecycle.NewSession("chat:C")
	child.Apply(at("chat:C", lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"), marker,
		row("u3", "user", "", "셋"), row("a3", "char", "g3", "첫째 기록과 둘째 이야기")))
	goOn(child, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"), marker, row("u3", "user", "", "셋"), row("a3", "char", "g3", "첫째 기록과 둘째 이야기"))
	if child.BranchOf != [2]string{"chat:A", "c:a1"} {
		t.Fatalf("branch of %v", child.BranchOf)
	}
	run(w, child)
	if m.calls() != 3 {
		t.Fatalf("child made %d turns, want only its own", m.calls()-2)
	}
	p := m.prompts[2]
	if !strings.Contains(p, "부모 첫째 기록") || strings.Contains(p, "부모 둘째 기록") {
		t.Fatalf("child read the wrong parent turns:\n%s", p)
	}
	// the branch edits its copy of the parent turn: that copy is its own,
	// and as the turn the branch went on from (confirmed by its first
	// input) it is made again like any confirmed turn edited
	child.Apply(at("chat:C", lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "분기에서 고친 첫째"), marker,
		row("u3", "user", "", "셋"), row("a3", "char", "g3", "첫째 기록과 둘째 이야기")))
	run(w, child)
	if m.calls() != 4 || !strings.Contains(m.prompts[3], "분기에서 고친 첫째") {
		t.Fatalf("edited copy: %d calls", m.calls())
	}
	if asked, err := w.Backfill(child.Key, memory.Own(child), 0, false); err != nil || len(asked) != 0 {
		t.Fatalf("asked %v %v", asked, err)
	}
}

// Each made turn is indexed; after an indexing failure the chat's next run
// first indexes every current job (so what failed is filled), and the
// backlog clears once that succeeds.
func TestIndexingFailureIsFilledOnTheNextRun(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	var calls [][]int64
	fail := true
	w.Index = func(ctx context.Context, jobs []int64) error {
		calls = append(calls, append([]int64(nil), jobs...))
		if fail {
			return errors.New("vectors down")
		}
		return nil
	}
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	if len(calls) != 1 || len(calls[0]) != 1 || !w.backlog[s.Key] {
		t.Fatalf("first run: %v backlog %v", calls, w.backlog)
	}
	fail = false
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"))
	run(w, s)
	// the failed job is filled first (the index asked before the run), then
	// the new turn's job is indexed as it is made
	if len(calls) != 3 || len(calls[1]) != 1 || calls[1][0] != calls[0][0] || len(calls[2]) != 1 || calls[2][0] == calls[0][0] || w.backlog[s.Key] {
		t.Fatalf("second run: %v backlog %v", calls, w.backlog)
	}
	run(w, s)
	if len(calls) != 3 {
		t.Fatalf("nothing new was indexed again: %v", calls)
	}
}

// saveFailsOnce is the real store whose first SaveGeneration fails, as a
// database error after the answer was received would.
type saveFailsOnce struct {
	*store.DB
	failed bool
}

func (s *saveFailsOnce) SaveGeneration(j store.Job, g store.Generation) ([]string, error) {
	if !s.failed {
		s.failed = true
		return nil, errors.New("database down")
	}
	return s.DB.SaveGeneration(j, g)
}

// An answer received but not stored is kept: the next run stores it without
// calling the model again, once, with each new item's id recorded by its
// place in the answer.
func TestReceivedAnswerIsStoredWithoutCallingAgain(t *testing.T) {
	db := testDB(t)
	st := &saveFailsOnce{DB: db}
	m := &fakeModel{}
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	src := memory.Own(s)[0].Source
	if _, err := Turn(context.Background(), st, m, "fake", nil, src); err == nil {
		t.Fatal("the failed store was not reported")
	}
	before, _ := db.JobFor(src)
	if before.State != "received" || before.Response == "" || m.calls() != 1 || before.Attempts != 1 {
		t.Fatalf("after the failed store: job %+v calls %d", before, m.calls())
	}
	r, err := Turn(context.Background(), st, m, "fake", nil, src)
	if err != nil || !r.Reused || m.calls() != 1 {
		t.Fatalf("second run: %+v %v calls %d", r, err, m.calls())
	}
	if r, err = Turn(context.Background(), st, m, "fake", nil, src); err != nil || !r.Done || m.calls() != 1 {
		t.Fatalf("third run: %+v %v calls %d", r, err, m.calls())
	}
	if got := bodies(t, db, s); len(got[1]) != 1 || got[1][0] != "첫째 기록" {
		t.Fatalf("stored %v", got)
	}
	// storing the kept answer is no model call: the calls counted stay
	j, _ := db.JobFor(src)
	if j.Attempts != before.Attempts {
		t.Fatalf("attempts %d, before the reuse %d", j.Attempts, before.Attempts)
	}
	ids, err := db.ItemIDs(j.ID)
	if err != nil || len(ids) != 1 || !strings.HasPrefix(ids["memories/0"], "M") {
		t.Fatalf("item ids %v %v", ids, err)
	}
}

// saveLosesReply stores for real but reports a failure, as a lost commit
// reply would.
type saveLosesReply struct{ *store.DB }

func (s saveLosesReply) SaveGeneration(j store.Job, g store.Generation) ([]string, error) {
	if _, err := s.DB.SaveGeneration(j, g); err != nil {
		return nil, err
	}
	return nil, errors.New("connection lost after commit")
}

// A store that committed but whose reply was lost is found done on the next
// run: nothing is called or stored again.
func TestCommittedStoreWithLostReplyIsNotRedone(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	src := memory.Own(s)[0].Source
	if _, err := Turn(context.Background(), saveLosesReply{db}, m, "fake", nil, src); err == nil {
		t.Fatal("the lost reply was not reported")
	}
	if r, err := Turn(context.Background(), db, m, "fake", nil, src); err != nil || !r.Done || m.calls() != 1 {
		t.Fatalf("next run: %+v %v calls %d", r, err, m.calls())
	}
	if got := bodies(t, db, s); len(got[1]) != 1 {
		t.Fatalf("stored %v", got)
	}
}

// writesFailOnce is the real store whose first answer keeping and first
// store both fail, as one database outage would.
type writesFailOnce struct {
	*store.DB
	keepFailed, saveFailed bool
}

func (s *writesFailOnce) KeepResponse(j store.Job, model, request, readItems, response string) error {
	if !s.keepFailed {
		s.keepFailed = true
		return errors.New("database down")
	}
	return s.DB.KeepResponse(j, model, request, readItems, response)
}

func (s *writesFailOnce) SaveGeneration(j store.Job, g store.Generation) ([]string, error) {
	if !s.saveFailed {
		s.saveFailed = true
		return nil, errors.New("database down")
	}
	return s.DB.SaveGeneration(j, g)
}

// When keeping the answer and storing it both fail, the worker holds the
// answer it received and stores it on the next run without calling the
// model again; the failure notes are reported.
func TestWorkerHoldsAnAnswerWhoseStoresFailed(t *testing.T) {
	db := testDB(t)
	st := &writesFailOnce{DB: db}
	m := &fakeModel{}
	w := NewWorker(st, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	src := memory.Own(s)[0].Source
	if j, _ := db.JobFor(src); m.calls() != 1 || w.failed != 1 || j.State == "done" || j.State == "received" {
		t.Fatalf("after the outage: calls %d failed %d job %q", m.calls(), w.failed, j.State)
	}
	run(w, s)
	if got := bodies(t, db, s); m.calls() != 1 || len(got[1]) != 1 {
		t.Fatalf("after recovery: calls %d stored %v", m.calls(), got)
	}
	if len(w.held) != 0 {
		t.Fatalf("held answers left: %v", w.held)
	}
}

// A job that keeps failing gets 1 + Retries model calls, however many
// notices come, also across a new worker (a restart); the calls are counted
// as they start.
func TestFailedCallsStopAtTheRetryLimit(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{fail: true}
	later := func() time.Time { return time.Now().Add(24 * time.Hour) } // every wait is over
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	w := NewWorker(db, m, "fake", time.Minute)
	w.now = later
	for i := 0; i < 2; i++ {
		run(w, s)
	}
	w = NewWorker(db, m, "fake", time.Minute) // restart
	w.now = later
	for i := 0; i < 5; i++ {
		run(w, s)
	}
	if m.calls() != 1+3 || w.Diagnostics()["generation_given_up_turns"] != 1 {
		t.Fatalf("calls %d diag %v", m.calls(), w.Diagnostics())
	}
	if j, _ := db.JobFor(memory.Own(s)[0].Source); j.Attempts != 4 {
		t.Fatalf("attempts %d", j.Attempts)
	}
	// a smaller setting is used as it is
	m2 := &fakeModel{fail: true}
	s2 := lifecycle.NewSession("chat:B")
	sn := snap(lifecycle.SourceOutputEvent, row("v1", "user", "", "다른"), row("b1", "char", "h1", "답"))
	sn.SessionKey = "chat:B"
	s2.Apply(sn)
	goOn(s2, row("v1", "user", "", "다른"), row("b1", "char", "h1", "답"))
	w2 := NewWorker(db, m2, "fake", time.Minute)
	w2.Retries, w2.now = 1, later
	for i := 0; i < 4; i++ {
		run(w2, s2)
	}
	if m2.calls() != 2 {
		t.Fatalf("Retries=1: calls %d", m2.calls())
	}
}

// After a failed call, a notice before the wait is over calls nothing; the
// worker calls again by itself when the wait is over, with no new notice.
func TestFailedCallWaitsAndRetriesWithoutANotice(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 300*time.Millisecond)
	m := &fakeModel{fail: true}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	run(w, s) // within the wait
	if m.calls() != 1 {
		t.Fatalf("called within the wait: %d", m.calls())
	}
	m.mu.Lock()
	m.fail = false
	m.mu.Unlock()
	runFor(t, w)
	waitFor(t, func() bool { return len(bodies(t, db, s)[1]) == 1 })
	if got := bodies(t, db, s); len(got[1]) != 1 || m.calls() != 2 {
		t.Fatalf("no retry without a notice: calls %d %v", m.calls(), got)
	}
}

// shortWaits makes RetryDelays short for a test and gives them back.
func shortWaits(t *testing.T, d time.Duration) {
	saved := RetryDelays
	RetryDelays = []time.Duration{d}
	t.Cleanup(func() { RetryDelays = saved })
}

// runFor runs the worker until the test ends and waits for it to stop.
func runFor(t *testing.T, w *Worker) {
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
}

func waitFor(t *testing.T, cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// The last allowed call answers but storing fails: the answer is stored
// later without a notice and without another call.
func TestLastCallsAnswerIsStoredWithoutANotice(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 100*time.Millisecond)
	m := &fakeModel{}
	w := NewWorker(&saveFailsOnce{DB: db}, m, "fake", time.Minute)
	w.Retries = 0
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	runFor(t, w)
	if !waitFor(t, func() bool { return len(bodies(t, db, s)[1]) == 1 }) || m.calls() != 1 {
		t.Fatalf("calls %d stored %v", m.calls(), bodies(t, db, s))
	}
}

// After a reroll replaced a failed reply, the old reply's wait does not call
// the model for it again.
func TestRerollDropsTheOldReplysRetry(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 150*time.Millisecond)
	m := &fakeModel{fail: true}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	old := memory.Own(s)[0].Source
	run(w, s)
	m.mu.Lock()
	m.fail = false
	m.mu.Unlock()
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g2", "다른 답")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g2", "다른 답"))
	save(w, s)
	w.Notify(s.Key, memory.Own(s))
	runFor(t, w)
	if !waitFor(t, func() bool { return len(bodies(t, db, s)[1]) == 1 }) {
		t.Fatalf("reroll not made: %v", bodies(t, db, s))
	}
	time.Sleep(400 * time.Millisecond) // past the old wait
	if j, _ := db.JobFor(old); m.calls() != 2 || j.State == "done" {
		t.Fatalf("old reply called again: calls %d old job %q", m.calls(), j.State)
	}
}

// A newer notice is the chat's latest observation: a retry that a run of
// the older notice set up afterwards does not put the older one back.
func TestDueRetryKeepsTheNewerNotice(t *testing.T) {
	w := NewWorker(nil, nil, "fake", time.Minute)
	older := notice{turns: []memory.ScopedTurn{{Source: memory.Source{Turn: 1}}}}
	newer := notice{turns: []memory.ScopedTurn{{Source: memory.Source{Turn: 2}}}}
	w.Notify("chat:A", newer.turns)
	w.waitRetry("chat:A", older, time.Now().Add(-time.Second))
	w.retryDue()
	_, n, ok := w.next(true)
	if !ok || len(n.turns) != 1 || n.turns[0].Source.Turn != 2 {
		t.Fatalf("pending %+v %v", n, ok)
	}
	if _, _, ok := w.next(true); ok {
		t.Fatal("the older notice came back")
	}
}

// doneKeysFailsOnce is the real store whose first DoneKeys fails.
type doneKeysFailsOnce struct {
	*store.DB
	failed bool
}

func (s *doneKeysFailsOnce) DoneKeys(session string) (map[string]bool, error) {
	if !s.failed {
		s.failed = true
		return nil, errors.New("database down")
	}
	return s.DB.DoneKeys(session)
}

// A read error when a chat runs keeps it waiting to run
// again.
func TestReadErrorKeepsTheChatWaiting(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 100*time.Millisecond)
	m := &fakeModel{}
	w := NewWorker(&doneKeysFailsOnce{DB: db}, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	runFor(t, w)
	if !waitFor(t, func() bool { return len(bodies(t, db, s)[1]) == 1 }) || m.calls() != 1 {
		t.Fatalf("calls %d stored %v", m.calls(), bodies(t, db, s))
	}
}

// slowFail fails each call after taking d.
type slowFail struct {
	d     time.Duration
	calls int
}

func (f *slowFail) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	f.calls++
	time.Sleep(f.d)
	return provider.Result{}, errors.New("provider down")
}

// The wait counts from the failure, not from the call's start; with calls
// used up the turn is reported as given up at once.
func TestWaitCountsFromTheFailure(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 400*time.Millisecond)
	p := &slowFail{d: 250 * time.Millisecond}
	w := NewWorker(db, p, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	j, _ := db.JobFor(memory.Own(s)[0].Source)
	if left := time.Until(j.NextAt); left < 300*time.Millisecond {
		t.Fatalf("wait left after the failure: %v", left)
	}
	w2 := NewWorker(db, p, "fake", time.Minute)
	w2.Retries = 0
	s2 := lifecycle.NewSession("chat:B")
	sn := snap(lifecycle.SourceOutputEvent, row("v1", "user", "", "다른"), row("b1", "char", "h1", "답"))
	sn.SessionKey = "chat:B"
	s2.Apply(sn)
	goOn(s2, row("v1", "user", "", "다른"), row("b1", "char", "h1", "답"))
	run(w2, s2)
	if d := w2.Diagnostics(); d["generation_given_up_turns"] != 1 || d["generation_retry_waiting_chats"] != 0 {
		t.Fatalf("diag after the last call: %v", d)
	}
}

// A run of the chat's latest notice decides its waits again: after it made
// every turn, no wait is left to wake the worker for nothing.
func TestRunLeavesNoWaitWhenAllIsMade(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	w.waitRetry(s.Key, notice{turns: memory.Own(s)}, time.Now().Add(time.Hour))
	run(w, s)
	if d := w.Diagnostics(); d["generation_retry_waiting_chats"] != 0 || m.calls() != 1 {
		t.Fatalf("diag %v calls %d", d, m.calls())
	}
}

// scriptModel answers with its replies in order (the last repeats); an
// empty reply is a provider failure.
type scriptModel struct {
	mu      sync.Mutex
	replies []string
	n       int
}

func (f *scriptModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.n
	if i >= len(f.replies) {
		i = len(f.replies) - 1
	}
	f.n++
	if f.replies[i] == "" {
		return provider.Result{}, errors.New("provider down")
	}
	return provider.Result{Content: f.replies[i]}, nil
}

func (f *scriptModel) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// synthetic answers shaped like the two stored ones that could not be read:
// prose only, and the contract's keys written in other words
const proseAnswer = "1. 新的内容：主角去了市场，买了米。没有 JSON。"
const otherKeysAnswer = `{"엔티티":[],"기억":[{"id":"m1","kind":"event","본문":"시장에 갔다"}]}`
const goodAnswer = `{"memories":[{"id":"m1","kind":"fact","body":"다시 만든 기록"}]}`

// An answer with no response object is kept and made again after a wait,
// without a notice; the stored job notes the earlier answer.
func TestUnreadableAnswerIsMadeAgain(t *testing.T) {
	for _, bad := range []string{proseAnswer, otherKeysAnswer} {
		t.Run("", func(t *testing.T) {
			db := testDB(t)
			shortWaits(t, 100*time.Millisecond)
			m := &scriptModel{replies: []string{bad, goodAnswer}}
			w := NewWorker(db, m, "fake", time.Minute)
			s := lifecycle.NewSession("chat:A")
			s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
			goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
			run(w, s)
			src := memory.Own(s)[0].Source
			if j, _ := db.JobFor(src); j.State != "unreadable" || j.Response != bad || m.calls() != 1 {
				t.Fatalf("after the unreadable answer: %+v calls %d", j, m.calls())
			}
			runFor(t, w)
			if !waitFor(t, func() bool { return len(bodies(t, db, s)[1]) == 1 }) || m.calls() != 2 {
				t.Fatalf("not made again: calls %d %v", m.calls(), bodies(t, db, s))
			}
		})
	}
}

// Unreadable answers use the job's call limit like failed calls; with the
// calls used up the last answer stays kept and the turn is given up.
func TestUnreadableAnswersUseTheCallLimit(t *testing.T) {
	db := testDB(t)
	m := &scriptModel{replies: []string{"", proseAnswer}} // a failed call, then unreadable answers
	w := NewWorker(db, m, "fake", time.Minute)
	w.Retries, w.now = 2, func() time.Time { return time.Now().Add(24 * time.Hour) }
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	for i := 0; i < 6; i++ {
		run(w, s)
	}
	j, _ := db.JobFor(memory.Own(s)[0].Source)
	if m.calls() != 3 || j.Attempts != 3 || j.State != "unreadable" || j.Response != proseAnswer || w.Diagnostics()["generation_given_up_turns"] != 1 {
		t.Fatalf("calls %d job %+v diag %v", m.calls(), j, w.Diagnostics())
	}
}

// "{}" (nothing new this turn) is an answer: stored at once, not made again.
func TestEmptyAnswerIsNotMadeAgain(t *testing.T) {
	db := testDB(t)
	m := &scriptModel{replies: []string{"{}"}}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	run(w, s)
	if j, _ := db.JobFor(memory.Own(s)[0].Source); j.State != "done" || m.calls() != 1 {
		t.Fatalf("job %q calls %d", j.State, m.calls())
	}
}

// A kept unreadable answer that the parser can read now (a later parser) is
// stored without calling the model.
func TestUnreadableAnswerIsReadAgainFirst(t *testing.T) {
	db := testDB(t)
	m := &scriptModel{replies: []string{goodAnswer}}
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	src := memory.Own(s)[0].Source
	j, _ := db.JobFor(src)
	// as an older parser left it: kept as unreadable, readable now
	if err := db.KeepUnreadable(j, "old-model", "the request", "", goodAnswer, []string{memory.NoResponse}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := Turn(context.Background(), db, m, "fake", nil, src)
	if err != nil || !r.Reused || m.calls() != 0 || len(bodies(t, db, s)[1]) != 1 {
		t.Fatalf("%+v %v calls %d", r, err, m.calls())
	}
}

// failsOnce is the real store whose named writes fail once each.
type failsOnce struct {
	*store.DB
	mu   sync.Mutex
	fail map[string]bool
}

func (s *failsOnce) once(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail[name] {
		delete(s.fail, name)
		return errors.New(name + ": database down")
	}
	return nil
}

func (s *failsOnce) StartAttempt(j store.Job) error {
	if err := s.once("start"); err != nil {
		return err
	}
	return s.DB.StartAttempt(j)
}

func (s *failsOnce) KeepResponse(j store.Job, model, request, readItems, response string) error {
	if err := s.once("keep"); err != nil {
		return err
	}
	return s.DB.KeepResponse(j, model, request, readItems, response)
}

func (s *failsOnce) KeepUnreadable(j store.Job, model, request, readItems, response string, notes []string, retryAt time.Time) error {
	if err := s.once("unreadable"); err != nil {
		return err
	}
	return s.DB.KeepUnreadable(j, model, request, readItems, response, notes, retryAt)
}

func (s *failsOnce) SaveGeneration(j store.Job, g store.Generation) ([]string, error) {
	if err := s.once("save"); err != nil {
		return nil, err
	}
	return s.DB.SaveGeneration(j, g)
}

func oneTurn() *lifecycle.Session {
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	return s
}

// A database error before the first call (no call counted yet) does not
// stop the worker; the turn is made later without a notice
// (RetryDelay(0) used to panic).
func TestErrorBeforeTheFirstCallIsRetried(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 100*time.Millisecond)
	m := &scriptModel{replies: []string{goodAnswer}}
	w := NewWorker(&failsOnce{DB: db, fail: map[string]bool{"start": true}}, m, "fake", time.Minute)
	s := oneTurn()
	run(w, s)
	runFor(t, w)
	if !waitFor(t, func() bool { return len(bodies(t, db, s)[1]) == 1 }) || m.calls() != 1 {
		t.Fatalf("calls %d %v", m.calls(), bodies(t, db, s))
	}
}

// A kept unreadable answer that reads now is stored even when the job's
// calls are used up: reading it needs no call (§2-3).
func TestReadAgainAtTheCallLimit(t *testing.T) {
	db := testDB(t)
	m := &scriptModel{replies: []string{goodAnswer}}
	s := oneTurn()
	src := memory.Own(s)[0].Source
	j, _ := db.JobFor(src)
	db.StartAttempt(j)
	if err := db.KeepUnreadable(j, "old-model", "the request", "", goodAnswer, []string{memory.NoResponse}, time.Now()); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(db, m, "fake", time.Minute)
	w.Retries = 0
	run(w, s)
	if len(bodies(t, db, s)[1]) != 1 || m.calls() != 0 {
		t.Fatalf("calls %d %v diag %v", m.calls(), bodies(t, db, s), w.Diagnostics())
	}
}

// Until an unreadable answer is recorded it stays in hand: when recording
// fails (alone, or with keeping the answer), the next run records it
// without a call, and the answer is kept (§2-4).
func TestUnreadableAnswerStaysInHandUntilRecorded(t *testing.T) {
	for _, fail := range []map[string]bool{{"unreadable": true}, {"keep": true, "unreadable": true}} {
		t.Run("", func(t *testing.T) {
			db := testDB(t)
			m := &scriptModel{replies: []string{proseAnswer}}
			w := NewWorker(&failsOnce{DB: db, fail: fail}, m, "fake", time.Minute)
			w.Retries, w.now = 0, func() time.Time { return time.Now().Add(24 * time.Hour) }
			s := oneTurn()
			run(w, s)
			run(w, s)
			j, _ := db.JobFor(memory.Own(s)[0].Source)
			got, _ := db.UnreadAnswers(j.ID)
			if m.calls() != 1 || j.State != "unreadable" || len(got) != 1 || got[0] != proseAnswer || len(w.held) != 0 {
				t.Fatalf("calls %d job %q unread %v held %d", m.calls(), j.State, got, len(w.held))
			}
		})
	}
}

// An unreadable answer is kept however the job goes on: a failed call or a
// failed store after it, then a good answer (§2-5).
func TestUnreadableAnswerIsKeptThroughLaterSteps(t *testing.T) {
	cases := []struct {
		replies []string
		fail    map[string]bool
	}{
		{[]string{proseAnswer, "", goodAnswer}, nil},                        // a failed call in between
		{[]string{proseAnswer, goodAnswer}, map[string]bool{"save": false}}, // store fails after the good answer (set below)
	}
	for i, c := range cases {
		t.Run("", func(t *testing.T) {
			db := testDB(t)
			m := &scriptModel{replies: c.replies}
			st := &failsOnce{DB: db, fail: map[string]bool{}}
			w := NewWorker(st, m, "fake", time.Minute)
			w.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
			s := oneTurn()
			run(w, s) // unreadable
			if i == 1 {
				st.fail["save"] = true
			}
			for k := 0; k < 4; k++ {
				run(w, s)
			}
			j, _ := db.JobFor(memory.Own(s)[0].Source)
			got, _ := db.UnreadAnswers(j.ID)
			if j.State != "done" || len(got) != 1 || got[0] != proseAnswer || len(bodies(t, db, s)[1]) != 1 {
				t.Fatalf("job %q unread %v stored %v calls %d", j.State, got, bodies(t, db, s), m.calls())
			}
		})
	}
}

// Given up is decided again by each run: after a reroll replaced the turn
// whose calls were used up, it is no longer counted (§3).
func TestGivenUpIsClearedAfterAReroll(t *testing.T) {
	db := testDB(t)
	m := &scriptModel{replies: []string{"", goodAnswer}}
	w := NewWorker(db, m, "fake", time.Minute)
	w.Retries, w.now = 0, func() time.Time { return time.Now().Add(24 * time.Hour) }
	s := oneTurn()
	run(w, s)
	if w.Diagnostics()["generation_given_up_turns"] != 1 {
		t.Fatalf("not given up: %v", w.Diagnostics())
	}
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g2", "다른 답")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g2", "다른 답"))
	run(w, s)
	if d := w.Diagnostics(); d["generation_given_up_turns"] != 0 || len(bodies(t, db, s)[1]) != 1 {
		t.Fatalf("diag %v stored %v", d, bodies(t, db, s))
	}
}

// history is a chat opened with three turns of earlier history (loaded, not
// confirmed by an output event), saved as the observation path saves it.
func history(t *testing.T, db *store.DB) *lifecycle.Session {
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"),
		row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"), row("u3", "user", "", "셋"), row("a3", "char", "g3", "셋째")))
	if err := db.SaveSession(s, nil, nil); err != nil {
		t.Fatal(err)
	}
	return s
}

// History turns the user asked for are kept in the store: after a restart
// they are made without a new notice or request; the others are not.
func TestAskedHistoryIsMadeAfterARestart(t *testing.T) {
	db := testDB(t)
	s := history(t, db)
	m := &fakeModel{}
	w1 := NewWorker(db, m, "fake", time.Minute)
	if asked, err := w1.Backfill(s.Key, memory.Own(s), 2, false); err != nil || len(asked) != 2 {
		t.Fatalf("asked %v %v", asked, err)
	}
	// restart before the worker ran
	w2 := NewWorker(db, m, "fake", time.Minute)
	if err := w2.Resume(); err != nil {
		t.Fatal(err)
	}
	runFor(t, w2)
	// the grant is released after the memories are stored, so wait for both
	// before checking them
	if !waitFor(t, func() bool { g, _ := db.Grants(s.Key); return len(bodies(t, db, s)) == 2 && len(g) == 0 }) {
		g, _ := db.Grants(s.Key)
		t.Fatalf("made %v grants left %v", bodies(t, db, s), g)
	}
	got := bodies(t, db, s)
	g, _ := db.Grants(s.Key)
	if len(got[1]) != 1 || len(got[2]) != 1 || len(got[3]) != 0 || m.calls() != 2 || len(g) != 0 {
		t.Fatalf("made %v calls %d grants left %v", got, m.calls(), g)
	}
}

// After a restart the worker goes on by itself: a confirmed turn never
// made, and an answer received but not stored (stored without a call).
func TestResumeMakesWhatIsLeftWithoutANotice(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	s.Apply(snap(lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘")))
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"))
	if own := memory.Own(s); !own[0].Confirmed || !own[1].Confirmed {
		t.Fatal("setup: both turns should be confirmed")
	}
	if err := db.SaveSession(s, nil, nil); err != nil {
		t.Fatal(err)
	}
	own := memory.Own(s)
	j, _ := db.JobFor(own[0].Source) // turn 1: answer received before the stop
	if err := db.KeepResponse(j, "fake", "the request", "", `{"memories":[{"id":"m1","kind":"fact","body":"받아 둔 기록"}]}`); err != nil {
		t.Fatal(err)
	}
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	if err := w.Resume(); err != nil {
		t.Fatal(err)
	}
	runFor(t, w)
	if !waitFor(t, func() bool { return len(bodies(t, db, s)) == 2 }) {
		t.Fatalf("made %v", bodies(t, db, s))
	}
	got := bodies(t, db, s)
	if got[1][0] != "받아 둔 기록" || m.calls() != 1 {
		t.Fatalf("made %v calls %d (turn 1 needs no call)", got, m.calls())
	}
}

// A restart does not make history no one asked for.
func TestResumeDoesNotMakeUnaskedHistory(t *testing.T) {
	db := testDB(t)
	history(t, db)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	if err := w.Resume(); err != nil {
		t.Fatal(err)
	}
	runFor(t, w)
	time.Sleep(300 * time.Millisecond)
	if m.calls() != 0 {
		t.Fatalf("calls %d", m.calls())
	}
}

// After a restart, memory stored without vectors (the stop came before
// indexing) is indexed; nothing is generated again.
func TestResumeIndexesMemoryWithoutVectors(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	m := &fakeModel{}
	run(NewWorker(db, m, "fake", time.Minute), s) // made, no Index set: no vectors
	var mu sync.Mutex
	var indexed []int64
	w := NewWorker(db, m, "fake", time.Minute)
	w.Index = func(ctx context.Context, jobs []int64) error {
		mu.Lock()
		defer mu.Unlock()
		indexed = append(indexed, jobs...)
		return nil
	}
	if err := w.Resume(); err != nil {
		t.Fatal(err)
	}
	runFor(t, w)
	j, _ := db.JobFor(memory.Own(s)[0].Source)
	ok := waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(indexed) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if !ok || indexed[0] != j.ID || m.calls() != 1 {
		t.Fatalf("indexed %v (job %d) calls %d", indexed, j.ID, m.calls())
	}
}

// ungrantFailsOnce is the real store whose first Ungrant fails.
type ungrantFailsOnce struct {
	*store.DB
	failed bool
}

func (s *ungrantFailsOnce) Ungrant(session string, src memory.Source) error {
	if !s.failed {
		s.failed = true
		return errors.New("database down")
	}
	return s.DB.Ungrant(session, src)
}

// A grant whose turn is already made is dropped on the next run, without a
// new call.
func TestGrantOfAMadeTurnIsDropped(t *testing.T) {
	db := testDB(t)
	s := history(t, db)
	m := &fakeModel{}
	w := NewWorker(&ungrantFailsOnce{DB: db}, m, "fake", time.Minute)
	if _, err := w.Backfill(s.Key, memory.Own(s), 1, false); err != nil {
		t.Fatal(err)
	}
	run(w, s) // made; dropping the grant fails
	if g, _ := db.Grants(s.Key); len(g) != 1 {
		t.Fatalf("setup: grants %v", g)
	}
	run(w, s)
	if g, _ := db.Grants(s.Key); len(g) != 0 || m.calls() != 1 {
		t.Fatalf("grants %v calls %d", g, m.calls())
	}
}

// keysFailOnce is the real store whose first SessionKeys fails.
type keysFailOnce struct {
	*store.DB
	failed bool
}

func (s *keysFailOnce) SessionKeys() ([]string, error) {
	if !s.failed {
		s.failed = true
		return nil, errors.New("database down")
	}
	return s.DB.SessionKeys()
}

// A Resume whose chat list could not be read is tried again by the worker
// (§3-4).
func TestFailedResumeIsTriedAgain(t *testing.T) {
	db := testDB(t)
	shortWaits(t, 100*time.Millisecond)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	if err := db.SaveSession(s, nil, nil); err != nil {
		t.Fatal(err)
	}
	m := &fakeModel{}
	w := NewWorker(&keysFailOnce{DB: db}, m, "fake", time.Minute)
	if err := w.Resume(); err == nil {
		t.Fatal("the failed read was not reported")
	}
	runFor(t, w)
	if !waitFor(t, func() bool { return len(bodies(t, db, s)[1]) == 1 }) || m.calls() != 1 {
		t.Fatalf("calls %d %v", m.calls(), bodies(t, db, s))
	}
}

// A chat with a notice runs before the chats Resume queued, also when it
// was queued by Resume itself.
func TestNoticedChatsRunBeforeResumedOnes(t *testing.T) {
	w := NewWorker(nil, nil, "fake", time.Minute)
	w.mu.Lock()
	for _, k := range []string{"chat:A", "chat:B", "chat:C"} {
		w.resumed = append(w.resumed, k)
		w.pending[k] = notice{reload: true}
	}
	w.mu.Unlock()
	w.Notify("chat:C", nil)
	w.Notify("chat:NEW", nil)
	var got []string
	for {
		s, n, ok := w.next(true)
		if !ok {
			break
		}
		got = append(got, fmt.Sprintf("%s/%v", s, n.reload))
	}
	if strings.Join(got, " ") != "chat:C/false chat:NEW/false chat:A/true chat:B/true" {
		t.Fatalf("order %v", got)
	}
}

// goOn is the next input after rows (a new user row each time): it
// confirms the turn before it.
func goOn(s *lifecycle.Session, rows ...lifecycle.Row) lifecycle.Snapshot {
	nextInputs++
	sn := a28Snap(s.Key, lifecycle.SourceBeforeRequest, append(append([]lifecycle.Row{}, rows...), row(fmt.Sprintf("next%d", nextInputs), "user", "", "다음"))...)
	s.Apply(sn)
	return sn
}

var nextInputs int

// Beta round 2 restores final-output reading before reasoning removal;
// source storage still keeps the original reply.
func TestGenerationReadsFinalOutput(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:R")
	reply := "<Thoughts>숨긴 비밀</Thoughts>초안<ACFinal><think>최종 생각</think>실제 응답</ACFinal>"
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "입력"), row("a1", "char", "g1", reply)))
	goOn(s, row("u1", "user", "", "입력"), row("a1", "char", "g1", reply))
	run(w, s)
	if m.calls() != 1 {
		t.Fatalf("calls %d", m.calls())
	}
	m.mu.Lock()
	prompt := m.prompts[0]
	m.mu.Unlock()
	if !strings.Contains(prompt, "REPLY: 실제 응답") || strings.Contains(prompt, "초안") || strings.Contains(prompt, "숨긴 비밀") || strings.Contains(prompt, "최종 생각") {
		t.Fatalf("generation did not read the cleaned final reply: %q", prompt[strings.Index(prompt, "THIS TURN"):])
	}
	if got := bodies(t, db, s); len(got) != 1 || got[1][0] != "실제 응답 기록" {
		t.Fatalf("memory %v", got)
	}
	src := memory.SourceOf("chat:R", s.ActiveTurns()[0])
	if len(src.Responses) != 1 || src.Responses[0].Data != reply {
		t.Fatalf("stored reply changed: %+v", src.Responses)
	}
}

// Generation reads a reply without the model's reasoning envelope; the
// stored reply keeps it.
func TestGenerationIgnoresReasoning(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:R")
	reply := "<Thoughts>\n- 계획: 숨긴 비밀을 다음 턴에 밝힌다\n</Thoughts>\n실제 응답"
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "입력"), row("a1", "char", "g1", reply)))
	goOn(s, row("u1", "user", "", "입력"), row("a1", "char", "g1", reply))
	run(w, s)
	if m.calls() != 1 {
		t.Fatalf("calls %d", m.calls())
	}
	m.mu.Lock()
	prompt := m.prompts[0]
	m.mu.Unlock()
	if strings.Contains(prompt, "Thoughts") || strings.Contains(prompt, "숨긴 비밀") || !strings.Contains(prompt, "REPLY: 실제 응답") {
		t.Fatalf("generation read the reasoning: %q", prompt[strings.Index(prompt, "THIS TURN"):])
	}
	if got := bodies(t, db, s); len(got) != 1 || got[1][0] != "실제 응답 기록" {
		t.Fatalf("memory %v", got)
	}
	src := memory.SourceOf("chat:R", s.ActiveTurns()[0])
	if len(src.Responses) != 1 || src.Responses[0].Data != reply {
		t.Fatalf("stored reply changed: %+v", src.Responses)
	}
}

// The critic reads a reply without the markup a card or another plugin put
// in it (an illustration plugin wraps the prose in HTML): the words stay,
// the tags and style blocks go; the stored reply keeps it all.
func TestGenerationReadsWithoutMarkup(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:H")
	reply := "문이 열렸다.\n<div class=\"am-illustration-projection\" data-am-message-id=\"x1\"><style>.a{color:red}</style><span style=\"display:block\">검은 정장의 <b>소녀</b>가 서 있었다.</span></div>\n그녀가 웃었다 &amp; 인사했다."
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "입력"), row("a1", "char", "g1", reply)))
	goOn(s, row("u1", "user", "", "입력"), row("a1", "char", "g1", reply))
	run(w, s)
	if m.calls() != 1 {
		t.Fatalf("calls %d", m.calls())
	}
	m.mu.Lock()
	prompt := m.prompts[0]
	m.mu.Unlock()
	turn := prompt[strings.Index(prompt, "THIS TURN"):]
	for _, gone := range []string{"<div", "<span", "<b>", "am-illustration", "color:red", "&amp;"} {
		if strings.Contains(turn, gone) {
			t.Fatalf("the critic read markup %q: %q", gone, turn)
		}
	}
	for _, kept := range []string{"문이 열렸다.", "검은 정장의 소녀가 서 있었다.", "그녀가 웃었다 & 인사했다."} {
		if !strings.Contains(turn, kept) {
			t.Fatalf("the critic lost %q: %q", kept, turn)
		}
	}
	src := memory.SourceOf("chat:H", s.ActiveTurns()[0])
	if len(src.Responses) != 1 || src.Responses[0].Data != reply {
		t.Fatalf("stored reply changed: %+v", src.Responses)
	}
}
