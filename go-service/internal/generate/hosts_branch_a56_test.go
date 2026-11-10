package generate

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
	"strings"
	"testing"
	"time"
)

func a56seed(t *testing.T, db *store.DB, src memory.Source, body string) {
	t.Helper()
	raw := `{"entities":[{"id":"n1","name":"Mira","type":"person"}],"memories":[{"id":"m1","kind":"fact","mode":"상태","about":"n1","body":"` + body + `"}]}`
	out, notes := memory.Parse(raw)
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SaveGeneration(j, store.Generation{Model: "synthetic", Response: raw, Output: out, Notes: notes}); err != nil {
		t.Fatal(err)
	}
}

func TestA56DeletedParentMemoryRead(t *testing.T) {
	for _, kept := range []bool{false, true} {
		name := "new_ids"
		if kept {
			name = "kept_ids"
		}
		t.Run(name, func(t *testing.T) {
			db := testDB(t)
			p := pipeline.New(db)
			pr := []lifecycle.Row{row("u1", "user", "", "first input"), row("a1", "char", "g1", "first answer"), row("u2", "user", "", "second input"), row("a2", "char", "g2", "second answer")}
			ps := a55apply(t, p, db, "P", 1, pr...)
			src := memory.SourceOf(ps.Key, ps.Turns[0])
			a55seed(t, db, src, "DELETED_PARENT_MEMORY")
			a55apply(t, p, db, "P", 2, pr[2:]...)
			cr := append([]lifecycle.Row{}, pr...)
			if !kept {
				for i := range cr {
					cr[i].ChatID = "child_" + cr[i].ChatID
				}
			}
			cr = append(cr, marker("P", "a2", "mk"), row("cn", "user", "", "next"))
			c := a55apply(t, p, db, "C", 3, cr...)
			sc := memory.ScopeOf(c)
			got := a28Bodies(t, db, c)
			t.Logf("A56: deleted unique kept=%v inherited=%v keyEqual=%v current=%q", kept, sc[0].Inherited, sc[0].Source.Key() == src.Key(), got)
			if !sc[0].Inherited || sc[0].Source.Key() != src.Key() || !strings.Contains(got, "DELETED_PARENT_MEMORY") {
				t.Fatal("deleted unique parent copy memory unavailable")
			}
		})
	}
}

func TestA56MissingRecordAvailability(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	pr := []lifecycle.Row{row("u1", "user", "", "q1"), row("a1", "char", "g1", "first"), row("u2", "user", "", "q2"), row("a2", "char", "g2", "last")}
	cr := append(append([]lifecycle.Row{}, pr...), marker("P", "a2", "mk"), row("cn", "user", "", "next"))
	c := a55apply(t, p, db, "C", 1, cr...)
	run(w, c)
	before := a28Bodies(t, db, c)
	if strings.Contains(before, "first") || !strings.Contains(before, "last") {
		t.Fatalf("unexpected unrecorded history %q", before)
	}
	ps := a55apply(t, p, db, "P", 2, pr...)
	a55seed(t, db, memory.SourceOf(ps.Key, ps.Turns[0]), "FIRST_PARENT")
	a55seed(t, db, memory.SourceOf(ps.Key, ps.Turns[1]), "LAST_PARENT")
	noMarker := append(append([]lifecycle.Row{}, pr...), cr[len(cr)-1])
	c = a55apply(t, p, db, "C", 3, noMarker...)
	run(w, c)
	missing := a28Bodies(t, db, c)
	want, err := w.Backfill(c.Key, memory.Own(c), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("A56: absent-parent current=%q marker-absent current=%q calls=%d copies=%v backfillTurns=%v", before, missing, m.calls(), c.CopiesOf, want)
	if c.CopiesOf[0] != "" || strings.Contains(missing, "FIRST_PARENT") || len(want) != 1 {
		t.Fatal("unrecorded scope/backfill contract")
	}
	run(w, c)
	filled := a28Bodies(t, db, c)
	t.Logf("A56: explicit Backfill current=%q calls=%d", filled, m.calls())
	if !strings.Contains(filled, "first 기록") {
		t.Fatal("explicit own-history Backfill failed")
	}
	c = a55apply(t, p, db, "C", 4, cr...)
	run(w, c)
	restored := a28Bodies(t, db, c)
	t.Logf("A56: marker restored copies=%v current=%q calls=%d", c.CopiesOf, restored, m.calls())
	if !strings.Contains(restored, "FIRST_PARENT") || !strings.Contains(restored, "LAST_PARENT") {
		t.Fatal("record recovery failed")
	}
}

func TestA56A50RecordedInputSurvivesReload(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	pr := []lifecycle.Row{row("u", "user", "", "old input"), row("a", "char", "g1", "answer")}
	ps := a55apply(t, p, db, "P", 1, pr...)
	src := memory.SourceOf(ps.Key, ps.Turns[0])
	a55seed(t, db, src, "OLD_PARENT_VERSION")
	cr := append(append([]lifecycle.Row{}, pr...), marker("P", "a", "mk"), row("cn", "user", "", "next"))
	c := a55apply(t, p, db, "C", 2, cr...)
	pr[0].Data = "edited input"
	ps = a55apply(t, p, db, "P", 3, pr...)
	a55seed(t, db, memory.SourceOf(ps.Key, ps.Turns[0]), "NEW_PARENT_VERSION")
	c = a55apply(t, pipeline.New(db), db, "C", 4, cr...)
	sc := memory.ScopeOf(c)
	got := a28Bodies(t, db, c)
	t.Logf("A56: A50 after reload inherited=%v oldKey=%v oldInput=%q current=%q", sc[0].Inherited, sc[0].Source.Key() == src.Key(), sc[0].Source.Members[0].Data, got)
	if !sc[0].Inherited || sc[0].Source.Key() != src.Key() || !strings.Contains(got, "OLD_PARENT_VERSION") || strings.Contains(got, "NEW_PARENT_VERSION") {
		t.Fatal("recorded old input changed after parent edit")
	}
	cr[0].Data = "child edited input"
	c = a55apply(t, pipeline.New(db), db, "C", 5, cr...)
	if memory.ScopeOf(c)[0].Inherited {
		t.Fatal("child input edit must use own source")
	}
	// A first record after the parent input changed cannot reconstruct an old member revision.
	dr := []lifecycle.Row{row("u", "user", "", "old input"), row("a", "char", "g1", "answer"), marker("P", "a", "dm")}
	d := a55apply(t, p, db, "D", 6, dr...)
	t.Logf("A56: first observed after parent input edit inherited=%v current=%q", memory.ScopeOf(d)[0].Inherited, a28Bodies(t, db, d))
	if memory.ScopeOf(d)[0].Inherited {
		t.Fatal("unobserved old input must not borrow new input version")
	}
}

func TestA56PartialOffRowsShareSourceKey(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	rr := []lifecycle.Row{row("u", "user", "", "q"), row("a", "char", "gA", "same"), row("b", "char", "gB", "same"), row("n", "user", "", "next")}
	rr[2].Disabled = "true"
	s := a55apply(t, p, db, "C", 1, rr...)
	a := memory.SourceOf(s.Key, s.Turns[0])
	a55seed(t, db, a, "ROW_A_ONLY")
	rr[1].Disabled = "true"
	rr[2].Disabled = ""
	s = a55apply(t, p, db, "C", 2, rr...)
	b := memory.SourceOf(s.Key, s.Turns[0])
	got := a28Bodies(t, db, s)
	t.Logf("A56: partial switch row=%s -> %s gen=%s -> %s seq=%d -> %d keysEqual=%v current=%q", a.Responses[0].Key, b.Responses[0].Key, a.Responses[0].Gen, b.Responses[0].Gen, a.Responses[0].Seq, b.Responses[0].Seq, a.Key() == b.Key(), got)
	if strings.Contains(got, "ROW_A_ONLY") {
		t.Fatal("disabled distinct response row A reused as response row B memory")
	}
}
