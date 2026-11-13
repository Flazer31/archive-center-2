package main

import (
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// wlChat is a chat of n turns (rows u<i>, a<i>) after the rows first;
// change rewrites a row (role, id, generation, text, disabled) by its turn
// and role.
func wlChat(n int, change func(turn int, role string, id, gen, text, disabled *string), first ...string) []string {
	rows := append([]string{}, first...)
	for t := 1; t <= n; t++ {
		for _, role := range []string{"user", "char"} {
			id, gen, text, off := fmt.Sprintf("u%d", t), "", fmt.Sprintf("입력 %d", t), ""
			if role == "char" {
				id, gen, text = fmt.Sprintf("a%d", t), fmt.Sprintf("g%d", t), fmt.Sprintf("답 %d", t)
			}
			if change != nil {
				change(t, role, &id, &gen, &text, &off)
			}
			rows = append(rows, fmt.Sprintf(`{"i":%d,"role":%q,"chat_id":%q,"generation_id":%q,"disabled":%q,"data":{"text":%q}}`, len(rows), role, id, gen, off, text))
			if role == "char" && extra != nil {
				if more := extra(t); more != "" { // a second part of the reply
					rows = append(rows, fmt.Sprintf(`{"i":%d,"role":"char","chat_id":"%s-2","generation_id":%q,"data":{"text":%q}}`, len(rows), id, gen, more))
				}
			}
		}
	}
	return rows
}

func wlNodes(t *testing.T, a memAnswer) []map[string]any {
	t.Helper()
	if a.Code != 200 {
		t.Fatalf("worldline %d %s", a.Code, a.Raw)
	}
	var out []map[string]any
	for _, n := range a.V["nodes"].([]any) {
		out = append(out, n.(map[string]any))
	}
	return out
}

// extra, when set, gives a turn a second reply part (empty: none).
var extra func(turn int) string

// The worldline map: the first turn, the newest turns and the turn a branch
// splits at are shown; the rest is one fold until it is opened. A turn shows
// its other versions (a reroll, an edit), whether it is turned off, and its
// memories, numbered as the memory screen numbers them; a branch shows the
// parent turn it split at and its copied turns as the parent's.
func TestWorldlineMap(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	greeting := `{"i":0,"role":"char","chat_id":"gr","data":{"text":"인사"}}`
	off := func(turn int, role string, id, gen, text, disabled *string) {
		if turn == 5 && role == "user" && *disabled == "x" {
			*disabled = "true"
		}
		if *disabled == "x" {
			*disabled = ""
		}
	}
	snap := func(seq int, change func(turn int, role string, id, gen, text, disabled *string)) {
		t.Helper()
		if _, err := pipe.Process([]observe.Record{memEvent(seq, "load", "A", wlChat(30, change, greeting)...)}); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { extra = nil }()
	extra = func(turn int) string {
		if turn == 8 {
			return "이어 쓴 답 8"
		}
		return ""
	}
	snap(1, nil)
	snap(2, func(turn int, role string, id, gen, text, disabled *string) {
		if turn == 9 && role == "char" {
			*text = "잠깐 고친 답 9"
		}
	})
	extra = nil // turn 8's second part deleted
	// turn 3 rerolled (a new reply row), turn 4's reply edited, turn 9's
	// edit undone (its earlier text is the current one), turn 5 turned off
	// after its memory was made (below)
	snap(3, func(turn int, role string, id, gen, text, disabled *string) {
		if turn == 3 && role == "char" {
			*id, *gen, *text = "a3x", "g3x", "다시 쓴 답 3"
		}
		if turn == 4 && role == "char" {
			*text = "고친 답 4"
		}
	})
	// B branches from A at turn 10's reply
	b := wlChat(10, nil, greeting)
	b = append(b, `{"i":21,"role":"char","chat_id":"mk","is_comment":true,"disabled":"true","data":{"text":"{{specialcomment::branchedfrom::A::이름::a10::}}"}}`,
		`{"i":22,"role":"user","chat_id":"bu","data":{"text":"분기 입력"}}`)
	if _, err := pipe.Process([]observe.Record{memEvent(5, "beforeRequest", "B", b...)}); err != nil {
		t.Fatal(err)
	}

	var turns []memory.ScopedTurn
	pipe.WithSession("chat:A", func(s *lifecycle.Session) { turns = memory.Own(s) })
	if len(turns) != 30 || turns[4].Off {
		t.Fatalf("fixture: %d turns", len(turns))
	}
	save := func(turn int, raw string) {
		j, err := db.JobFor(turns[turn-1].Source)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := memory.Parse(raw)
		if _, err := db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
			t.Fatal(err)
		}
	}
	save(2, `{"memories":[{"id":"m1","kind":"fact","body":"하나"},{"id":"m2","kind":"event","body":"둘"}]}`)
	save(5, `{"memories":[{"id":"m1","kind":"fact","body":"꺼진 턴"}]}`)
	save(6, `{"memories":[{"id":"m1","kind":"fact","body":"꺼진 턴 다음"}]}`)
	snap(4, func(turn int, role string, id, gen, text, disabled *string) {
		*disabled = "x"
		off(turn, role, id, gen, text, disabled)
		if turn == 3 && role == "char" {
			*id, *gen, *text = "a3x", "g3x", "다시 쓴 답 3"
		}
		if turn == 4 && role == "char" {
			*text = "고친 답 4"
		}
	})
	pipe.WithSession("chat:A", func(s *lifecycle.Session) { turns = memory.Own(s) })
	if !turns[4].Off {
		t.Fatal("fixture: turn 5 is not off")
	}

	h := worldlineHandler(pipe, db)
	nodes := wlNodes(t, callMem(t, h, "GET", "/worldline?chat_id=A", ""))
	var got []string
	for _, n := range nodes {
		if f, ok := n["fold"].([]any); ok {
			got = append(got, fmt.Sprintf("fold%v-%v", f[0], f[1]))
		} else {
			got = append(got, fmt.Sprint(n["turn"]))
		}
	}
	want := []string{"1", "fold2-9", "10"}
	for i := 11; i <= 30; i++ {
		want = append(want, fmt.Sprint(i))
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("map\n got %v\nwant %v", got, want)
	}
	if br := nodes[2]["branches"].([]any); len(br) != 1 || br[0].(map[string]any)["chat"] != "B" || br[0].(map[string]any)["turns"].(float64) != 11 {
		t.Fatalf("branch at turn 10: %v", nodes[2])
	}

	open := wlNodes(t, callMem(t, h, "GET", "/worldline?chat_id=A&open=2-9", ""))
	if len(open) != 30 {
		t.Fatalf("opened: %d nodes", len(open))
	}
	at := func(n int) map[string]any { return open[n-1] }
	if at(2)["memories"].(float64) != 2 || at(6)["memories"].(float64) != 1 || at(5)["off"] != true || at(5)["memories"] != nil ||
		// only the newest reply is kept: a reroll, an edit and an undone edit
		// leave no other versions
		at(3)["versions"] != nil || at(4)["versions"] != nil || at(1)["versions"] != nil || at(4)["reply"] != "고친 답 4" ||
		at(8)["versions"] != nil || at(8)["reply"] != "답 8" || at(9)["versions"] != nil || at(3)["reply"] != "다시 쓴 답 3" {
		t.Fatalf("turns: %v | %v | %v | %v | %v | %v | %v", at(2), at(3), at(4), at(5), at(6), at(8), at(9))
	}

	detail := func(n int) memAnswer {
		return callMem(t, h, "GET", fmt.Sprintf("/worldline/turn?chat_id=A&turn=%d", n), "")
	}
	noVersions := func(v any) bool { return v == nil || fmt.Sprint(v) == "[]" }
	if d := detail(3); d.V["reply"] != "다시 쓴 답 3" || !noVersions(d.V["versions"]) || strings.Contains(d.Raw, "\"답 3\"") {
		t.Fatalf("turn 3 (the rerolled reply must be gone): %s", d.Raw)
	}
	if d := detail(4); d.V["reply"] != "고친 답 4" || !noVersions(d.V["versions"]) {
		t.Fatalf("turn 4 (the text before the edit must be gone): %s", d.Raw)
	}
	if d := detail(31); d.Code != 404 || d.V["code"] != "turn_not_found" {
		t.Fatalf("turn 31: %d %s", d.Code, d.Raw)
	}

	// the memory screen numbers turns as the map does (the turn turned off
	// keeps its place)
	mh := memoriesHandler(pipe, db, nil, nil)
	if l := listed(t, callMem(t, mh, "GET", "/memory/list?chat_id=A&turn=6", "")); len(l) != 1 || l[0]["body"] != "꺼진 턴 다음" {
		t.Fatalf("turn 6 memories: %v", l)
	}
	if l := listed(t, callMem(t, mh, "GET", "/memory/list?chat_id=A&turn=2", "")); len(l) != 2 {
		t.Fatalf("turn 2 memories: %v", l)
	}
	if l := listed(t, callMem(t, mh, "GET", "/memory/list?chat_id=A&turn=5", "")); len(l) != 0 {
		t.Fatalf("the turn turned off: %v", l)
	}

	bv := callMem(t, h, "GET", "/worldline?chat_id=B", "")
	bn := wlNodes(t, bv)
	if p := bv.V["parent"].(map[string]any); p["chat"] != "A" || p["turn"].(float64) != 10 {
		t.Fatalf("parent %v", bv.V["parent"])
	}
	if len(bn) != 11 || bn[9]["inherited"] != true || bn[10]["inherited"] != nil || bn[1]["memories"].(float64) != 2 {
		t.Fatalf("branch map %v", bn)
	}
}

// A long branch keeps the turns where it leaves its parent on the map, and
// an opened range shows at most worldlineOpenMax turns (the newest of it).
func TestWorldlineFoldsALongChat(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "A", wlChat(40, nil)...)}); err != nil {
		t.Fatal(err)
	}
	b := wlChat(40, nil)
	b = append(b, `{"i":80,"role":"char","chat_id":"mk","is_comment":true,"disabled":"true","data":{"text":"{{specialcomment::branchedfrom::A::이름::a40::}}"}}`)
	for i := 41; i <= 120; i++ {
		b = append(b, fmt.Sprintf(`{"i":%d,"role":"user","chat_id":"bu%d","data":{"text":"분기 %d"}}`, len(b), i, i),
			fmt.Sprintf(`{"i":%d,"role":"char","chat_id":"ba%d","generation_id":"bg%d","data":{"text":"분기 답 %d"}}`, len(b)+1, i, i, i))
	}
	if _, err := pipe.Process([]observe.Record{memEvent(2, "load", "B", b...)}); err != nil {
		t.Fatal(err)
	}
	h := worldlineHandler(pipe, db)
	shape := func(path string) string {
		var got []string
		for _, n := range wlNodes(t, callMem(t, h, "GET", path, "")) {
			if f, ok := n["fold"].([]any); ok {
				got = append(got, fmt.Sprintf("fold%v-%v", f[0], f[1]))
			} else {
				got = append(got, fmt.Sprint(n["turn"]))
			}
		}
		return strings.Join(got, " ")
	}
	recent := func(from int) string {
		var s []string
		for i := from; i <= 120; i++ {
			s = append(s, fmt.Sprint(i))
		}
		return strings.Join(s, " ")
	}
	if got, want := shape("/worldline?chat_id=B"), "1 fold2-39 40 41 fold42-100 "+recent(101); got != want {
		t.Fatalf("branch\n got %s\nwant %s", got, want)
	}
	// opening the fold 42-100 shows its newest 50 turns
	if got, want := shape("/worldline?chat_id=B&open=42-100"), "1 fold2-39 40 41 fold42-50 "+recent(51); got != want {
		t.Fatalf("opened\n got %s\nwant %s", got, want)
	}
	// one range is open at a time: opening another folds the first again, so the
	// map stays short however far it is opened
	var early []string
	for i := 1; i <= 41; i++ {
		early = append(early, fmt.Sprint(i))
	}
	if got, want := shape("/worldline?chat_id=B&open=2-39"), strings.Join(early, " ")+" fold42-100 "+recent(101); got != want {
		t.Fatalf("another range\n got %s\nwant %s", got, want)
	}
	if got, want := shape("/worldline?chat_id=B&open=2-39,42-100"), "1 fold2-39 40 41 fold42-100 "+recent(101); got != want {
		t.Fatalf("two ranges\n got %s\nwant %s", got, want)
	}
}
