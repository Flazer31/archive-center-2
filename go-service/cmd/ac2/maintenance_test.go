package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/embed"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
)

type maintenanceFixture struct {
	m       *maintenance
	mu      sync.Mutex
	texts   []string
	fail    bool
	block   <-chan struct{}
	entered chan<- struct{}
	jobs    map[string][]int64
}

func newMaintenanceFixture(t *testing.T, model string) *maintenanceFixture {
	t.Helper()
	f := &maintenanceFixture{jobs: map[string][]int64{}}
	db := modelsDB(t)
	box, err := secret.LoadBox(filepath.Join(t.TempDir(), "synthetic-secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string          `json:"model"`
			Input  json.RawMessage `json:"input"`
			Inputs [][]string      `json:"inputs"`
			Type   string          `json:"input_type"`
		}
		path := "/embeddings"
		contextual := strings.HasPrefix(model, "voyage-context-")
		if contextual {
			path = "/contextualizedembeddings"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != model || req.Type != "document" || r.URL.Path != path {
			t.Errorf("unexpected synthetic embedding request: path=%s model=%s type=%s", r.URL.Path, req.Model, req.Type)
			http.Error(w, "unexpected request", 400)
			return
		}
		var texts []string
		if contextual {
			if len(req.Inputs) != 1 {
				t.Error("expected one contextual turn")
				http.Error(w, "unexpected groups", 400)
				return
			}
			texts = req.Inputs[0]
		} else if err := json.Unmarshal(req.Input, &texts); err != nil {
			t.Error(err)
			http.Error(w, "bad input", 400)
			return
		}
		f.mu.Lock()
		f.texts = append(f.texts, texts...)
		fail, block, entered := f.fail, f.block, f.entered
		f.mu.Unlock()
		if entered != nil {
			entered <- struct{}{}
		}
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		if fail {
			http.Error(w, "synthetic failure", 503)
			return
		}
		data := []map[string]any{}
		for i := range texts {
			data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0, 0}})
		}
		if contextual {
			data = []map[string]any{{"index": 0, "data": data}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]int{"total_tokens": len(texts)}})
	}))
	t.Cleanup(fake.Close)
	w := generate.NewWorker(db, nil, "", time.Minute)
	mods := &models{db: db, box: box, worker: w}
	mods.apply(store.ModelSettings{CriticSet: true, Retries: store.DefaultRetries, EmbedProvider: "voyageai", EmbedModel: model, EmbedURL: fake.URL, EmbedKey: "synthetic-not-a-real-key"}, false)
	if err := db.SetModelSettings(box, mods.saved); err != nil {
		t.Fatal(err)
	}
	w.Index = func(ctx context.Context, jobs []int64) error {
		v := mods.query()
		if v == nil {
			return nil
		}
		_, err := embed.Index(ctx, db, v, v.VectorModel(), jobs, 64)
		return err
	}
	pipe := pipeline.New(db)
	pipe.Changed = func(s *lifecycle.Session) { w.Notify(s.Key, memory.Own(s)) }
	pipe.Label = func(key, char, chat string) {
		if err := db.SetChatLabel(key, char, chat); err != nil {
			t.Error(err)
		}
	}
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.After = withGeneration{pipe, w}
	st := newStatus(db, mods, w)
	w.Event = st.workerEvent
	_, clearCold := backfillHandlers(pipe, w, db, srv)
	f.m = &maintenance{db: db, mods: mods, worker: w, pipe: pipe, server: srv, status: st, clearCold: clearCold}
	return f
}

func (f *maintenanceFixture) seed(t *testing.T, chat string, bodies ...string) {
	t.Helper()
	var rows []string
	for i, body := range bodies {
		rows = append(rows, memRow(2*i, "user", fmt.Sprintf("%s-u%d", chat, i), "", body), memRow(2*i+1, "char", fmt.Sprintf("%s-a%d", chat, i), "g", "답변"))
	}
	rec := memEvent(1, "load", chat, rows...)
	// Each chat has its own plugin boot in this fixture.
	rec.Event = json.RawMessage(strings.Replace(string(rec.Event), `"boot_id":"b"`, `"boot_id":"`+chat+`"`, 1))
	_, _, _, err := f.m.server.Apply(observe.Batch{PluginVersion: "synthetic", Events: []json.RawMessage{rec.Event}})
	if err != nil {
		t.Fatal(err)
	}
	err = f.m.pipe.WithSession("chat:"+chat, func(s *lifecycle.Session) {
		for i, tr := range s.ActiveTurns() {
			j, err := f.m.db.JobFor(memory.SourceOf(s.Key, tr))
			if err != nil {
				t.Fatal(err)
			}
			out, _ := memory.Parse(fmt.Sprintf(`{"entities":[{"id":"n","name":"%s"}],"memories":[{"id":"m","kind":"fact","about":"n","body":%q}]}`, chat, bodies[i]))
			if _, err := f.m.db.SaveGeneration(j, store.Generation{Output: out}); err != nil {
				t.Fatal(err)
			}
			f.jobs[chat] = append(f.jobs[chat], j.ID)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *maintenanceFixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.m.worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	})
}

func (f *maintenanceFixture) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.texts...)
}
func eventuallyMaintenance(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("maintenance state did not settle")
}

func TestMaintenanceReindexMissing(t *testing.T) {
	f := newMaintenanceFixture(t, "voyage-4-large")
	f.seed(t, "A", "A의 이미 저장된 기억", "A의 빠진 기억")
	f.seed(t, "B", "B의 빠진 기억")
	// Seed only A's first vector through the production indexer.
	if err := f.m.worker.Index(context.Background(), f.jobs["A"][:1]); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.texts = nil
	f.mu.Unlock()
	preview := callMem(t, f.m.reindex, "GET", "/maintenance/reindex?mode=missing", "")
	if preview.Code != 200 || preview.V["count"] != float64(2) || len(f.calls()) != 0 {
		t.Fatalf("preview %s", preview.Raw)
	}
	a := callMem(t, f.m.reindex, "POST", "/maintenance/reindex", `{"mode":"missing"}`)
	if a.Code != 200 || a.V["count"] != preview.V["count"] {
		t.Fatalf("queue %s", a.Raw)
	}
	if n, _ := f.m.db.VectorsWaiting("voyage-4-large"); n != 2 {
		t.Fatalf("waiting %d", n)
	}
	f.start(t)
	eventuallyMaintenance(t, func() bool {
		n, _ := f.m.db.VectorsWaiting("voyage-4-large")
		return n == 0 && f.m.worker.Diagnostics()["index_backlog_chats"] == 0
	})
	got := f.calls()
	sort.Strings(got)
	for _, s := range got {
		if strings.Contains(s, "이미 저장된") {
			t.Fatal("existing vector was called again")
		}
	}
	if len(got) != 2 || !strings.Contains(strings.Join(got, "\n"), "B의 빠진 기억") {
		t.Fatalf("wrong documents %v", got)
	}
}

func TestMaintenanceReindexAllSelectedChat(t *testing.T) {
	f := newMaintenanceFixture(t, "voyage-4-large")
	f.seed(t, "A", "A의 첫 기억", "A의 둘째 기억")
	f.seed(t, "B", "B의 기억")
	for _, jobs := range f.jobs {
		if err := f.m.worker.Index(context.Background(), jobs); err != nil {
			t.Fatal(err)
		}
	}
	// Another model's vectors must survive a forced refresh.
	other := store.TextHash("another model")
	if err := f.m.db.SaveEmbeddings("old-model", []string{other}, [][]float32{{0, 1, 0}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.texts = nil
	f.mu.Unlock()
	a := callMem(t, f.m.reindex, "POST", "/maintenance/reindex", `{"chat_id":"A","mode":"all"}`)
	if a.Code != 200 || a.V["count"] != float64(len(f.jobs["A"])) {
		t.Fatalf("all %s", a.Raw)
	}
	f.start(t)
	eventuallyMaintenance(t, func() bool {
		n, _ := f.m.db.VectorsWaiting("voyage-4-large")
		return n == 0 && f.m.worker.Diagnostics()["index_backlog_chats"] == 0
	})
	got := f.calls()
	if len(got) != len(f.jobs["A"]) {
		t.Fatalf("calls %v", got)
	}
	for _, s := range got {
		if !strings.Contains(s, "A의") {
			t.Fatalf("unselected chat called: %s", s)
		}
	}
	if _, ok, err := f.m.db.Embedding("old-model", other); err != nil || !ok {
		t.Fatal("other model lost")
	}
}

func TestMaintenanceContextualReindex(t *testing.T) {
	const model = "voyage-context-4"
	f := newMaintenanceFixture(t, model)
	f.seed(t, "A", "A의 첫 턴", "A의 빠진 턴")
	f.seed(t, "B", "B의 다른 턴")
	for _, jobs := range f.jobs {
		if err := f.m.worker.Index(context.Background(), jobs); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := embed.Documents(f.m.db, model, f.jobs["A"][1:])
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for _, h := range docs {
		hashes = append(hashes, h)
	}
	if err := f.m.db.ForgetVectors(model, hashes); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.texts = nil
	f.mu.Unlock()
	p := callMem(t, f.m.reindex, "GET", "/maintenance/reindex?mode=missing&chat_id=A", "")
	if p.Code != 200 || p.V["count"] != float64(len(docs)) {
		t.Fatal(p.Raw)
	}
	if a := callMem(t, f.m.reindex, "POST", "/maintenance/reindex", `{"chat_id":"A","mode":"missing"}`); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	f.start(t)
	eventuallyMaintenance(t, func() bool {
		n, _ := f.m.db.VectorsWaiting(model)
		return n == 0 && f.m.worker.Diagnostics()["index_backlog_chats"] == 0
	})
	got := strings.Join(f.calls(), "\n")
	if !strings.Contains(got, "A의 빠진 턴") || strings.Contains(got, "A의 첫 턴") || strings.Contains(got, "B의 다른 턴") {
		t.Fatalf("wrong contextual group %s", got)
	}
	if err := f.m.worker.Maintain(func() error { f.mu.Lock(); f.texts = nil; f.mu.Unlock(); return nil }); err != nil {
		t.Fatal(err)
	}
	if a := callMem(t, f.m.reindex, "POST", "/maintenance/reindex", `{"chat_id":"A","mode":"all"}`); a.Code != 200 || a.V["count"] != float64(len(f.jobs["A"])) {
		t.Fatal(a.Raw)
	}
	eventuallyMaintenance(t, func() bool {
		n, _ := f.m.db.VectorsWaiting(model)
		return n == 0 && f.m.worker.Diagnostics()["index_backlog_chats"] == 0
	})
	got = strings.Join(f.calls(), "\n")
	if !strings.Contains(got, "A의 첫 턴") || !strings.Contains(got, "A의 빠진 턴") || strings.Contains(got, "B의 다른 턴") {
		t.Fatalf("wrong forced contextual groups %s", got)
	}
}

func TestMaintenanceEmbeddingOffAndFailure(t *testing.T) {
	f := newMaintenanceFixture(t, "voyage-4-large")
	f.seed(t, "A", "실패를 시험할 기억")
	f.m.mods.apply(store.ModelSettings{CriticSet: true, EmbedModel: "voyage-4-large"}, false)
	before := f.m.worker.Diagnostics()
	a := callMem(t, f.m.reindex, "POST", "/maintenance/reindex", `{"mode":"all"}`)
	if a.Code != 400 || a.V["code"] != "embedding_off" || len(f.calls()) != 0 || !reflect.DeepEqual(before, f.m.worker.Diagnostics()) {
		t.Fatalf("off %s", a.Raw)
	}
	saved, err := f.m.db.ModelSettings(f.m.mods.box)
	if err != nil {
		t.Fatal(err)
	}
	f.m.mods.apply(saved, false)
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	a = callMem(t, f.m.reindex, "POST", "/maintenance/reindex", `{"mode":"missing"}`)
	if a.Code != 200 {
		t.Fatal(a.Raw)
	}
	f.start(t)
	eventuallyMaintenance(t, func() bool { return f.m.worker.Diagnostics()["index_failures"] == 1 })
	v := getStatus(t, f.m.status)["embedding"].(map[string]any)
	if v["waiting_vectors"] != float64(1) || v["failures"] != float64(1) || v["index_failing"] != true {
		t.Fatalf("failure progress %v", v)
	}
}

func TestMaintenanceResetAndFreshObservation(t *testing.T) {
	f := newMaintenanceFixture(t, "voyage-4-large")
	f.seed(t, "A", "초기화할 기억")
	f.seed(t, "B", "다른 채팅의 기억")
	for _, jobs := range f.jobs {
		if err := f.m.worker.Index(context.Background(), jobs); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.m.db.SetMemoryBudgets(1234, 2345); err != nil {
		t.Fatal(err)
	}
	saved, err := f.m.db.ModelSettings(f.m.mods.box)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := observe.LoadToken(filepath.Join(t.TempDir(), "synthetic-token.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f.m.server.Token = tok
	f.m.status.workerEvent("index_failed", "chat:A", 0, fmt.Errorf("synthetic failure"))
	f.m.status.injected("A", store.Delivery{}, "", 1, 1, 1, nil, false)
	cold, clearCold := backfillHandlers(f.m.pipe, f.m.worker, f.m.db, f.m.server)
	f.m.clearCold = clearCold
	// A pending cold run and queue must not survive reset.
	recCold := memEvent(1, "load", "cold", memRow(0, "user", "cold-u", "", "지난 입력"), memRow(1, "char", "cold-a", "g", "지난 답변"))
	if _, _, _, err := f.m.server.Apply(observe.Batch{Events: []json.RawMessage{recCold.Event}}); err != nil {
		t.Fatal(err)
	}
	if a := callMem(t, cold, "POST", "/memory/backfill", `{"chat_id":"cold"}`); a.Code != 200 || len(a.V["turns"].([]any)) == 0 {
		t.Fatal("cold run fixture was not queued")
	}
	keysBefore, _ := f.m.db.SessionKeys()
	for _, phrase := range []string{"", "전체 DB 초기화 ", " 전체 DB 초기화", "잘못된 문구"} {
		body, _ := json.Marshal(map[string]string{"phrase": phrase})
		a := callMem(t, f.m.reset, "POST", "/maintenance/reset", string(body))
		keys, _ := f.m.db.SessionKeys()
		if a.Code != 400 || len(keys) != len(keysBefore) {
			t.Fatalf("wrong phrase changed data: %s", a.Raw)
		}
	}
	a := callMem(t, f.m.reset, "POST", "/maintenance/reset", `{"phrase":"전체 DB 초기화"}`)
	if a.Code != 200 {
		t.Fatal(a.Raw)
	}
	raw := dbSQL(t, f.m.db)
	rows, err := raw.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE()`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	rows.Close()
	for _, table := range tables {
		if table == "settings" {
			continue
		}
		var n int
		if err := raw.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil || n != 0 {
			t.Fatalf("reset table %s rows=%d err=%v", table, n, err)
		}
	}
	after, err := f.m.db.ModelSettings(f.m.mods.box)
	if err != nil || !reflect.DeepEqual(saved, after) {
		t.Fatal("saved model settings changed")
	}
	if c, s, _ := f.m.db.MemoryBudgets(); c != 1234 || s != 2345 {
		t.Fatal("budgets changed")
	}
	if f.m.server.Token != tok {
		t.Fatal("access token replaced")
	}
	if d := f.m.pipe.Diagnostics(); d["sessions"] != 0 || d["snapshots_applied"] != uint64(0) {
		t.Fatalf("pipeline not cleared %v", d)
	}
	d := f.m.worker.Diagnostics()
	for _, k := range []string{"generation_waiting_chats", "index_backlog_chats", "index_failures", "generation_retry_waiting_chats", "generation_given_up_turns"} {
		if d[k] != 0 {
			t.Fatalf("worker %s not cleared", k)
		}
	}
	v := getStatus(t, f.m.status)
	if len(v["activity"].([]any)) != 0 || v["injection"] != nil || v["embedding"].(map[string]any)["index_failing"] != false {
		t.Fatal("status history remained")
	}
	if p := callMem(t, cold, "GET", "/memory/backfill?chat_id=cold", ""); p.V["total"] != float64(0) {
		t.Fatal("cold run remained")
	}
	// Same boot and seq as before: reset must have cleared raw dedup and
	// pipeline progress as well as sessions/texts. New data is observed again.
	rec := memEvent(1, "load", "A", memRow(0, "user", "fresh", "", "새 관찰"))
	rec.Event = json.RawMessage(strings.Replace(string(rec.Event), `"boot_id":"b"`, `"boot_id":"A"`, 1))
	n, _, _, err := f.m.server.Apply(observe.Batch{Events: []json.RawMessage{rec.Event}})
	if err != nil || n != 1 {
		t.Fatalf("new observation n=%d err=%v", n, err)
	}
	f.m.pipe.WithSession("chat:A", func(s *lifecycle.Session) {
		if len(s.ActiveTurns()) != 1 || s.ActiveTurns()[0].Members[0].Data != "새 관찰" {
			t.Fatal("fresh observation not applied")
		}
	})
}

func TestMaintenanceResetWaitsForInFlightIndex(t *testing.T) {
	f := newMaintenanceFixture(t, "voyage-4-large")
	f.seed(t, "A", "진행 중인 색인")
	block, entered := make(chan struct{}), make(chan struct{}, 1)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(block) }) })
	f.mu.Lock()
	f.block, f.entered = block, entered
	f.mu.Unlock()
	a := callMem(t, f.m.reindex, "POST", "/maintenance/reindex", `{"mode":"missing"}`)
	if a.Code != 200 {
		t.Fatal(a.Raw)
	}
	f.start(t)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("index did not start")
	}
	done := make(chan memAnswer, 1)
	go func() {
		done <- callMem(t, f.m.reset, "POST", "/maintenance/reset", `{"phrase":"전체 DB 초기화"}`)
	}()
	select {
	case a = <-done:
		if a.Code != 200 {
			t.Fatal(a.Raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not cancel and drain the running index")
	}
	// The server's response gate is still closed: reset had to cancel the
	// in-flight HTTP request, then drain its failure before clearing state.
	release.Do(func() { close(block) })
	keys, err := f.m.db.SessionKeys()
	if err != nil || len(keys) != 0 {
		t.Fatal("running index restored chat data")
	}
	if d := f.m.worker.Diagnostics(); d["generation_waiting_chats"] != 0 || d["index_backlog_chats"] != 0 {
		t.Fatalf("work was restored: %v", d)
	}
	if v := getStatus(t, f.m.status); len(v["activity"].([]any)) != 0 || v["embedding"].(map[string]any)["state"] != "unknown" {
		t.Fatal("late index result restored status")
	}
}

// A cold start after the data is reset: the backend has no record of the
// chat, and the run's request carries the plugin's fresh snapshot with the
// texts; the snapshot is read first and its earlier turns are asked for.
// Without it the run finds nothing (the reported bug); a snapshot whose
// texts came as fingerprints only asks the plugin to send them again.
func TestColdStartReadsTheSnapshotItCarries(t *testing.T) {
	f := newMaintenanceFixture(t, "voyage-4-large")
	cold, _ := backfillHandlers(f.m.pipe, f.m.worker, f.m.db, f.m.server)
	if a := callMem(t, cold, "POST", "/memory/backfill", `{"chat_id":"fresh"}`); a.Code != 200 || coldTurns(a) != 0 {
		t.Fatalf("no record, no snapshot: %d %s", a.Code, a.Raw)
	}
	snap := memEvent(1, "load", "fresh", memRow(0, "user", "f-u1", "", "첫 입력"), memRow(1, "char", "f-a1", "g1", "첫 답"),
		memRow(2, "user", "f-u2", "", "둘째 입력"), memRow(3, "char", "f-a2", "g2", "둘째 답"))
	batch, _ := json.Marshal(observe.Batch{Contract: observe.ContractVersion, Events: []json.RawMessage{snap.Event}})
	a := callMem(t, cold, "POST", "/memory/backfill", `{"chat_id":"fresh","observation":`+string(batch)+`}`)
	if a.Code != 200 || coldTurns(a) != 2 || a.V["resend_text"] != false {
		t.Fatalf("with the snapshot: %d %s", a.Code, a.Raw)
	}
	// texts as fingerprints only (the plugin thought they were sent): asked again
	hashOnly := observe.Record{Event: json.RawMessage(`{"boot_id":"b","seq":2,"hook":"load","stored_chat":{"chat_id":"other","rows":[{"i":0,"role":"user","chat_id":"o-u1","data":{"len":3,"h":"0000000000000001"}},{"i":1,"role":"char","chat_id":"o-a1","data":{"len":3,"h":"0000000000000002"}}]}}`)}
	batch, _ = json.Marshal(observe.Batch{Contract: observe.ContractVersion, Events: []json.RawMessage{hashOnly.Event}})
	if a := callMem(t, cold, "POST", "/memory/backfill", `{"chat_id":"other","observation":`+string(batch)+`}`); a.Code != 200 || a.V["resend_text"] != true {
		t.Fatalf("fingerprints only: %d %s", a.Code, a.Raw)
	}
}

func coldTurns(a memAnswer) int {
	l, _ := a.V["turns"].([]any)
	return len(l)
}
