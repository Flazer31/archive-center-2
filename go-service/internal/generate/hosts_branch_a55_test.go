package generate

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func a55event(chat string, seq int, rows ...lifecycle.Row) observe.Record {
	rr := []map[string]any{}
	for i, r := range rows {
		rr = append(rr, map[string]any{"i": i, "role": r.Role, "chat_id": r.ChatID, "generation_id": r.GenerationID, "is_comment": r.IsComment, "disabled": r.Disabled == "true", "data": map[string]any{"text": r.Data}})
	}
	b, _ := json.Marshal(map[string]any{"boot_id": "a55", "seq": seq, "hook": "beforeRequest", "stored_chat": map[string]any{"chat_id": chat, "rows": rr}})
	return observe.Record{Event: b}
}
func a55apply(t *testing.T, p *pipeline.Pipeline, db *store.DB, chat string, seq int, rows ...lifecycle.Row) *lifecycle.Session {
	t.Helper()
	if _, err := p.Process([]observe.Record{a55event(chat, seq, rows...)}); err != nil {
		t.Fatal(err)
	}
	s, ok, err := db.LoadSession("chat:" + chat)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	return s
}
func a55seed(t *testing.T, db *store.DB, src memory.Source, body string) store.Job {
	t.Helper()
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"memories": []map[string]string{{"id": "m1", "kind": "fact", "body": body}}})
	out, notes := memory.Parse(string(b))
	if _, err = db.SaveGeneration(j, store.Generation{Model: "synthetic", Response: string(b), Output: out, Notes: notes}); err != nil {
		t.Fatal(err)
	}
	return j
}
func a55scope(t *testing.T, db *store.DB, s *lifecycle.Session) []memory.ScopedTurn {
	t.Helper()
	return memory.ScopeOf(s)
}

func TestA55DBFirstRecord(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	pr := []lifecycle.Row{row("p1u", "user", "", "q"), row("p1a", "char", "g1", "a"), row("p2u", "user", "", "q"), row("p2a", "char", "g2", "a")}
	ps := a55apply(t, p, db, "P", 1, pr...)
	first := memory.SourceOf(ps.Key, ps.Turns[0])
	second := memory.SourceOf(ps.Key, ps.Turns[1])
	a55seed(t, db, first, "first occurrence")
	j2 := a55seed(t, db, second, "second occurrence")
	a55apply(t, p, db, "P", 2, pr[2:]...)
	cr := []lifecycle.Row{row("c1u", "user", "", "q"), row("c1a", "char", "g1", "a"), row("c2u", "user", "", "q"), row("c2a", "char", "g2", "a"), marker("P", "p2a", "mk"), row("next", "user", "", "next")}
	c := a55apply(t, p, db, "C", 3, cr...)
	got := a28Bodies(t, db, c)
	scope := a55scope(t, db, c)
	jobs, err := db.CurrentJobs([]memory.Source{scope[0].Source})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("A55: first child source=%s/%d selectedJobs=%v wrongParentJob=%d CurrentKnown=%q", scope[0].Source.Session, scope[0].Source.Turn, jobs, j2.ID, got)
	// A new pipeline reads the durable mapping and ignores a resent observation.
	p = pipeline.New(db)
	c = a55apply(t, p, db, "C", 3, cr...)
	again := a55scope(t, db, c)
	if scope[0].Source.Key() != again[0].Source.Key() {
		t.Fatal("restart/replay changed mapping")
	}
	if scope[0].Inherited && scope[0].Source.Key() != first.Key() {
		t.Fatal("CurrentKnown uses the second occurrence for the first copied turn, durably")
	}
}

func TestA55DBOffRows(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	pr := []lifecycle.Row{row("u", "user", "", "q"), row("a", "char", "g1", "same"), row("b", "char", "g2", "same")}
	pr[1].Disabled = "true"
	ps := a55apply(t, p, db, "P", 1, pr...)
	src := memory.SourceOf(ps.Key, ps.Turns[0])
	a55seed(t, db, src, "reply B only")
	cr := append([]lifecycle.Row{}, pr...)
	cr[1].Disabled = ""
	cr[2].Disabled = "true"
	cr = append(cr, marker("P", "b", "mk"), row("next", "user", "", "next"))
	c := a55apply(t, p, db, "C", 2, cr...)
	got := a28Bodies(t, db, c)
	scope := a55scope(t, db, c)
	t.Logf("A55: Off rows CurrentKnown=%q source=%+v", got, scope[0].Source)
	if strings.Contains(got, "reply B only") {
		t.Fatal("disabled child reply B's memory read as active reply A")
	}
}

func TestA55InitialOffIsMoreThanCost(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	pr := []lifecycle.Row{row("u1", "user", "", "q1"), row("a1", "char", "g1", "first"), row("u2", "user", "", "q2"), row("a2", "char", "g2", "last")}
	ps := a55apply(t, p, db, "P", 1, pr...)
	a55seed(t, db, memory.SourceOf(ps.Key, ps.Turns[0]), "first stored")
	a55seed(t, db, memory.SourceOf(ps.Key, ps.Turns[1]), "last stored")
	cr := append([]lifecycle.Row{}, pr...)
	cr[1].Disabled = "true"
	cr = append(cr, marker("P", "a2", "mk"), row("next", "user", "", "next"))
	c := a55apply(t, p, db, "C", 2, cr...)
	run(w, c)
	cr[1].Disabled = ""
	c = a55apply(t, p, db, "C", 3, cr...)
	run(w, c)
	got := a28Bodies(t, db, c)
	sc := a55scope(t, db, c)
	t.Logf("A55: initial Off then on calls=%d confirmed=%v inherited=%v current=%q", m.calls(), sc[0].Confirmed, sc[0].Inherited, got)
	if !strings.Contains(got, "first stored") && m.calls() == 0 {
		t.Fatal("reenabled earlier copy has no memory and no regeneration; not just extra cost")
	}
}

func TestA55DBDeferredBoundary(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	cr := []lifecycle.Row{row("cu", "user", "", "q"), marker("P", "p2u", "mk"), row("ca", "char", "g3", "a"), row("next", "user", "", "next")}
	c := a55apply(t, p, db, "C", 1, cr...)
	if c.CopiesOf[0] != "" {
		t.Fatal("recorded absent parent")
	}
	pr := []lifecycle.Row{row("p1u", "user", "", "q"), row("p1a", "char", "g1", "a"), row("p2u", "user", "", "q"), row("p2a", "char", "g2", "a")}
	ps := a55apply(t, p, db, "P", 2, pr...)
	a55seed(t, db, memory.SourceOf(ps.Key, ps.Turns[0]), "earlier parent occurrence")
	c = a55apply(t, p, db, "C", 3, cr[0], cr[2], cr[3])
	got := a28Bodies(t, db, c)
	t.Logf("A55: deferred durable copies=%v current=%q", c.CopiesOf, got)
	if strings.Contains(got, "earlier parent occurrence") {
		t.Fatal("record deferred, but current read still ignores the known child boundary")
	}
}

func TestA55CAndBControls(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Second)
	pr := []lifecycle.Row{row("u", "user", "", "q"), row("a", "char", "g1", "one"), row("b", "char", "g2", "two")}
	ps := a55apply(t, p, db, "P", 1, append(append([]lifecycle.Row{}, pr...), row("pn", "user", "", "next"))...)
	run(w, ps)
	cr := append(append([]lifecycle.Row{}, pr...), marker("P", "b", "mk"), row("cn", "user", "", "next"))
	c := a55apply(t, p, db, "C", 2, cr...)
	run(w, c)
	original := a55scope(t, db, c)[0].Source.Key()
	if m.calls() != 1 {
		t.Fatal("full copy not reused")
	}
	pr[1].Disabled = "true"
	a55apply(t, p, db, "P", 3, append(pr, row("pn", "user", "", "next"))...)
	if a55scope(t, db, c)[0].Source.Key() != original {
		t.Fatal("parent Off changed child copy")
	}
	cr[2].Disabled = "true"
	c = a55apply(t, p, db, "C", 4, cr...)
	run(w, c)
	if c.Turns[0].Off || !c.Turns[0].Responses[1].Off || m.calls() != 2 {
		t.Fatal("reply Off did not persist or produce partial source")
	}
	if strings.Contains(m.prompts[1], "REPLY: two") {
		t.Fatal("disabled reply in prompt")
	}
	cr[1].Disabled = "true"
	c = a55apply(t, p, db, "C", 5, cr...)
	run(w, c)
	if got := a28Bodies(t, db, c); got != "" || m.calls() != 2 {
		t.Fatalf("all off current=%q calls=%d", got, m.calls())
	}
	cr[1].Disabled = ""
	cr[2].Disabled = ""
	c = a55apply(t, p, db, "C", 6, cr...)
	run(w, c)
	if a55scope(t, db, c)[0].Source.Key() != original || m.calls() != 2 {
		t.Fatal("reenabling full source did not reuse original parent job")
	}
	cr[0].Disabled = "true"
	c = a55apply(t, p, db, "C", 7, cr...)
	run(w, c)
	if got := a28Bodies(t, db, c); got != "" || !c.Turns[0].Off || m.calls() != 2 {
		t.Fatal("user Off failed")
	}
	t.Logf("A55: C/B controls full/partial/all-off/on/user-off passed, calls=%d", m.calls())
}

func TestA55LateDirectMarker(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	heard := 0
	p.Changed = func(s *lifecycle.Session) { heard++ }
	ar := []lifecycle.Row{row("au", "user", "", "q1"), row("aa", "char", "g1", "one")}
	a55apply(t, p, db, "A", 1, ar...)
	br := []lifecycle.Row{row("bu1", "user", "", "q1"), row("ba1", "char", "g1", "one"), marker("A", "aa", "bma"), row("bu2", "user", "", "q2"), row("ba2", "char", "g2", "two")}
	a55apply(t, p, db, "B", 2, br...)
	gr := []lifecycle.Row{row("gu1", "user", "", "q1"), row("ga1", "char", "g1", "one"), marker("A", "aa", "gma"), row("gu2", "user", "", "q2"), row("ga2", "char", "g2", "two")}
	own := []lifecycle.Row{row("gu3", "user", "", "q2"), row("ga3", "char", "g3", "two"), row("gu4", "user", "", "next")}
	g := a55apply(t, p, db, "G", 3, append(append([]lifecycle.Row{}, gr...), own...)...)
	ids := []int{}
	for _, tr := range g.ActiveTurns() {
		ids = append(ids, tr.ID)
	}
	later := append(append(append([]lifecycle.Row{}, gr...), marker("B", "ba2", "gmb")), own...)
	g = a55apply(t, p, db, "G", 4, later...)
	sc := a55scope(t, db, g)
	if g.CopiesOf != [2]string{"chat:B", "c:ba2"} || !sc[0].Inherited || !sc[1].Inherited || sc[2].Inherited {
		t.Fatalf("late direct scope %+v", sc)
	}
	for i, tr := range g.ActiveTurns() {
		if tr.ID != ids[i] {
			t.Fatal("turn ids changed")
		}
	}
	before := heard
	g = a55apply(t, p, db, "G", 4, later...)
	if heard != before {
		t.Fatal("resent observation emitted Changed")
	}
	g = a55apply(t, pipeline.New(db), db, "G", 4, later...)
	sc = a55scope(t, db, g)
	if len(g.Copies) != 2 || sc[2].Inherited {
		t.Fatal("restart changed boundary")
	}
	t.Logf("A55: late parent=%v copies=%d own turn=%d resent Changed delta=%d", g.CopiesOf, len(g.Copies), sc[2].Source.Turn, heard-before)
}

func TestA55Cleanup(t *testing.T) {
	db, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err = db.QueryRow("SELECT VERSION()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT schema_name FROM information_schema.schemata WHERE LEFT(schema_name,5) IN ('a55g_','a55s_')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	t.Logf("A55: MariaDB=%s remaining owned schemas=%v", version, names)
	if len(names) != 0 {
		t.Fatal(fmt.Sprint(names))
	}
}
