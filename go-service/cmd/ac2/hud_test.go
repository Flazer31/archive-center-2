package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"archivecenter2/goservice/internal/embed"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
)

func hudFixture(t *testing.T, modelURL string, retries int) *hud {
	t.Helper()
	db := modelsDB(t)
	p := pipeline.New(db)
	w := generate.NewWorker(db, nil, "", time.Minute)
	box, err := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	m := &models{db: db, worker: w, box: box, flagRetries: retries, flagEmbed: "voyage-4-large", envKey: "synthetic-key"}
	if modelURL != "" {
		m.flagCritic = provider.Config{Provider: "ollama", Model: "fake", URL: modelURL}
	}
	m.apply(store.ModelSettings{Retries: retries}, false)
	st := newStatus(db, m, w)
	w.Event = st.workerEvent
	h := newHUD(db, p, w, st)
	p.Changed = func(s *lifecycle.Session) { w.Notify(s.Key, memory.Own(s)) }
	return h
}

func hudGet(t *testing.T, h *hud, chat string) map[string]any {
	t.Helper()
	a := callMem(t, h.handler(), "GET", "/hud?chat_id="+chat, "")
	if a.Code != 200 {
		t.Fatalf("HUD %d %s", a.Code, a.Raw)
	}
	return a.V
}

func hudTurnView(t *testing.T, v map[string]any, turn int) map[string]any {
	t.Helper()
	for _, raw := range v["turns"].([]any) {
		row := raw.(map[string]any)
		if int(row["turn"].(float64)) == turn {
			return row
		}
	}
	t.Fatalf("turn %d absent: %v", turn, v)
	return nil
}

func hudStageView(t *testing.T, row map[string]any, key string) map[string]any {
	t.Helper()
	for _, raw := range row["stages"].([]any) {
		s := raw.(map[string]any)
		if s["key"] == key {
			return s
		}
	}
	t.Fatalf("stage %s absent", key)
	return nil
}

func hudWait(t *testing.T, fn func() bool) {
	t.Helper()
	end := time.Now().Add(8 * time.Second)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("HUD condition did not arrive")
}

func hudRun(t *testing.T, w *generate.Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	})
}

func TestHUDConcurrentChats(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	releaseModel := func() { once.Do(func() { close(release) }) }
	defer releaseModel()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if r.URL.Path != "/api/chat" || json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Messages) == 0 {
			t.Error("unexpected synthetic model request")
			http.Error(w, "unexpected input", 400)
			return
		}
		prompt := request.Messages[len(request.Messages)-1].Content
		if !strings.Contains(prompt, "alpha evidence") && !strings.Contains(prompt, "beta evidence") {
			t.Error("unexpected synthetic prompt")
			http.Error(w, "unexpected prompt", 400)
			return
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[{"id":"m1","kind":"fact","body":"synthetic memory"}]}`}})
	}))
	defer func() { releaseModel(); model.Close() }()
	h := hudFixture(t, model.URL, 0)
	for i, chat := range []string{"HUD-A", "HUD-B"} {
		body := []string{"alpha evidence", "beta evidence"}[i]
		u, a := memRow(0, "user", "u1", "", body), memRow(1, "char", "a1", "g1", body)
		for seq, rows := range [][]string{{u}, {u, a}, {u, a, memRow(2, "user", "u2", "", "next")}} {
			if _, err := h.pipe.Process([]observe.Record{memEvent(i*3+seq+1, "beforeRequest", chat, rows...)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	hudRun(t, h.worker)
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("both HUD chats did not start together")
		}
	}
	v := getStatus(t, h.st)
	g := v["generation"].(map[string]any)
	if g["running_chats"] != float64(2) || fmt.Sprint(g["running_sessions"]) != "[chat:HUD-A chat:HUD-B]" {
		t.Fatalf("global running chats missing: %v", g)
	}
	for _, chat := range []string{"HUD-A", "HUD-B"} {
		view := hudGet(t, h, chat)
		first := view["turns"].([]any)[1].(map[string]any)
		if stage := hudStageView(t, first, "generation"); stage["state"] != "running" {
			t.Fatalf("%s running HUD absent: %v", chat, stage)
		}
		if getStatus(t, h.st, chat)["generation"].(map[string]any)["chat"].(map[string]any)["running"] != true {
			t.Fatal("per-chat status absent")
		}
	}
	releaseModel()
	hudWait(t, func() bool { return h.worker.Diagnostics()["generation_running_chats"] == 0 })
}

// Real pipeline, injection HTTP handler, generation worker, provider HTTP and
// MariaDB. Only the model and vector provider are synthetic external boundaries.
func TestHUDNormalStagesAndInjection(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected model path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[{"id":"m1","kind":"fact","body":"리나는 약사다"}]}`}})
	}))
	defer model.Close()
	var releaseOnce sync.Once
	releaseModel := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseModel()
	h := hudFixture(t, model.URL, 0)
	user := memRow(0, "user", "u1", "", "리나를 만난다")
	reply := memRow(1, "char", "a1", "g1", "리나는 약사다")
	apply := func(seq int, rows ...string) {
		t.Helper()
		if _, err := h.pipe.Process([]observe.Record{memEvent(seq, "beforeRequest", "HUD", rows...)}); err != nil {
			t.Fatal(err)
		}
	}
	apply(1, user)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	inject := injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st)
	a := callMem(t, inject, "POST", "/memory/inject", `{"chat_id":"HUD","input":"リナ","payload":[]}`)
	if a.Code != 200 {
		t.Fatal(a.Raw)
	}
	row := hudTurnView(t, hudGet(t, h, "HUD"), 1)
	i := row["injection"].(map[string]any)
	ids, _ := a.V["ids"].([]any)
	if i["items"] != float64(len(ids)) {
		t.Fatalf("injection count %v", i)
	}
	chars, secret, _ := h.db.MemoryBudgets()
	if i["chars_max"] != float64(chars) || i["secret_max"] != float64(secret) || i["warning"] != a.V["warning"] || i["total_ms"] != a.V["total_ms"] {
		t.Fatalf("injection not actual result: %v / %v", i, a.V)
	}
	apply(2, user, reply)
	if s := hudStageView(t, hudTurnView(t, hudGet(t, h, "HUD"), 1), "confirmed"); s["state"] != "waiting" {
		t.Fatalf("confirmed before next input %v", s)
	}
	apply(3, user, reply, memRow(2, "user", "u2", "", "다음"))
	var indexed atomic.Int32
	h.worker.Index = func(ctx context.Context, jobs []int64) error {
		if len(jobs) != 1 {
			return fmt.Errorf("unexpected jobs %v", jobs)
		}
		indexed.Add(1)
		return nil
	}
	hudRun(t, h.worker)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider not entered")
	}
	row = hudTurnView(t, hudGet(t, h, "HUD"), 1)
	if s := hudStageView(t, row, "generation"); s["state"] != "running" || s["at"] == nil || s["elapsed_ms"] == nil {
		t.Fatalf("during generation %v", s)
	}
	releaseModel()
	hudWait(t, func() bool {
		return hudStageView(t, hudTurnView(t, hudGet(t, h, "HUD"), 1), "vectors")["state"] == "done"
	})
	view := hudGet(t, h, "HUD")
	row = hudTurnView(t, view, 1)
	keys := []string{"observed", "injected", "response_saved", "confirmed", "generation", "vectors"}
	for index, raw := range row["stages"].([]any) {
		s := raw.(map[string]any)
		if s["key"] != keys[index] || s["state"] != "done" || s["at"] == nil {
			t.Fatalf("stage order/state %v", row)
		}
	}
	if gen := hudStageView(t, row, "generation"); gen["count"] != float64(indexed.Load()) || gen["elapsed_ms"] == nil {
		t.Fatalf("stored count/time %v", gen)
	}
	if view["active"] != false || len(view["turns"].([]any)) != 2 {
		t.Fatalf("did not settle: %v", view)
	}
	if current := row["current_stage"].(map[string]any); current["key"] != "vectors" || current["state"] != "done" {
		t.Fatalf("summary did not follow completed stages: %v", current)
	}
	if other := hudGet(t, h, "OTHER"); len(other["turns"].([]any)) != 0 {
		t.Fatal("cross-chat HUD leak")
	}
}

func TestHUDFailureRetryAndRestart(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "synthetic failure", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[{"id":"m1","kind":"fact","body":"하나"}]}`}})
	}))
	defer model.Close()
	h := hudFixture(t, model.URL, 0)
	if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "FAIL", memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "답"), memRow(2, "user", "u2", "", "둘"))}); err != nil {
		t.Fatal(err)
	}
	// Another failed job must retain its attempt count after a turn retry.
	other := memory.Source{Session: "chat:OTHER", Turn: 1, Members: []memory.SourceMember{{Data: "다른 입력"}}, Responses: []memory.SourceResponse{{Seq: 1, Data: "다른 답"}}}
	j, err := h.db.JobFor(other)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.StartAttempt(j); err != nil {
		t.Fatal(err)
	}
	if err = h.db.FailJob(j, "fake", "", "", "other failure", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool { return hudTurnView(t, hudGet(t, h, "FAIL"), 1)["retry"] == true })
	row := hudTurnView(t, hudGet(t, h, "FAIL"), 1)
	if gen := hudStageView(t, row, "generation"); gen["state"] != "failed" || !strings.Contains(gen["detail"].(string), "503") {
		t.Fatalf("lost failure reason %v", gen)
	}
	// Recreate HUD as after a backend restart: reason comes from the job,
	// unrecorded timing is null, and no GET creates or retries a job.
	h.mu.Lock()
	h.chats = map[string]*hudChat{}
	h.mu.Unlock()
	row = hudTurnView(t, hudGet(t, h, "FAIL"), 1)
	if hudStageView(t, row, "generation")["elapsed_ms"] != nil || row["retry"] != true || calls.Load() != 1 {
		t.Fatalf("restart projection %v calls %d", row, calls.Load())
	}
	a := callMem(t, h.handler(), "POST", "/hud/retry", `{"chat_id":"FAIL","turn":1}`)
	if a.Code != 200 {
		t.Fatal(a.Raw)
	}
	hudWait(t, func() bool {
		return hudStageView(t, hudTurnView(t, hudGet(t, h, "FAIL"), 1), "generation")["state"] == "done"
	})
	if calls.Load() != 2 {
		t.Fatalf("retry calls %d", calls.Load())
	}
	unchanged, err := h.db.JobFor(other)
	if err != nil || unchanged.Attempts != j.Attempts+1 {
		t.Fatalf("other job reset %v %v", unchanged, err)
	}
}

func TestHUDLifecycleNoticesAndSettings(t *testing.T) {
	h := hudFixture(t, "", 0)
	user := memRow(0, "user", "u1", "", "하나")
	reply := memRow(1, "char", "a1", "g1", "원래 답")
	apply := func(seq int, rows ...string) {
		t.Helper()
		if _, err := h.pipe.Process([]observe.Record{memEvent(seq, "beforeRequest", "LIFE", rows...)}); err != nil {
			t.Fatal(err)
		}
	}
	apply(1, user, reply)
	apply(2, user)
	apply(3, user, memRow(1, "char", "a2", "g2", "새 답"))
	apply(4, memRow(0, "user", "u1", "", "수정 입력"), memRow(1, "char", "a2", "g2", "새 답"))
	apply(5, strings.Replace(memRow(0, "user", "u1", "", "수정 입력"), `"role":"user"`, `"role":"user","disabled":"true"`, 1), memRow(1, "char", "a2", "g2", "새 답"))
	seen := map[string]bool{}
	view := hudGet(t, h, "LIFE")
	for _, raw := range view["notices"].([]any) {
		seen[raw.(map[string]any)["kind"].(string)] = true
	}
	for _, kind := range []string{"response_deleted", "response_replaced", "member_edited", "turn_off"} {
		if !seen[kind] {
			t.Fatalf("notice %s missing: %v", kind, seen)
		}
	}
	if row := hudTurnView(t, view, 1); row["off"] != true || hudStageView(t, row, "generation")["state"] != "off" {
		t.Fatalf("OOC %v", row)
	}
	if current := hudTurnView(t, view, 1)["current_stage"].(map[string]any); current["key"] != "generation" || current["state"] != "off" {
		t.Fatalf("OOC summary %v", current)
	}
	a := callMem(t, h.handler(), "POST", "/settings/hud", `{"enabled":false,"compact":false}`)
	if a.Code != 200 {
		t.Fatal(a.Raw)
	}
	read, err := h.db.HUDSettings()
	if err != nil || read.Enabled || read.Compact {
		t.Fatalf("not persisted %v %v", read, err)
	}
	// A new settings reader and GET use the same persisted settings.
	if got := callMem(t, h.handler(), "GET", "/settings/hud", ""); got.V["enabled"] != false || got.V["compact"] != false {
		t.Fatal(got.Raw)
	}
	if bad := callMem(t, h.handler(), "POST", "/settings/hud", `{"enabled":`); bad.Code != 400 {
		t.Fatal(bad.Raw)
	}
	if got := callMem(t, h.handler(), "POST", "/hud", ""); got.Code != 405 {
		t.Fatal(got.Raw)
	}
}

func TestHUDVectorRecoveryAndLateRevisionTiming(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[{"id":"m1","kind":"fact","body":"하나"}]}`}})
	}))
	defer model.Close()
	h := hudFixture(t, model.URL, 0)
	user := memRow(0, "user", "u1", "", "하나")
	reply := memRow(1, "char", "a1", "g1", "답")
	apply := func(seq int, rows ...string) {
		t.Helper()
		if _, err := h.pipe.Process([]observe.Record{memEvent(seq, "beforeRequest", "VECTOR", rows...)}); err != nil {
			t.Fatal(err)
		}
	}
	apply(1, user, reply, memRow(2, "user", "u2", "", "둘"))
	var attempts atomic.Int32
	h.worker.Index = func(ctx context.Context, jobs []int64) error {
		if attempts.Add(1) == 1 {
			return fmt.Errorf("synthetic vector failure")
		}
		return nil
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool {
		return hudStageView(t, hudTurnView(t, hudGet(t, h, "VECTOR"), 1), "vectors")["state"] == "retry_waiting"
	})
	view := hudGet(t, h, "VECTOR")
	if view["active"] != true || hudStageView(t, hudTurnView(t, view, 1), "vectors")["detail"] != "synthetic vector failure" /* the error text as it was (11-08) */ {
		t.Fatalf("vector wait %v", view)
	}
	h.worker.Reindex("chat:VECTOR")
	hudWait(t, func() bool {
		return hudStageView(t, hudTurnView(t, hudGet(t, h, "VECTOR"), 1), "vectors")["state"] == "done"
	})
	if attempts.Load() != 2 {
		t.Fatalf("vector calls %d", attempts.Load())
	}
	// A late completion of the old revision cannot paint its replacement.
	h.worker.Configure(nil, "", 0)
	var old memory.Source
	h.pipe.WithSession("chat:VECTOR", func(s *lifecycle.Session) { old = memory.SourceOf(s.Key, s.Turns[0]) })
	apply(2, user, memRow(1, "char", "a1", "g1", "수정한 답"), memRow(2, "user", "u2", "", "둘"))
	h.progress(generate.Progress{Kind: "generation_finished", Session: old.Session, Turn: old.Turn, Source: old.Key()})
	row := hudTurnView(t, hudGet(t, h, "VECTOR"), old.Turn)
	if gen := hudStageView(t, row, "generation"); gen["state"] == "done" || gen["elapsed_ms"] != nil {
		t.Fatalf("old completion painted new revision: %v", gen)
	}
}

func TestHUDAutomaticRetryWaiting(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "synthetic failure", 503) }))
	defer model.Close()
	h := hudFixture(t, model.URL, store.DefaultRetries)
	if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "WAIT", memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "답"), memRow(2, "user", "u2", "", "둘"))}); err != nil {
		t.Fatal(err)
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool { return hudTurnView(t, hudGet(t, h, "WAIT"), 1)["retry"] == true })
	view := hudGet(t, h, "WAIT")
	row := hudTurnView(t, view, 1)
	if gen := hudStageView(t, row, "generation"); gen["state"] != "retry_waiting" || row["next_at"] == nil || view["active"] != true {
		t.Fatalf("retry wait not shown: %v", view)
	}
}

type hudFakeEmbed struct{ calls int }

func (e *hudFakeEmbed) Embed(ctx context.Context, texts []string, inputType string) (provider.Embedding, error) {
	if inputType != "document" || len(texts) == 0 {
		return provider.Embedding{}, fmt.Errorf("unexpected vector request %s %v", inputType, texts)
	}
	e.calls++
	res := provider.Embedding{}
	for _, text := range texts {
		if !strings.Contains(text, "하나") {
			return res, fmt.Errorf("unexpected indexed document %q", text)
		}
		res.Vectors = append(res.Vectors, []float32{1, 0, 0})
	}
	return res, nil
}
func (e *hudFakeEmbed) EmbedGroups(context.Context, [][]string, string) (provider.GroupEmbedding, error) {
	return provider.GroupEmbedding{}, fmt.Errorf("unexpected contextual vector request")
}

func TestHUDPersistedVectorsAndInheritedJob(t *testing.T) {
	h := hudFixture(t, "", 0)
	user := memRow(0, "user", "u1", "", "하나")
	reply := memRow(1, "char", "a1", "g1", "하나 답")
	if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "PARENT", user, reply, memRow(2, "user", "u2", "", "둘"))}); err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	h.pipe.WithSession("chat:PARENT", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.Turns[0]) })
	j, err := h.db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"memories":[{"id":"m1","kind":"fact","body":"하나 기록"}]}`
	out, _ := memory.Parse(raw)
	if _, err = h.db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.chats = map[string]*hudChat{}
	h.mu.Unlock()
	row := hudTurnView(t, hudGet(t, h, "PARENT"), src.Turn)
	if s := hudStageView(t, row, "vectors"); s["state"] == "done" {
		t.Fatalf("saved memory invented vector success %v", s)
	}
	e := &hudFakeEmbed{}
	if _, err = embed.Index(context.Background(), h.db, e, h.st.mods.query().VectorModel(), []int64{j.ID}, 64); err != nil {
		t.Fatal(err)
	}
	row = hudTurnView(t, hudGet(t, h, "PARENT"), src.Turn)
	if s := hudStageView(t, row, "vectors"); s["state"] != "done" || s["at"] != nil || s["elapsed_ms"] != nil || e.calls != 1 {
		t.Fatalf("persisted vectors %v calls %d", s, e.calls)
	}
	marker := `{"i":2,"role":"char","chat_id":"mk","is_comment":true,"disabled":"true","data":{"text":"{{specialcomment::branchedfrom::PARENT::이름::a1::}}"}}`
	if _, err = h.pipe.Process([]observe.Record{memEvent(2, "beforeRequest", "CHILD", user, reply, marker, memRow(3, "user", "cu", "", "분기 입력"))}); err != nil {
		t.Fatal(err)
	}
	row = hudTurnView(t, hudGet(t, h, "CHILD"), src.Turn)
	if s := hudStageView(t, row, "generation"); s["state"] != "done" || s["count"] != float64(len(out.Memories)) {
		t.Fatalf("inherited job was not used %v", s)
	}
}
