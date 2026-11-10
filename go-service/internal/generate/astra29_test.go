package generate

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestA29SameTextRevision(t *testing.T) {
	db := testDB(t)
	w := NewWorker(db, &fakeModel{}, "fake", time.Second)
	p := lifecycle.NewSession("chat:A")
	original := []lifecycle.Row{row("u1", "user", "", "질문"), row("a1", "char", "g1", "같은 대답")}
	p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, original...))
	goOn(p, original...)
	a28Save(t, db, p)
	run(w, p)
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceLoad, append(original, a28Marker("A", "a1"))...))
	a28Save(t, db, c)
	before, e := db.CurrentKnown(c)
	if e != nil {
		t.Fatal(e)
	}
	p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "질문"), row("a1", "char", "g2", "같은 대답")))
	goOn(p, row("u1", "user", "", "질문"), row("a1", "char", "g2", "같은 대답"))
	a28Save(t, db, p)
	run(w, p)
	after, e := db.CurrentKnown(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("parent revisions=%+v before=%+v after=%+v", p.ActiveTurns()[0].Responses[0].Revisions, before.Memories, after.Memories)
	if len(before.Memories) != 1 || len(after.Memories) != 1 {
		t.Fatal("fixture memory missing")
	}
	if before.Memories[0].ID != after.Memories[0].ID {
		t.Error("unchanged child switched to parent's later same-text revision")
	}
}

func TestA29BranchDate(t *testing.T) {
	db := testDB(t)
	w := NewWorker(db, &fakeModel{}, "fake", time.Second)
	p := lifecycle.NewSession("chat:A")
	original := []lifecycle.Row{row("u1", "user", "", "질문"), row("a1", "char", "g1", "옛 대답")}
	sn := a28Snap(p.Key, lifecycle.SourceOutputEvent, original...)
	sn.Dates = map[string]string{"clock": "1423/05/15"}
	p.Apply(sn)
	goOn(p, original...)
	a28Save(t, db, p)
	run(w, p)
	c := lifecycle.NewSession("chat:C")
	cn := a28Snap(c.Key, lifecycle.SourceLoad, append(original, a28Marker("A", "a1"))...)
	cn.Dates = map[string]string{"clock": "1423/05/15"}
	c.Apply(cn)
	a28Save(t, db, c)
	before, e := db.CurrentKnown(c)
	if e != nil {
		t.Fatal(e)
	}
	sn = a28Snap(p.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "질문"), row("a1", "char", "g2", "새 대답"))
	sn.Dates = map[string]string{"clock": "1423/05/16"}
	p.Apply(sn)
	goOn(p, row("u1", "user", "", "질문"), row("a1", "char", "g2", "새 대답"))
	a28Save(t, db, p)
	run(w, p)
	after, e := db.CurrentKnown(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("before=%+v after=%+v", before.Memories, after.Memories)
	if len(before.Memories) != 1 || len(after.Memories) != 1 {
		t.Fatal("fixture memory missing")
	}
	if before.Memories[0].Date != after.Memories[0].Date {
		t.Error("parent reroll rewrites unchanged child's inherited clock")
	}
}

func TestA29PeopleControls(t *testing.T) {
	for _, v := range []string{`["서진"]`, `[{"who":"서진"}]`, `[{"name":"서진"}]`, `"서진"`} {
		out, notes := memory.Parse(`{"memories":[{"kind":"fact","body":"비밀","hide_from":` + v + `}]}`)
		if len(out.Memories) != 1 || len(out.Memories[0].HideFrom) != 1 || out.Memories[0].HideFrom[0] != "서진" {
			t.Fatalf("%s -> %+v %v", v, out, notes)
		}
	}
	out, notes := memory.Parse(`{"memories":[{"kind":"fact","body":"비밀","hide_from":[{"who":"N2","name":"서진","except":"금고 위치만 공개"}]}]}`)
	t.Logf("object with qualifier: %+v notes=%v", out.Memories, notes)
}

func TestA29LiteralNames(t *testing.T) {
	for _, name := range []string{"Mina", "Rina", "Lena", "Sarah"} {
		m := memory.KnownMemory{Memory: memory.Memory{Kind: "view", Owner: name, Body: "금고를 두려워한다"}}
		d := memory.DocText(m, map[string]string{})
		t.Logf("owner=%s document=%s", name, d)
		if !strings.Contains(d, name) {
			t.Errorf("literal owner %s lost", name)
		}
	}
}

func TestA29TextBoundary(t *testing.T) {
	p := lifecycle.NewSession("chat:A")
	p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "첫 입력\nREPLY: 인용"), row("a1", "char", "g1", "대답")))
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "첫 입력"), row("a1", "char", "g2", "인용\nREPLY: 대답")))
	got := memory.Inherit(memory.Own(p), "c:a1", memory.Own(c))
	t.Logf("parent=%+v child=%+v scope=%+v", memory.Own(p), memory.Own(c), got)
	if len(got) != 1 || got[0].Source.Session != c.Key {
		t.Error("formatted text equality conflates distinct input/reply boundaries")
	}
}

func TestA29SavedGLMProtection(t *testing.T) {
	b, e := os.ReadFile(os.Getenv("A29_GLM_FIXTURE"))
	if e != nil {
		t.Skip("A29_GLM_FIXTURE (saved glm answers) not set")
	}
	var fixture struct {
		Jobs []struct {
			Turn     int
			Response string
		}
	}
	if e = json.Unmarshal(b, &fixture); e != nil {
		t.Fatal(e)
	}
	var raw string
	for _, j := range fixture.Jobs {
		if j.Turn == 1 {
			raw = j.Response
		}
	}
	if raw == "" {
		t.Fatal("missing saved T1")
	}
	out, notes := memory.Parse(raw)
	db := testDB(t)
	j, e := db.JobFor(memory.Source{Session: "replay", Turn: 1, Responses: []memory.SourceResponse{{Key: "a", Seq: 1, Data: "saved T1"}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.SaveGeneration(j, store.Generation{Output: out, Response: raw, Notes: notes}); e != nil {
		t.Fatal(e)
	}
	k, e := db.KnownOf([]int64{j.ID})
	if e != nil {
		t.Fatal(e)
	}
	sel := memory.Select(k, "태블릿을 감춘 한얼", nil)
	rendered := memory.RenderKnown(sel, memory.Superseded(k))
	t.Logf("parse notes=%v stored awareness=%+v rendered protection=%v", notes, k.Awareness, strings.Contains(rendered, "hide_from: 조선 사람들"))
	if !strings.Contains(rendered, "hide_from: 조선 사람들") {
		t.Fatal("saved model protection missing after parse/store/select/render")
	}
}
