package generate

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
	"context"
	"strings"
	"testing"
	"time"
)

func a28Snap(key string, src lifecycle.Source, rows ...lifecycle.Row) lifecycle.Snapshot {
	sn := snap(src, rows...)
	sn.SessionKey = key
	return sn
}
func a28Save(t *testing.T, db *store.DB, s *lifecycle.Session) {
	t.Helper()
	recordBranch(db, s)
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
}
func a28Marker(parent, row string) lifecycle.Row {
	return lifecycle.Row{ChatID: "mk", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::" + parent + "::이름::" + row + "::}}"}
}
func a28Bodies(t *testing.T, db *store.DB, s *lifecycle.Session) string {
	t.Helper()
	k, e := db.CurrentKnown(s)
	if e != nil {
		t.Fatal(e)
	}
	var b []string
	for _, m := range k.Memories {
		b = append(b, m.Body)
	}
	return strings.Join(b, " | ")
}

func TestA28ParentRevisionCannotRewriteChildPast(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	p := lifecycle.NewSession("chat:A")
	original := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "분기 당시 과거")}
	p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, original...))
	goOn(p, original...)
	a28Save(t, db, p)
	run(w, p)
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceLoad, append(append([]lifecycle.Row{}, original...), a28Marker("A", "a1"))...))
	a28Save(t, db, c)
	run(w, c)
	before := a28Bodies(t, db, c)
	p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g2", "분기 이후 부모 리롤")))
	goOn(p, row("u1", "user", "", "하나"), row("a1", "char", "g2", "분기 이후 부모 리롤"))
	a28Save(t, db, p)
	run(w, p)
	after := a28Bodies(t, db, c)
	t.Logf("before=%q after=%q", before, after)
	if !strings.Contains(before, "분기 당시 과거 기록") || m.calls() != 2 {
		t.Fatalf("fixture: child past %q, calls %d (the parent's reroll must be made too)", before, m.calls())
	}
	if after != before {
		t.Fatal("parent revision changed the unchanged child's past")
	}
}

func TestA28EditedChildReplacesInheritedTurn(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	p := lifecycle.NewSession("chat:A")
	p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 옛 값")))
	goOn(p, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 옛 값"))
	a28Save(t, db, p)
	run(w, p)
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g2", "자식 고친 값"), a28Marker("A", "a1")))
	goOn(c, row("u1", "user", "", "하나"), row("a1", "char", "g2", "자식 고친 값"), a28Marker("A", "a1"))
	a28Save(t, db, c)
	run(w, c)
	got := a28Bodies(t, db, c)
	t.Logf("current=%q", got)
	if strings.Contains(got, "부모 옛 값") || !strings.Contains(got, "자식 고친 값") {
		t.Fatal("edited child contains both replaced parent and child")
	}
}

func TestA28UserForkDoesNotIncludeParentAnswer(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	p := lifecycle.NewSession("chat:A")
	p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "질문"), row("a1", "char", "g1", "부모에게만 있던 답")))
	goOn(p, row("u1", "user", "", "질문"), row("a1", "char", "g1", "부모에게만 있던 답"))
	a28Save(t, db, p)
	run(w, p)
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceLoad, row("u1", "user", "", "질문"), a28Marker("A", "u1")))
	a28Save(t, db, c)
	got := a28Bodies(t, db, c)
	t.Logf("branch=%v own=%+v current=%q", c.BranchOf, memory.Own(c), got)
	if !strings.Contains(a28Bodies(t, db, p), "부모에게만 있던 답 기록") {
		t.Fatal("fixture: the parent's answer was not made")
	}
	if strings.Contains(got, "부모에게만 있던 답") {
		t.Fatal("user-row fork imported the parent's later answer")
	}
}

func TestA28NestedBranchRetainsAncestor(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	p := lifecycle.NewSession("chat:A")
	original := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "공유 과거")}
	p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, original...))
	goOn(p, original...)
	a28Save(t, db, p)
	run(w, p)
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceOutputEvent, append(append([]lifecycle.Row{}, original...), a28Marker("A", "a1"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "자식 이후"))...))
	goOn(c, append(append([]lifecycle.Row{}, original...), a28Marker("A", "a1"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "자식 이후"))...)
	a28Save(t, db, c)
	run(w, c)
	g := lifecycle.NewSession("chat:G")
	g.Apply(a28Snap(g.Key, lifecycle.SourceLoad, append(append([]lifecycle.Row{}, original...), row("u2", "user", "", "둘"), row("a2", "char", "g2", "자식 이후"), a28Marker("C", "a2"))...))
	a28Save(t, db, g)
	got := a28Bodies(t, db, g)
	t.Logf("parent=%q grandchild=%q", a28Bodies(t, db, c), got)
	if !strings.Contains(got, "공유 과거") {
		t.Fatal("nested branch lost ancestor memory")
	}
}

func TestA28LateStoryDateReachesGeneratedMemory(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	s := lifecycle.NewSession("chat:A")
	rows := []lifecycle.Row{row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")}
	sn := snap(lifecycle.SourceOutputEvent, rows...)
	sn.Dates = map[string]string{"clock": "1423/05/15"}
	s.Apply(sn)
	goOn(s, rows...)
	a28Save(t, db, s)
	run(w, s)
	sn.Source = lifecycle.SourceOutputReread
	sn.Dates = map[string]string{"clock": "1423/05/16"}
	s.Apply(sn)
	a28Save(t, db, s)
	run(w, s)
	j, _ := db.JobFor(memory.Own(s)[0].Source)
	t.Logf("latest_date=%v calls=%d job=%+v first_prompt_date=%v", s.ActiveTurns()[0].Dates, m.calls(), j, strings.Contains(m.prompts[0], "1423/05/15"))
	// the date is the turn's, read with its memory: the turn is not made
	// again, and the next turn reads T1's memory at the corrected date
	if m.calls() != 1 {
		t.Fatalf("a date change made the turn again: %d calls", m.calls())
	}
	next := snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "첫째 기록을 보았다"))
	next.Dates = map[string]string{"clock": "1423/05/16"}
	s.Apply(next)
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "첫째 기록을 보았다"))
	a28Save(t, db, s)
	run(w, s)
	if m.calls() != 2 || !strings.Contains(m.prompts[1], "T1 (clock = 1423/05/16) fact") {
		t.Fatalf("corrected observed date never reaches the generation input:\n%s", m.prompts[len(m.prompts)-1])
	}
}

func TestA28HistoryReadinessDoesNotConfirmTurns(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "둘째"), row("u3", "user", "", "셋"), row("a3", "char", "g3", "셋째")))
	// history is not made by itself (a range, as AC1); the user asks for
	// the oldest two, then the rest from the most recent
	run(w, s)
	if m.calls() != 0 {
		t.Fatalf("history made without a request: %d calls", m.calls())
	}
	asked, err := w.Backfill(s.Key, memory.Own(s), 2, false)
	if err != nil || len(asked) != 2 || asked[0] != 1 || asked[1] != 2 {
		t.Fatalf("asked %v %v", asked, err)
	}
	run(w, s)
	t.Logf("calls=%d turns=%d", m.calls(), len(s.ActiveTurns()))
	if m.calls() != 2 {
		t.Fatal("history readiness mismatch")
	}
	if asked, _ := w.Backfill(s.Key, memory.Own(s), 0, true); len(asked) != 1 || asked[0] != 3 {
		t.Fatalf("rest %v", asked)
	}
	run(w, s)
	if m.calls() != 3 {
		t.Fatalf("rest made %d", m.calls())
	}
	for _, tr := range s.ActiveTurns() {
		if tr.Confirmed {
			t.Fatal("history was confirmed")
		}
	}
}

// Keep context referenced: probes never invoke real providers.
var _ = context.Background

func TestA28MissingParentOrPoint(t *testing.T) {
	for _, missing := range []string{"parent", "point"} {
		t.Run(missing, func(t *testing.T) {
			db := testDB(t)
			m := &fakeModel{}
			w := NewWorker(db, m, "fake", time.Second)
			if missing == "point" {
				p := lifecycle.NewSession("chat:A")
				p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, row("ux", "user", "", "별개"), row("ax", "char", "gx", "무관한 부모")))
				goOn(p, row("ux", "user", "", "별개"), row("ax", "char", "gx", "무관한 부모"))
				a28Save(t, db, p)
				run(w, p)
			}
			c := lifecycle.NewSession("chat:C")
			c.Apply(a28Snap(c.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "자식 기록"), a28Marker("A", "a1")))
			goOn(c, row("u1", "user", "", "하나"), row("a1", "char", "g1", "자식 기록"), a28Marker("A", "a1"))
			a28Save(t, db, c)
			run(w, c)
			got := a28Bodies(t, db, c)
			t.Logf("missing=%s current=%q", missing, got)
			if got != "자식 기록 기록" {
				t.Fatal("child data unavailable or unrelated parent leaked")
			}
		})
	}
}

func TestA28FailedCallWaitsForNextNotice(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{fail: true}
	w := NewWorker(db, m, "fake", time.Second)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() { w.Run(ctx); close(ended) }()
	w.Notify(s.Key, memory.Own(s))
	deadline := time.Now().Add(3 * time.Second)
	for {
		w.mu.Lock()
		failed := w.failed
		w.mu.Unlock()
		if failed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not run")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	calls := m.calls()
	cancel()
	<-ended
	t.Logf("calls_after_one_notice=%d", calls)
	if calls != 1 {
		t.Fatal("worker loops after failure without another notice")
	}
}
