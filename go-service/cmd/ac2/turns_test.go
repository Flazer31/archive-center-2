package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

func u3Fixture(t *testing.T) (*store.DB, *pipeline.Pipeline, []memory.Source) {
	t.Helper()
	db := modelsDB(t)
	pipe := pipeline.New(db)
	rows := []string{}
	for i := 1; i <= 4; i++ {
		rows = append(rows, memRow(2*i-2, "user", fmt.Sprintf("u%d", i), "", fmt.Sprintf("입력 %d\n둘째 줄", i)), memRow(2*i-1, "char", fmt.Sprintf("a%d", i), fmt.Sprintf("g%d", i), fmt.Sprintf("현재 응답 %d", i)))
	}
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "U3", rows...)}); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	if err := pipe.WithSession("chat:U3", func(s *lifecycle.Session) {
		for _, v := range memory.Own(s) {
			srcs = append(srcs, v.Source)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return db, pipe, srcs
}

func u3Save(t *testing.T, db *store.DB, src memory.Source, raw string) store.Job {
	t.Helper()
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	out, notes := memory.Parse(raw)
	if _, err = db.SaveGeneration(j, store.Generation{Request: "synthetic request", ReadItems: "[]", Response: raw, Output: out, Notes: notes}); err != nil {
		t.Fatal(err)
	}
	return j
}

type u3Model struct {
	mu      sync.Mutex
	prompts []string
	first   chan struct{}
	release chan struct{}
}

func (m *u3Model) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	m.mu.Lock()
	m.prompts = append(m.prompts, prompt)
	n := len(m.prompts)
	m.mu.Unlock()
	if n == 1 && m.first != nil {
		close(m.first)
		select {
		case <-m.release:
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
	}
	return provider.Result{Content: fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","body":"새 작업 %d"}]}`, n)}, nil
}
func (m *u3Model) calls() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.prompts) }

func u3Wait(t *testing.T, done func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker did not finish")
}

func TestU3TurnsAndRemake(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	old := u3Save(t, db, srcs[0], `{"memories":[{"id":"m1","kind":"fact","body":"이전 작업"},{"id":"m2","kind":"view","body":"지울 관점"}]}`)
	u3Save(t, db, srcs[1], `{"memories":[{"id":"m1","kind":"fact","body":"다른 턴"}]}`)
	j, err := db.JobFor(srcs[2])
	if err != nil {
		t.Fatal(err)
	}
	if err = db.FailJob(j, "fake", "r", "[]", "synthetic failure", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	m := &u3Model{}
	worker := generate.NewWorker(db, m, "recording-fake", time.Second)
	h := turnsHandler(pipe, db, worker, nil)
	read := func(path string) []chatTurn {
		a := callMem(t, h, "GET", path, "")
		if a.Code != 200 {
			t.Fatal(a.Raw)
		}
		var v struct{ Turns []chatTurn }
		if err := json.Unmarshal([]byte(a.Raw), &v); err != nil {
			t.Fatal(err)
		}
		return v.Turns
	}
	all := read("/chats/turns?chat_id=U3&limit=4")
	if len(all) != len(srcs) {
		t.Fatalf("turns %v", all)
	}
	states := []string{"made", "made", "failed", "none"}
	for i, v := range all {
		if v.Turn != i+1 || v.User != srcs[i].Members[0].Data || v.Reply != srcs[i].Responses[0].Data || v.State != states[i] {
			t.Fatalf("turn %d: %+v", i, v)
		}
	}
	if all[0].Memories != 2 || all[1].Memories != 1 {
		t.Fatalf("counts %+v", all)
	}
	page := read("/chats/turns?chat_id=U3&offset=1&limit=1")
	if len(page) != 1 || page[0] != all[1] {
		t.Fatalf("page %+v", page)
	}
	if a := callMem(t, h, "POST", "/memory/turn/clear", `{"chat_id":"U3","turn":1}`); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	listedAll := listed(t, callMem(t, memoriesHandler(pipe, db, nil, nil), "GET", "/memory/list?chat_id=U3", ""))
	if len(listedAll) != 1 || listedAll[0]["body"] != "다른 턴" {
		t.Fatalf("clear affected another turn: %v", listedAll)
	}
	// a cleared turn is made again (user 10-29): a new job waits for it
	job, err := db.JobFor(srcs[0])
	if err != nil || job.ID == old.ID || job.State != "requested" {
		t.Fatalf("clear did not remake the turn: %+v %v", job, err)
	}
	cleared := job.ID
	if a := callMem(t, h, "POST", "/memory/turn/remake", `{"chat_id":"U3","turn":1}`); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	job, err = db.JobFor(srcs[0])
	if err != nil || job.ID == old.ID || job.ID == cleared || job.State != "requested" {
		t.Fatalf("no new job: %+v %v", job, err)
	}
	if v := read("/chats/turns?chat_id=U3&limit=1"); v[0].State != "waiting" {
		t.Fatal(v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })
	u3Wait(t, func() bool { j, e := db.JobFor(srcs[0]); return e == nil && j.State == "done" })
	if m.calls() != 1 {
		t.Fatalf("model calls %d", m.calls())
	}
	m.mu.Lock()
	prompt := m.prompts[0]
	m.mu.Unlock()
	if !strings.Contains(prompt, srcs[0].Members[0].Data) || !strings.Contains(prompt, srcs[0].Responses[0].Data) {
		t.Fatalf("not the requested source: %q", prompt)
	}
	listedAll = listed(t, callMem(t, memoriesHandler(pipe, db, nil, nil), "GET", "/memory/list?chat_id=U3", ""))
	if len(listedAll) != 2 || listedAll[1]["body"] != "새 작업 1" {
		t.Fatalf("remake %+v", listedAll)
	}
	jobs, err := db.CurrentJobs(srcs[:2])
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range jobs {
		if id == old.ID {
			t.Fatal("retired job still current")
		}
	}
	if a := callMem(t, h, "POST", "/memory/turn/clear", `{"chat_id":"U3","turn":99}`); a.Code != 404 {
		t.Fatal(a.Raw)
	}
}

func TestU3RemakeReplacesCurrentMemory(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	old := u3Save(t, db, srcs[0], `{"memories":[{"id":"m1","kind":"fact","body":"old current result"}]}`)
	m := &u3Model{}
	worker := generate.NewWorker(db, m, "fake", time.Second)
	h := turnsHandler(pipe, db, worker, nil)
	if a := callMem(t, h, "POST", "/memory/turn/remake", `{"chat_id":"U3","turn":1}`); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	end := make(chan struct{})
	go func() { defer close(end); worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-end })
	u3Wait(t, func() bool { j, e := db.JobFor(srcs[0]); return e == nil && j.State == "done" })
	all := listed(t, callMem(t, memoriesHandler(pipe, db, nil, nil), "GET", "/memory/list?chat_id=U3", ""))
	if len(all) != 1 || all[0]["body"] != "새 작업 1" || m.calls() != 1 {
		t.Fatalf("old result kept: %v calls %d", all, m.calls())
	}
	it, err := db.ItemsOf([]int64{old.ID})
	if err != nil || len(it.Memories) != 1 || it.Memories[0].Body != "old current result" {
		t.Fatalf("history %+v %v", it, err)
	}
}

func TestU3RemakeDuringCallKeepsNewGrant(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	m := &u3Model{first: make(chan struct{}), release: make(chan struct{})}
	worker := generate.NewWorker(db, m, "fake", time.Second)
	h := turnsHandler(pipe, db, worker, nil)
	if a := callMem(t, h, "POST", "/memory/turn/remake", `{"chat_id":"U3","turn":1}`); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	end := make(chan struct{})
	go func() { defer close(end); worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-end })
	select {
	case <-m.first:
	case <-time.After(3 * time.Second):
		t.Fatal("first call missing")
	}
	old, _ := db.JobFor(srcs[0])
	if a := callMem(t, h, "POST", "/memory/turn/remake", `{"chat_id":"U3","turn":1}`); a.Code != 200 {
		close(m.release)
		t.Fatal(a.Raw)
	}
	close(m.release)
	u3Wait(t, func() bool { j, e := db.JobFor(srcs[0]); return e == nil && j.ID != old.ID && j.State == "done" })
	if m.calls() != 2 {
		t.Fatalf("calls %d", m.calls())
	}
	all := listed(t, callMem(t, memoriesHandler(pipe, db, nil, nil), "GET", "/memory/list?chat_id=U3", ""))
	if len(all) != 1 || all[0]["body"] != "새 작업 2" {
		t.Fatalf("late old answer became current %v", all)
	}
}

func TestU3BulkDelete(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"memories":[{"id":"m1","kind":"fact","body":"one"},{"id":"m2","kind":"event","body":"two","effects":[{"slot":"x","value":"y"}]},{"id":"m3","kind":"view","body":"keep"}]}`)
	ids, err := db.ItemIDs(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := memoriesHandler(pipe, db, nil, nil)
	body := func(want []string) string {
		b, _ := json.Marshal(map[string]any{"chat_id": "U3", "ids": want})
		return string(b)
	}
	if a := callMem(t, h, "POST", "/memory/delete", body([]string{ids["memories/0"], "M999999999"})); a.Code != 404 {
		t.Fatal(a.Raw)
	}
	if all := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=U3", "")); len(all) != len(ids) {
		t.Fatalf("partial bulk delete %v", all)
	}
	if a := callMem(t, h, "POST", "/memory/delete", body([]string{ids["memories/0"], ids["memories/1"], ids["memories/0"]})); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	all := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=U3", ""))
	if len(all) != 1 || all[0]["id"] != ids["memories/2"] {
		t.Fatalf("bulk delete %v", all)
	}
}

func TestU3InheritedManagementRemainsReadOnly(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	parent := u3Save(t, db, srcs[0], `{"entities":[{"id":"n1","name":"부모 인물"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"부모 기억"}]}`)
	marker := `{"i":2,"role":"char","chat_id":"marker","is_comment":true,"disabled":true,"data":{"text":"{{specialcomment::branchedfrom::U3::이름::a1::}}"}}`
	if _, err := pipe.Process([]observe.Record{memEvent(2, "beforeRequest", "child", memRow(0, "user", "bu", "", srcs[0].Members[0].Data), memRow(1, "char", "ba", "g1", srcs[0].Responses[0].Data), marker, memRow(3, "user", "cu", "", "분기 입력"), memRow(4, "char", "ca", "cg", "분기 응답"))}); err != nil {
		t.Fatal(err)
	}
	var child memory.Source
	if err := pipe.WithSession("chat:child", func(s *lifecycle.Session) { child = memory.Own(s)[1].OwnSource() }); err != nil {
		t.Fatal(err)
	}
	own := u3Save(t, db, child, `{"entities":[{"id":"n1","name":"분기 인물"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"분기 기억"}]}`)
	parentIDs, _ := db.ItemIDs(parent.ID)
	ownIDs, _ := db.ItemIDs(own.ID)
	h := turnsHandler(pipe, db, nil, nil)
	for _, path := range []string{"/memory/turn/clear", "/memory/turn/remake"} {
		a := callMem(t, h, "POST", path, `{"chat_id":"child","turn":1}`)
		if a.Code != 403 || a.V["code"] != "inherited_read_only" {
			t.Fatalf("%s: %s", path, a.Raw)
		}
	}
	a := callMem(t, h, "GET", "/chats/turns?chat_id=child&offset=0&limit=1", "")
	var v struct{ Turns []chatTurn }
	json.Unmarshal([]byte(a.Raw), &v)
	if a.Code != 200 || len(v.Turns) != 1 || !v.Turns[0].Inherited || v.Turns[0].Memories != 1 {
		t.Fatal(a.Raw)
	}
	names := namesHandler(pipe, db, nil)
	for _, body := range []string{fmt.Sprintf(`{"chat_id":"child","id":%q,"name":"교체"}`, parentIDs["entities/0"])} {
		a = callMem(t, names, "POST", "/names/edit", body)
		if a.Code != 403 || a.V["code"] != "inherited_read_only" {
			t.Fatal(a.Raw)
		}
	}
	a = callMem(t, names, "POST", "/names/merge", fmt.Sprintf(`{"chat_id":"child","keep":%q,"merge":%q}`, ownIDs["entities/0"], parentIDs["entities/0"]))
	if a.Code != 403 || a.V["code"] != "inherited_read_only" {
		t.Fatal(a.Raw)
	}
	a = callMem(t, memoriesHandler(pipe, db, nil, nil), "POST", "/memory/delete", fmt.Sprintf(`{"chat_id":"child","ids":[%q,%q]}`, ownIDs["memories/0"], parentIDs["memories/0"]))
	if a.Code != 403 || a.V["code"] != "inherited_read_only" {
		t.Fatal(a.Raw)
	}
	all := listed(t, callMem(t, memoriesHandler(pipe, db, nil, nil), "GET", "/memory/list?chat_id=child", ""))
	if len(all) != 2 {
		t.Fatalf("mixed inherited delete removed own memory: %v", all)
	}
}

// Deleting from the memory list: a turn left with none of its memories is
// made again (user 10-29); a turn that keeps some is not.
func TestDeleteEmptyingATurnRemakesIt(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	first := u3Save(t, db, srcs[0], `{"memories":[{"id":"m1","kind":"fact","body":"첫 턴 하나"},{"id":"m2","kind":"view","body":"첫 턴 둘"}]}`)
	second := u3Save(t, db, srcs[1], `{"memories":[{"id":"m1","kind":"fact","body":"둘째 턴 하나"},{"id":"m2","kind":"fact","body":"둘째 턴 둘"}]}`)
	h := memoriesHandler(pipe, db, nil, nil)
	byBody := map[string]string{}
	for _, m := range listed(t, callMem(t, h, "GET", "/memory/list?chat_id=U3", "")) {
		byBody[m["body"].(string)] = m["id"].(string)
	}
	body, _ := json.Marshal(map[string]any{"chat_id": "U3", "ids": []string{byBody["첫 턴 하나"], byBody["첫 턴 둘"], byBody["둘째 턴 하나"]}})
	a := callMem(t, h, "POST", "/memory/delete", string(body))
	if a.Code != 200 {
		t.Fatal(a.Raw)
	}
	if r, _ := a.V["remade"].([]any); len(r) != 1 || r[0] != float64(1) {
		t.Fatalf("remade turns: %s", a.Raw)
	}
	if j, err := db.JobFor(srcs[0]); err != nil || j.ID == first.ID || j.State != "requested" {
		t.Fatalf("emptied turn not made again: %+v %v", j, err)
	}
	if j, err := db.JobFor(srcs[1]); err != nil || j.ID != second.ID || j.State != "done" {
		t.Fatalf("turn with memories left was remade: %+v %v", j, err)
	}
}
