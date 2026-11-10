package generate

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Only interleave a real observation at a real store-read boundary.
// All storage methods still run against disposable MariaDB.
type a57Store struct {
	*store.DB
	afterKnown func([]memory.Source)
	afterDone  func()
}

func (d *a57Store) KnownFor(s []memory.Source) (memory.Known, error) {
	k, e := d.DB.KnownFor(s)
	if d.afterKnown != nil {
		d.afterKnown(s)
	}
	return k, e
}
func (d *a57Store) DoneKeys(s string) (map[string]bool, error) {
	k, e := d.DB.DoneKeys(s)
	if d.afterDone != nil {
		d.afterDone()
	}
	return k, e
}
func a57Done(db *store.DB, src memory.Source) bool {
	j, e := db.JobFor(src)
	return e == nil && j.State == "done"
}

// This is an expected failing acceptance assertion if a deletion received
// during prompt construction still causes a deleted turn to be generated.
func TestA57NoticeDuringKnownRead(t *testing.T) {
	db := testDB(t)
	d := &a57Store{DB: db}
	m := &fakeModel{}
	w := NewWorker(d, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	rows := []lifecycle.Row{row("u1", "user", "", "one"), row("a1", "char", "g1", "first"), row("u2", "user", "", "two"), row("a2", "char", "g2", "deleted second"), row("u3", "user", "", "three")}
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, rows[:4]...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, rows...))
	oldSecond := memory.Own(s)[1].Source
	if _, e := w.Backfill(s.Key, memory.Own(s), 0, false); e != nil {
		t.Fatal(e)
	}
	injected := false
	changed := make(chan struct{})
	d.afterKnown = func(earlier []memory.Source) {
		if injected || len(earlier) != 1 {
			return
		}
		injected = true
		s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, rows[0], rows[1], rows[4]))
		notify(w, s)
		close(changed)
	}
	notify(w, s)
	stop := running(t, w)
	defer stop()
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("read interleaving did not run")
	}
	waitUntil(t, "latest notice taken", func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.pending) == 0 })
	stop()
	j, _ := db.JobFor(oldSecond)
	t.Logf("new notice during KnownFor; calls=%d deleted_job=%s attempts=%d current=%q", m.calls(), j.State, j.Attempts, a28Bodies(t, db, s))
	if m.calls() != 1 {
		t.Fatalf("deleted T2 still called after newer deletion notice: calls=%d", m.calls())
	}
}

func TestA57FiniteNoticesKeepGrantAndIndex(t *testing.T) {
	db := testDB(t)
	d := &a57Store{DB: db}
	m := &fakeModel{}
	w := NewWorker(d, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u1", "user", "", "one"), row("a1", "char", "g1", "first")))
	if _, e := w.Backfill(s.Key, memory.Own(s), 0, false); e != nil {
		t.Fatal(e)
	}
	save(w, s)
	count := 0
	var indexed atomic.Int64
	d.afterDone = func() {
		if count < 20 {
			count++
			w.Notify(s.Key, memory.Own(s))
			if m.calls() != 0 {
				t.Error("call before notice stream ended")
			}
		}
	}
	w.Index = func(_ context.Context, j []int64) error { indexed.Add(int64(len(j))); return nil }
	src := memory.Own(s)[0].Source
	stop := running(t, w)
	defer stop()
	waitUntil(t, "grant to finish", func() bool { return a57Done(db, src) })
	waitUntil(t, "separate index to finish", func() bool { return indexed.Load() == 1 })
	stop()
	g, e := db.Grants(s.Key)
	if e != nil {
		t.Fatal(e)
	}
	j, _ := db.JobFor(src)
	t.Logf("superseded_runs=%d calls=%d attempts=%d grants_left=%d indexed=%d", count, m.calls(), j.Attempts, len(g), indexed.Load())
	if count != 20 || m.calls() != 1 || j.Attempts != 1 || len(g) != 0 || indexed.Load() != 1 {
		t.Fatal("work was lost or duplicated")
	}
}

func TestA57LateOffAndSwipe(t *testing.T) {
	for _, mode := range []string{"off", "swipe"} {
		t.Run(mode, func(t *testing.T) {
			db := testDB(t)
			m := &fakeModel{hold: make(chan struct{})}
			w := NewWorker(db, m, "fake", time.Minute)
			s := lifecycle.NewSession("chat:A")
			u := row("u1", "user", "", "one")
			a := row("a1", "char", "g1", "old answer")
			s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, u, a))
			goOn(s, u, a)
			original := memory.Own(s)[0].Source
			stop := running(t, w)
			defer stop()
			notify(w, s)
			waitUntil(t, "first call", func() bool { return m.calls() == 1 })
			if mode == "off" {
				a = off(a, "true")
			} else {
				a.Data = "chosen answer"
			}
			u2 := row("u2", "user", "", "two")
			a2 := row("a2", "char", "g2", "chosen answer continued")
			s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, u, a))
			s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, u, a, u2, a2))
			goOn(s, u, a, u2, a2)
			lastSrc := memory.Own(s)[1].Source
			notify(w, s)
			close(m.hold)
			waitUntil(t, "second turn saved", func() bool { return a57Done(db, lastSrc) })
			stop()
			cur := a28Bodies(t, db, s)
			last := m.prompts[len(m.prompts)-1]
			old, _ := db.JobFor(original)
			want := 2
			if mode == "swipe" {
				want = 3
			}
			t.Logf("mode=%s calls=%d old=%s current=%q", mode, m.calls(), old.State, cur)
			if m.calls() != want || old.State != "done" || strings.Contains(cur, "old answer 기록") || strings.Contains(last, "old answer 기록") {
				t.Fatal("late old result leaked or lost")
			}
			if mode == "swipe" && !strings.Contains(last, "chosen answer 기록") {
				t.Fatal("chosen memory absent from next prompt")
			}
		})
	}
}

func TestA57LateParentResultInRecordedBranch(t *testing.T) {
	for _, ids := range []string{"kept", "new"} {
		t.Run(ids, func(t *testing.T) {
			db := testDB(t)
			m := &fakeModel{hold: make(chan struct{})}
			w := NewWorker(db, m, "fake", time.Minute)
			p := lifecycle.NewSession("chat:P")
			u := row("u1", "user", "", "one")
			a := row("a1", "char", "g1", "copied past")
			p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, u, a))
			goOn(p, u, a)
			original := memory.Own(p)[0].Source
			stop := running(t, w)
			defer stop()
			notify(w, p)
			waitUntil(t, "parent call", func() bool { return m.calls() == 1 })
			cu, ca := u, a
			if ids == "new" {
				cu.ChatID = "x1"
				ca.ChatID = "y1"
			}
			b := lifecycle.NewSession("chat:B")
			bu := row("u2", "user", "", "two")
			ba := row("a2", "char", "h2", "copied past continued")
			b.Apply(a28Snap(b.Key, lifecycle.SourceBeforeRequest, cu, ca, marker("P", "a1", "mk"), bu))
			notify(w, b)
			b.Apply(a28Snap(b.Key, lifecycle.SourceOutputEvent, cu, ca, marker("P", "a1", "mk"), bu, ba))
			goOn(b, cu, ca, marker("P", "a1", "mk"), bu, ba)
			notify(w, b)
			branchTurn := memory.Own(b)[1].Source
			p.Apply(a28Snap(p.Key, lifecycle.SourceBeforeRequest, u))
			fresh := row("a3", "char", "g3", "parent changed")
			p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, u, fresh))
			goOn(p, u, fresh)
			notify(w, p)
			currentParent := memory.Own(p)[0].Source
			// The child no longer waits behind its parent's provider call.
			// With the parent's copy still unmade, the existing fallback makes
			// it as the child's own source. Require this interleaving explicitly.
			waitUntil(t, "child's own copy while parent is pending", func() bool { return m.calls() == 2 })
			close(m.hold)
			waitUntil(t, "parent and child done", func() bool { return a57Done(db, currentParent) && a57Done(db, branchTurn) })
			stop()
			pm, bm := a28Bodies(t, db, p), a28Bodies(t, db, b)
			old, _ := db.JobFor(original)
			t.Logf("ids=%s calls=%d old=%s parent=%q branch=%q", ids, m.calls(), old.State, pm, bm)
			if m.calls() != 4 || old.State != "done" || strings.Contains(pm, "copied past 기록") || !strings.Contains(bm, "copied past 기록") || strings.Contains(bm, "parent changed 기록") {
				t.Fatal("branch/current source ownership broken")
			}
			var childPrompt string
			for _, prompt := range m.prompts {
				if strings.Contains(prompt, "REPLY: copied past continued") {
					childPrompt = prompt
				}
			}
			if !strings.Contains(childPrompt, "copied past 기록") || strings.Contains(childPrompt, "parent changed 기록") {
				t.Fatal("child prompt did not read its recorded past")
			}
		})
	}
}
