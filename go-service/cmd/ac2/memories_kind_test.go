package main

import (
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// The memory screen by kind (user 10-28: items apart, like AC1): kind keeps
// one kind; facts come grouped in the prompt's mode order and views by whose
// they are, each group the latest turn first; kinds counts every kind among
// what the search keeps, whichever kind is shown.
func TestMemoryScreenByKind(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "첫째"), memRow(2, "user", "u2", "", "둘"), memRow(3, "char", "a2", "g2", "둘째")}
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "K", rows...)}); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	pipe.WithSession("chat:K", func(s *lifecycle.Session) {
		for _, tr := range s.ActiveTurns() {
			srcs = append(srcs, memory.SourceOf(s.Key, tr))
		}
	})
	save := func(src memory.Source, raw string) {
		j, err := db.JobFor(src)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := memory.Parse(raw)
		if _, err := db.SaveGeneration(j, store.Generation{Request: "r", ReadItems: "[]", Response: raw, Output: out}); err != nil {
			t.Fatal(err)
		}
	}
	save(srcs[0], `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[
	 {"id":"m1","kind":"fact","about":"n1","mode":"의무","body":"리나는 매일 보고한다"},
	 {"id":"m2","kind":"fact","about":"n1","mode":"상태","body":"리나는 약사다"},
	 {"id":"m3","kind":"view","owner":"n2","about":"n1","aspect":"belief","body":"도윤은 리나를 믿는다"},
	 {"id":"m4","kind":"view","owner":"n1","about":"n2","aspect":"emotion","body":"리나는 도윤이 고맙다"},
	 {"id":"m5","kind":"event","body":"리나가 왔다"}]}`)
	save(srcs[1], `{"memories":[
	 {"id":"m1","kind":"fact","about":"N1","mode":"상태","body":"리나는 지쳤다"},
	 {"id":"m2","kind":"fact","about":"N1","mode":"엉뚱함","body":"리나는 모른다"},
	 {"id":"m3","kind":"view","owner":"N1","about":"N2","aspect":"goal","body":"리나는 떠나려 한다"}]}`)
	h := memoriesHandler(pipe, db, nil, nil)

	bodies := func(path string) ([]string, map[string]any) {
		a := callMem(t, h, "GET", path, "")
		var out []string
		for _, m := range listed(t, a) {
			out = append(out, m["body"].(string))
		}
		return out, a.V["kinds"].(map[string]any)
	}
	same := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	facts, kinds := bodies("/memory/list?chat_id=K&kind=fact")
	if want := []string{"리나는 지쳤다", "리나는 약사다", "리나는 매일 보고한다", "리나는 모른다"}; !same(facts, want) {
		t.Fatalf("facts by mode then latest turn: %v, want %v", facts, want)
	}
	if kinds["event"].(float64) != 1 || kinds["fact"].(float64) != 4 || kinds["view"].(float64) != 3 {
		t.Fatalf("kinds %v", kinds)
	}
	views, _ := bodies("/memory/list?chat_id=K&kind=view")
	if want := []string{"도윤은 리나를 믿는다", "리나는 떠나려 한다", "리나는 도윤이 고맙다"}; !same(views, want) {
		t.Fatalf("views by owner then latest turn: %v, want %v", views, want)
	}
	if events, _ := bodies("/memory/list?chat_id=K&kind=event"); !same(events, []string{"리나가 왔다"}) {
		t.Fatalf("events %v", events)
	}
	all, _ := bodies("/memory/list?chat_id=K")
	if want := []string{"리나는 지쳤다", "리나는 모른다", "리나는 떠나려 한다", "리나가 왔다", "리나는 매일 보고한다", "리나는 약사다", "도윤은 리나를 믿는다", "리나는 도윤이 고맙다"}; !same(all, want) {
		t.Fatalf("no kind: the latest turn first, a turn's events, facts, then views: %v, want %v", all, want)
	}
	// the counts follow the search, not the kind shown
	found, kinds := bodies("/memory/list?chat_id=K&kind=event&q=도윤")
	if len(found) != 0 || kinds["view"].(float64) != 3 || kinds["fact"].(float64) != 0 {
		t.Fatalf("search counts %v %v", found, kinds)
	}
}
