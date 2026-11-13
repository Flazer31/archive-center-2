package generate

import (
	"context"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
)

// A run that took the chat as it was before a turn was edited reads the
// grant the edited turn was just given: the old text is made, the edited
// one is not, so the grant stays and the edited turn is made (11-13: found
// by TestHypaImportReview15 failing now and then; the grant was dropped
// and the edited summary never made, since 1.0.0).
func TestGrantSurvivesAStaleRun(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "m", time.Minute)
	s := lifecycle.NewSession("chat:GR")
	s.Apply(a28Snap("chat:GR", lifecycle.SourceLoad, row("u1", "user", "", "표시 없음"), row("a1", "char", "g1", "요약 하나")))
	save(w, s)
	old := memory.WithPast(s)
	if asked, err := w.Backfill(s.Key, old, 0, false); err != nil || len(asked) != 1 {
		t.Fatalf("first ask %v %v", asked, err)
	}
	chatNow(w, s.Key, notice{turns: old})
	if m.calls() != 1 {
		t.Fatalf("first made: %d calls", m.calls())
	}
	// the turn's user row edited (its marks), the chat loaded again
	s.Apply(a28Snap("chat:GR", lifecycle.SourceLoad, row("u1", "user", "", "중요 표시"), row("a1", "char", "g1", "요약 하나")))
	save(w, s)
	if asked, err := w.Backfill(s.Key, memory.WithPast(s), 0, false); err != nil || len(asked) != 1 {
		t.Fatalf("second ask %v %v", asked, err)
	}
	// a run that took the chat before the edit reads the new grant
	w.chat(context.Background(), s.Key, notice{turns: old})
	g, err := db.Grants(s.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !g[1] {
		t.Fatalf("the edited turn's grant was dropped by the stale run: %v", g)
	}
	chatNow(w, s.Key, notice{turns: memory.WithPast(s)})
	if m.calls() != 2 {
		t.Fatalf("the edited turn was not made: %d calls, grants after the stale run %v", m.calls(), g)
	}
}

// A grant is dropped once the text it asks for is made.
func TestGrantDroppedWhenMade(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "m", time.Minute)
	s := lifecycle.NewSession("chat:GD")
	s.Apply(a28Snap("chat:GD", lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	save(w, s)
	if _, err := w.Backfill(s.Key, memory.WithPast(s), 0, false); err != nil {
		t.Fatal(err)
	}
	chatNow(w, s.Key, notice{turns: memory.WithPast(s)})
	if g, _ := db.Grants(s.Key); len(g) != 0 || m.calls() != 1 {
		t.Fatalf("made: grants %v calls %d", g, m.calls())
	}
}

// Asked again while an earlier grant of the turn is still kept (its job
// made, the grant not dropped yet): the ask is now, so a stale run keeps it.
func TestGrantAskedAgainIsNow(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "m", time.Minute)
	s := lifecycle.NewSession("chat:GA")
	s.Apply(a28Snap("chat:GA", lifecycle.SourceLoad, row("u1", "user", "", "표시 없음"), row("a1", "char", "g1", "요약 하나")))
	save(w, s)
	old := memory.WithPast(s)
	if err := db.Grant(s.Key, []memory.Source{old[0].Source}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	j, err := db.JobFor(old[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	out, notes := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"옛 기록"}]}`)
	if _, err := db.SaveGeneration(j, store.Generation{Request: "r", ReadItems: "[]", Response: "{}", Output: out, Notes: notes}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	s.Apply(a28Snap("chat:GA", lifecycle.SourceLoad, row("u1", "user", "", "중요 표시"), row("a1", "char", "g1", "요약 하나")))
	save(w, s)
	if _, err := w.Backfill(s.Key, memory.WithPast(s), 0, false); err != nil {
		t.Fatal(err)
	}
	w.chat(context.Background(), s.Key, notice{turns: old})
	if g, _ := db.Grants(s.Key); !g[1] {
		t.Fatalf("asked again, dropped by the stale run: %v", g)
	}
}

// Edited while the old text's call is under way: the old answer stored
// after the edit leaves the edited text's grant, and the edited text is
// made.
func TestGrantEditDuringTheOldCall(t *testing.T) {
	db := testDB(t)
	hold := make(chan struct{})
	m := &fakeModel{hold: hold}
	w := NewWorker(db, m, "m", time.Minute)
	runFor(t, w)
	s := lifecycle.NewSession("chat:GE")
	s.Apply(a28Snap("chat:GE", lifecycle.SourceLoad, row("u1", "user", "", "표시 없음"), row("a1", "char", "g1", "요약 하나")))
	save(w, s)
	if _, err := w.Backfill(s.Key, memory.WithPast(s), 0, false); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool { return m.calls() == 1 }) {
		t.Fatal("the old call did not start")
	}
	s.Apply(a28Snap("chat:GE", lifecycle.SourceLoad, row("u1", "user", "", "중요 표시"), row("a1", "char", "g1", "요약 하나")))
	save(w, s)
	if _, err := w.Backfill(s.Key, memory.WithPast(s), 0, false); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.hold = nil
	m.mu.Unlock()
	close(hold)
	edited := memory.WithPast(s)[0].Source
	if !waitFor(t, func() bool { st, _ := db.SourceJobState(edited); return st == "done" }) {
		g, _ := db.Grants(s.Key)
		t.Fatalf("the edited text was not made: calls %d grants %v", m.calls(), g)
	}
	if !waitFor(t, func() bool { g, _ := db.Grants(s.Key); return len(g) == 0 }) {
		t.Fatal("the edited text made, its grant kept")
	}
}

// Asked just after the same text was made (Backfill read the chat before
// the run stored it): the next run finds it made and drops the grant.
func TestGrantOfTheSameTextMadeIsDropped(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "m", time.Minute)
	s := lifecycle.NewSession("chat:GS")
	s.Apply(a28Snap("chat:GS", lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	save(w, s)
	src := memory.WithPast(s)[0].Source
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	out, notes := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"기록"}]}`)
	if _, err := db.SaveGeneration(j, store.Generation{Request: "r", ReadItems: "[]", Response: "{}", Output: out, Notes: notes}); err != nil {
		t.Fatal(err)
	}
	if err := db.Grant(s.Key, []memory.Source{src}); err != nil {
		t.Fatal(err)
	}
	chatNow(w, s.Key, notice{turns: memory.WithPast(s)})
	if g, _ := db.Grants(s.Key); len(g) != 0 || m.calls() != 0 {
		t.Fatalf("grants %v calls %d", g, m.calls())
	}
}

// Edited after it was asked for, by an observation alone (no Backfill
// again): the grant asks for the edited text, goes when it is made, and a
// later edit of the still unconfirmed turn makes no call.
func TestGrantFollowsAnEdit(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "m", time.Minute)
	s := lifecycle.NewSession("chat:GF")
	s.Apply(a28Snap("chat:GF", lifecycle.SourceLoad, row("u1", "user", "", "A"), row("a1", "char", "g1", "요약")))
	save(w, s)
	if _, err := w.Backfill(s.Key, memory.WithPast(s), 0, false); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	delete(w.pending, s.Key) // the notice is replaced by the edit's
	w.mu.Unlock()
	s.Apply(a28Snap("chat:GF", lifecycle.SourceLoad, row("u1", "user", "", "B"), row("a1", "char", "g1", "요약")))
	save(w, s)
	chatNow(w, s.Key, notice{turns: memory.WithPast(s)})
	if st, _ := db.SourceJobState(memory.WithPast(s)[0].Source); st != "done" || m.calls() != 1 {
		t.Fatalf("the edited text: %q, calls %d", st, m.calls())
	}
	if g, _ := db.Grants(s.Key); len(g) != 0 {
		t.Fatalf("the edited text made, its grant kept: %v", g)
	}
	s.Apply(a28Snap("chat:GF", lifecycle.SourceLoad, row("u1", "user", "", "C"), row("a1", "char", "g1", "요약")))
	save(w, s)
	chatNow(w, s.Key, notice{turns: memory.WithPast(s)})
	if m.calls() != 1 {
		t.Fatalf("an unconfirmed turn edited again was made without being asked: %d calls", m.calls())
	}
}
