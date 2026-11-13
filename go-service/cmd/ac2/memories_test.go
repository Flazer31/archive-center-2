package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

func memRow(i int, role, id, gen, text string) string {
	return fmt.Sprintf(`{"i":%d,"role":%q,"chat_id":%q,"generation_id":%q,"data":{"text":%q}}`, i, role, id, gen, text)
}

func memEvent(seq int, hook, chat string, rows ...string) observe.Record {
	return observe.Record{Event: json.RawMessage(fmt.Sprintf(`{"boot_id":"b","seq":%d,"hook":%q,"stored_chat":{"chat_id":%q,"rows":[%s]}}`, seq, hook, chat, strings.Join(rows, ",")))}
}

type memAnswer struct {
	Code int
	V    map[string]any
	Raw  string
}

func callMem(t *testing.T, h http.HandlerFunc, method, path, body string) memAnswer {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var v map[string]any
	json.Unmarshal(rec.Body.Bytes(), &v)
	return memAnswer{rec.Code, v, rec.Body.String()}
}

func listed(t *testing.T, a memAnswer) []map[string]any {
	t.Helper()
	if a.Code != 200 {
		t.Fatalf("list %d %s", a.Code, a.Raw)
	}
	var out []map[string]any
	for _, m := range a.V["memories"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

// The memory screen: a chat's memories, the latest turn first, found by a
// word; a body or importance edited is what the next injection delivers and
// what the next index embeds; a deleted memory is delivered no more, with
// its effects and the chat's awareness of it; a branch lists its parent's
// memories as inherited and changes none of them.
func TestMemoryScreen(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	a := []string{memRow(0, "user", "u1", "", "리나를 만났다"), memRow(1, "char", "a1", "g1", "첫째"), memRow(2, "user", "u2", "", "둘"), memRow(3, "char", "a2", "g2", "둘째")}
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "A", a...)}); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	pipe.WithSession("chat:A", func(s *lifecycle.Session) {
		for _, tr := range s.ActiveTurns() {
			srcs = append(srcs, memory.SourceOf(s.Key, tr))
		}
	})
	if len(srcs) != 2 {
		t.Fatalf("turns %d", len(srcs))
	}
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
	save(srcs[0], `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"리나는 약사다","imp":4},{"id":"m2","kind":"event","body":"리나가 돈을 냈다","effects":[{"about":"n1","slot":"돈","before":"10냥","value":"7냥"}],"hide_from":["n2"]}]}`)
	save(srcs[1], `{"memories":[{"id":"m1","kind":"fact","about":"N1","slot":"기분","body":"리나는 기분이 좋다"}]}`)

	w := generate.NewWorker(db, nil, "", time.Minute)
	w.Index = func(context.Context, []int64) error { return nil } // not run here: the test reads what is queued
	h := memoriesHandler(pipe, db, w, nil)

	list := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=A", ""))
	if len(list) != 3 || list[0]["turn"].(float64) != 2 || list[2]["turn"].(float64) != 1 || list[0]["inherited"] != false {
		t.Fatalf("list %v", list)
	}
	var job, paid map[string]any
	for _, m := range list {
		switch m["body"] {
		case "리나는 약사다":
			job = m
		case "리나가 돈을 냈다":
			paid = m
		}
	}
	if job == nil || paid == nil || job["about"] != "리나" || paid["secret"] != true || job["secret"] != false || len(paid["effects"].([]any)) != 1 {
		t.Fatalf("listed fields: %v / %v", job, paid)
	}
	if found := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=A&q=약사", "")); len(found) != 1 || found[0]["id"] != job["id"] {
		t.Fatalf("search %v", found)
	}

	// the next injection delivers what is stored
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.After = pipe
	inject := injectHandler(srv, pipe, db, func() *provider.Embedder { return nil }, nil)
	fp := func(s string) map[string]any {
		n, hh := ingest.Fingerprint(s)
		return map[string]any{"len": n, "h": hh}
	}
	reqBody, _ := json.Marshal(map[string]any{"chat_id": "A", "input": "리나에게 묻는다", "payload": []any{fp("둘"), fp("둘째"), fp("리나에게 묻는다")}})
	block := func() string {
		rec := httptest.NewRecorder()
		inject(rec, httptest.NewRequest(http.MethodPost, "/memory/inject", strings.NewReader(string(reqBody))))
		var d struct{ Block string }
		json.Unmarshal(rec.Body.Bytes(), &d)
		return d.Block
	}
	if b := block(); !strings.Contains(b, "리나는 약사다") {
		t.Fatalf("before the edit %q", b)
	}

	id := job["id"].(string)
	for _, c := range []struct {
		body, code string
		status     int
	}{
		{`{"chat_id":"A","id":"` + id + `","body":"  "}`, "empty_body", 400},
		{`{"chat_id":"A","id":"` + id + `","imp":11}`, "bad_importance", 400},
		{`{"chat_id":"A","id":"M999999"}`, "memory_not_found", 404},
		{`{"chat_id":"Z","id":"` + id + `","body":"x"}`, "memory_not_found", 404},
		{`{"chat_id":"A"`, "bad_request", 400},
	} {
		if a := callMem(t, h, "POST", "/memory/edit", c.body); a.Code != c.status || a.V["code"] != c.code {
			t.Fatalf("%s: %d %s", c.body, a.Code, a.Raw)
		}
	}
	if a := callMem(t, h, "POST", "/memory/edit", `{"chat_id":"A","id":"`+id+`","body":"리나는 의사다","imp":9}`); a.Code != 200 {
		t.Fatalf("edit %d %s", a.Code, a.Raw)
	}
	if b := block(); !strings.Contains(b, "리나는 의사다") || strings.Contains(b, "약사") {
		t.Fatalf("after the edit %q", b)
	}
	if found := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=A&q=의사", "")); len(found) != 1 || found[0]["imp"].(float64) != 9 {
		t.Fatalf("edited %v", found)
	}
	// the search words the reading range looks up are the new body's
	var newWords, oldWords int
	words := dbSQL(t, db)
	words.QueryRow(`SELECT COUNT(*) FROM search_words WHERE item=? AND word LIKE '%의사%'`, id).Scan(&newWords)
	words.QueryRow(`SELECT COUNT(*) FROM search_words WHERE item=? AND word LIKE '%약사%'`, id).Scan(&oldWords)
	if newWords == 0 || oldWords != 0 {
		t.Fatalf("search words after the edit: new %d old %d", newWords, oldWords)
	}
	if d := w.Diagnostics(); d["generation_waiting_chats"] != 1 || d["index_backlog_chats"] != 1 {
		t.Fatalf("the edited chat was not sent to be indexed: %v", d)
	}

	// a branch of A at its first turn reads A's first turn as inherited
	marker := `{"i":2,"role":"char","chat_id":"mk","is_comment":true,"disabled":true,"data":{"text":"{{specialcomment::branchedfrom::A::이름::a1::}}"}}`
	if _, err := pipe.Process([]observe.Record{memEvent(2, "beforeRequest", "B", memRow(0, "user", "xb", "", "리나를 만났다"), memRow(1, "char", "yb", "g1", "첫째"), marker, memRow(3, "user", "zb", "", "분기 입력"))}); err != nil {
		t.Fatal(err)
	}
	branch := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=B", ""))
	if len(branch) != 2 || branch[0]["inherited"] != true || branch[0]["body"] == nil {
		t.Fatalf("branch list %v", branch)
	}
	for _, path := range []string{"/memory/edit", "/memory/delete", "/memory/hold"} {
		if a := callMem(t, h, "POST", path, `{"chat_id":"B","id":"`+id+`","body":"분기에서 고침","hold":"pin"}`); a.Code != 403 || a.V["code"] != "inherited_read_only" {
			t.Fatalf("%s in the branch: %d %s", path, a.Code, a.Raw)
		}
	}
	if found := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=A&q=의사", "")); len(found) != 1 {
		t.Fatalf("the branch changed the parent: %v", found)
	}
	// C18 flags: pin | mute | "" on the chat's own memory, shown by the list
	for _, c := range []struct {
		body, code string
		status     int
	}{
		{`{"chat_id":"A","id":"` + id + `","hold":"always"}`, "bad_hold", 400},
		{`{"chat_id":"A","id":"M999999","hold":"pin"}`, "memory_not_found", 404},
		{`{"chat_id":"Z","id":"` + id + `","hold":"pin"}`, "memory_not_found", 404},
		{`{"chat_id":"A","hold":"pin"}`, "bad_request", 400},
	} {
		if a := callMem(t, h, "POST", "/memory/hold", c.body); a.Code != c.status || a.V["code"] != c.code {
			t.Fatalf("hold %s: %d %s", c.body, a.Code, a.Raw)
		}
	}
	for _, hold := range []string{"pin", "mute", ""} {
		if a := callMem(t, h, "POST", "/memory/hold", `{"chat_id":"A","id":"`+id+`","hold":"`+hold+`"}`); a.Code != 200 || a.V["changed"] != true {
			t.Fatalf("hold %q: %d %s", hold, a.Code, a.Raw)
		}
		found := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=A&q=의사", ""))
		if got, _ := found[0]["hold"].(string); got != hold {
			t.Fatalf("listed hold %q, set %q", got, hold)
		}
		if b := block(); (hold == "mute") == strings.Contains(b, "리나는 의사다") {
			t.Fatalf("hold %q block %q", hold, b)
		}
	}

	// delete: gone from the list and the injection, with its effects and
	// the chat's awareness of it
	pid := paid["id"].(string)
	if a := callMem(t, h, "POST", "/memory/delete", `{"chat_id":"A","id":"`+pid+`"}`); a.Code != 200 {
		t.Fatalf("delete %d %s", a.Code, a.Raw)
	}
	if a := callMem(t, h, "POST", "/memory/delete", `{"chat_id":"A","id":"`+pid+`"}`); a.Code != 404 {
		t.Fatalf("deleted twice %d %s", a.Code, a.Raw)
	}
	after := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=A", ""))
	if len(after) != 2 {
		t.Fatalf("after delete %v", after)
	}
	if b := block(); strings.Contains(b, "돈을 냈다") || !strings.Contains(b, "의사") {
		t.Fatalf("after the delete %q", b)
	}
	raw := dbSQL(t, db)
	n := strings.TrimPrefix(pid, "M")
	for _, q := range []string{
		`SELECT COUNT(*) FROM effects WHERE memory_id=` + n,
		`SELECT COUNT(*) FROM awareness WHERE target='` + pid + `' OR target LIKE '` + pid + `.%'`,
		`SELECT COUNT(*) FROM search_words WHERE item='` + pid + `'`,
		`SELECT COUNT(*) FROM search_refs WHERE item='` + pid + `'`,
	} {
		var c int
		if err := raw.QueryRow(q).Scan(&c); err != nil || c != 0 {
			t.Fatalf("%s: %d %v", q, c, err)
		}
	}
}

// A branch's copied turn the parent never made is made as the branch's own:
// the memory screen lists it as the branch's (not inherited) and lets it be
// edited, as delivery reads it.
func TestBranchOwnMemoryOnTheScreen(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "A", memRow(0, "user", "u1", "", "리나를 만났다"), memRow(1, "char", "a1", "g1", "첫째"))}); err != nil {
		t.Fatal(err)
	}
	marker := `{"i":2,"role":"char","chat_id":"mk","is_comment":true,"disabled":true,"data":{"text":"{{specialcomment::branchedfrom::A::이름::a1::}}"}}`
	if _, err := pipe.Process([]observe.Record{memEvent(2, "beforeRequest", "B", memRow(0, "user", "ub", "", "리나를 만났다"), memRow(1, "char", "ab", "g1", "첫째"), marker, memRow(3, "user", "next", "", "리나에게 묻는다"))}); err != nil {
		t.Fatal(err)
	}
	var scope []memory.ScopedTurn
	pipe.WithSession("chat:B", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	if len(scope) == 0 || !scope[0].Inherited || scope[0].OwnSource().Session != "chat:B" {
		t.Fatalf("fixture: %+v", scope)
	}
	j, err := db.JobFor(scope[0].OwnSource()) // as the worker makes it
	if err != nil {
		t.Fatal(err)
	}
	out, _ := memory.Parse(`{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"리나는 약사다"}]}`)
	if _, err := db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
		t.Fatal(err)
	}
	h := memoriesHandler(pipe, db, nil, nil)
	list := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=B", ""))
	if len(list) != 1 || list[0]["inherited"] != false || list[0]["body"] != "리나는 약사다" {
		t.Fatalf("branch's own memory %v", list)
	}
	if a := callMem(t, h, "POST", "/memory/edit", `{"chat_id":"B","id":"`+list[0]["id"].(string)+`","body":"리나는 의사다"}`); a.Code != 200 {
		t.Fatalf("edit %d %s", a.Code, a.Raw)
	}
	// the worldline counts it on the branch's turn, as the list reads it
	if n := wlNodes(t, callMem(t, worldlineHandler(pipe, db), "GET", "/worldline?chat_id=B", "")); len(n) == 0 || n[0]["memories"] != float64(1) {
		t.Fatalf("worldline count %v", n)
	}
}

// An edit or a delete leaves the turn's memories without a vector until the
// next index: none of them is ranked by a vector made from the old texts
// (a contextualized vector is the turn's memories together; R02, R03).
func TestEditAndDeleteUnlinkTheTurnsVectors(t *testing.T) {
	db := modelsDB(t)
	src := memory.Source{Session: "chat:V", Turn: 1}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"첫 기록"},{"id":"m2","kind":"fact","body":"둘째 기록"},{"id":"m3","kind":"fact","body":"셋째 기록"}]}`)
	if _, err := db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
		t.Fatal(err)
	}
	other, _ := db.JobFor(memory.Source{Session: "chat:V", Turn: 2})
	o2, _ := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"다른 턴"}]}`)
	db.SaveGeneration(other, store.Generation{Output: o2})
	raw := dbSQL(t, db)
	link := func() {
		if _, err := raw.Exec(`UPDATE memories SET doc_hash=SHA2(CONCAT('h', id), 256)`); err != nil {
			t.Fatal(err)
		}
	}
	unlinked := func() (turn, rest int) {
		raw.QueryRow(`SELECT COUNT(*) FROM memories WHERE job_id=? AND doc_hash=''`, j.ID).Scan(&turn)
		raw.QueryRow(`SELECT COUNT(*) FROM memories WHERE job_id<>? AND doc_hash=''`, j.ID).Scan(&rest)
		return
	}
	k, _ := db.KnownOf([]int64{j.ID})
	link()
	body := "첫 기록, 고침"
	if err := db.EditMemory("chat:V", k.Memories[0].ID, &body, nil); err != nil {
		t.Fatal(err)
	}
	if turn, rest := unlinked(); turn != 3 || rest != 0 {
		t.Fatalf("after the edit: turn %d other turns %d", turn, rest)
	}
	link()
	if err := db.DeleteMemory("chat:V", k.Memories[1].ID); err != nil {
		t.Fatal(err)
	}
	if turn, rest := unlinked(); turn != 2 || rest != 0 {
		t.Fatalf("after the delete: turn %d other turns %d", turn, rest)
	}
}

// A delete alone (no edit before it) asks the chat to be indexed again: its
// turn's other memories wait for their vectors.
func TestDeleteAsksForAnIndex(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "D", memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "첫째"))}); err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	pipe.WithSession("chat:D", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.ActiveTurns()[0]) })
	j, _ := db.JobFor(src)
	out, _ := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"지울 기록"},{"id":"m2","kind":"fact","body":"남을 기록"}]}`)
	if _, err := db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
		t.Fatal(err)
	}
	w := generate.NewWorker(db, nil, "", time.Minute)
	w.Index = func(context.Context, []int64) error { return nil } // not run here
	h := memoriesHandler(pipe, db, w, nil)
	list := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=D", ""))
	if d := w.Diagnostics(); d["index_backlog_chats"] != 0 {
		t.Fatalf("asked before the delete: %v", d)
	}
	if a := callMem(t, h, "POST", "/memory/delete", `{"chat_id":"D","id":"`+list[1]["id"].(string)+`"}`); a.Code != 200 {
		t.Fatalf("delete %d %s", a.Code, a.Raw)
	}
	if d := w.Diagnostics(); d["index_backlog_chats"] != 1 || d["generation_waiting_chats"] != 1 {
		t.Fatalf("the delete did not ask for an index: %v", d)
	}
}
