package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
)

func getStatus(t *testing.T, st *status, chat ...string) map[string]any {
	t.Helper()
	path := "/status"
	if len(chat) > 0 {
		path += "?chat_id=" + chat[0]
	}
	rec := httptest.NewRecorder()
	st.handler()(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	return v
}

func kinds(v map[string]any) []string {
	var out []string
	for _, e := range v["activity"].([]any) {
		out = append(out, e.(map[string]any)["kind"].(string))
	}
	return out
}

// The status screen shows what Go decides: the chat chip's state, the
// generation counts, the last injection, the vector state with the
// memories still waiting for a vector, and the recent activity, newest
// first and at most 20.
func TestStatusView(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	w := generate.NewWorker(db, nil, "", time.Minute)
	m := &models{db: db, box: box, worker: w, flagEmbed: "voyage-4-large", flagRetries: 3}
	m.apply(store.ModelSettings{Retries: store.DefaultRetries}, false)
	st := newStatus(db, m, w)

	v := getStatus(t, st)
	emb := v["embedding"].(map[string]any)
	if v["chip"] != "no_critic" || emb["state"] != "off" || v["injection"] != nil || len(v["activity"].([]any)) != 0 || v["version"] != observe.ServiceVersion {
		t.Fatalf("first status %v", v)
	}
	if code, _, raw := callModels(t, m.handler(), "POST", `{"critic":{"provider":"ollama","model":"a","retries":3},"embedding":{"key":"pa-test"}}`); code != 200 {
		t.Fatalf("save %s", raw)
	}
	if v := getStatus(t, st); v["chip"] != "done" || v["embedding"].(map[string]any)["state"] != "unknown" || v["generation"].(map[string]any)["critic"].(map[string]any)["model"] != "a" {
		t.Fatalf("with a critic %v", v)
	}

	// a vector failure shows until a call of the same kind succeeds; a
	// success is not listed
	st.workerEvent("index_failed", "chat:A", 0, errors.New("voyage: 503"))
	v = getStatus(t, st)
	emb = v["embedding"].(map[string]any)
	if v["chip"] != "embedding_failed" || emb["index_failing"] != true || emb["index_detail"] != "voyage: 503" /* the error text as it was (11-08) */ || kinds(v)[0] != "index_failed" {
		t.Fatalf("index failure %v", v)
	}
	// a good query is not the index's recovery
	st.injected("Q", store.Delivery{}, "", 1, 1, 1, nil, true)
	if v := getStatus(t, st); v["chip"] != "embedding_failed" || v["embedding"].(map[string]any)["state"] != "failing" {
		t.Fatalf("a good query cleared the index failure %v", v)
	}
	st.workerEvent("indexed", "chat:A", 0, nil)
	if v := getStatus(t, st); v["chip"] != "done" || v["embedding"].(map[string]any)["state"] != "ok" || len(kinds(v)) != 2 {
		t.Fatalf("after a good index %v", v)
	}

	// a memory request: its result is the last injection; a failed query
	// vector is the vector state and an event of its own
	voyage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer voyage.Close()
	pipe := pipeline.New(db)
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.After = pipe
	qe, err := provider.NewEmbedder(provider.EmbedConfig{Provider: "voyageai", Model: "voyage-4-large", URL: voyage.URL, Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	h := injectHandler(srv, pipe, db, func() *provider.Embedder { return qe }, st)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/memory/inject", strings.NewReader(`{"chat_id":"Z","input":"안녕","payload":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("inject %d %s", rec.Code, rec.Body.String())
	}
	v = getStatus(t, st, "Z")
	in := v["injection"].(map[string]any)
	chars, secretMax, _ := db.MemoryBudgets()
	if in["chat"] != "Z" || in["warning"] != "query_vector_failed" || in["chars_max"].(float64) != float64(chars) || in["secret_max"].(float64) != float64(secretMax) || in["items"].(float64) != 0 {
		t.Fatalf("last injection %v", in)
	}
	if k := kinds(v); v["chip"] != "embedding_failed" || v["embedding"].(map[string]any)["query_failing"] != true || k[0] != "injected" || k[1] != "query_failed" {
		t.Fatalf("query failure %v", v)
	}

	// memories indexing gave a text without its vector are counted waiting
	raw := dbSQL(t, db)
	hash := store.TextHash("리나는 약사다")
	if _, err := raw.Exec(`INSERT INTO memories (session_key, job_id, kind, body, q, detail, doc_hash) VALUES ('chat:Z', 1, 'fact', 'b', '', '{}', ?), ('chat:Z', 1, 'fact', 'c', '', '{}', '')`, hash); err != nil {
		t.Fatal(err)
	}
	if n := getStatus(t, st)["embedding"].(map[string]any)["waiting_vectors"].(float64); n != 1 {
		t.Fatalf("waiting before a vector %v", n)
	}
	if err := db.SaveEmbeddings("voyage-4-large", []string{hash}, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if n := getStatus(t, st)["embedding"].(map[string]any)["waiting_vectors"].(float64); n != 0 {
		t.Fatalf("waiting after its vector %v", n)
	}

	// turns tried again after another critic is saved are listed
	if _, err := raw.Exec(`INSERT INTO gen_jobs (session_key, turn_id, source_key, source, state, attempts, created_at) VALUES ('chat:Z', 1, 'st', '{}', 'failed', 4, NOW())`); err != nil {
		t.Fatal(err)
	}
	w.Event = st.workerEvent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx) // the worker rearms between chats
	callModels(t, m.handler(), "POST", `{"critic":{"provider":"ollama","model":"b","retries":3}}`)
	listed := false
	for i := 0; i < 100 && !listed; i++ {
		e := getStatus(t, st)["activity"].([]any)[0].(map[string]any)
		listed = e["kind"] == "rearmed" && e["detail"] == "1"
		time.Sleep(20 * time.Millisecond)
	}
	if !listed {
		t.Fatalf("rearm not listed %v", getStatus(t, st)["activity"])
	}
	cancel()

	// at most 20, newest first
	for i := 0; i < 25; i++ {
		st.workerEvent("made", "chat:Z", i+1, nil)
	}
	list := getStatus(t, st)["activity"].([]any)
	if len(list) != activityKept || list[0].(map[string]any)["turn"].(float64) != 25 || list[19].(map[string]any)["turn"].(float64) != 6 {
		t.Fatalf("list %d %v", len(list), fmt.Sprint(list[0], list[len(list)-1]))
	}
}

// The status is the current chat's: another chat's injection or generation
// is not shown as this chat's; a chat being worked on is generating.
func TestStatusIsTheChats(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	w := generate.NewWorker(db, nil, "", time.Minute)
	m := &models{db: db, box: box, worker: w, flagRetries: 3, flagCritic: provider.Config{Provider: "ollama", Model: "a", URL: "http://127.0.0.1:9"}}
	m.apply(store.ModelSettings{Retries: store.DefaultRetries}, false)
	st := newStatus(db, m, w)
	st.injected("A", store.Delivery{IDs: []string{"M1"}, Chars: 10}, "", 100, 50, 1, nil, false)
	st.injected("B", store.Delivery{Chars: 70}, "", 900, 50, 1, nil, false)
	a := getStatus(t, st, "A")
	if in := a["injection"].(map[string]any); a["chat"] != "A" || in["chat"] != "A" || in["chars_max"].(float64) != 100 || in["items"].(float64) != 1 {
		t.Fatalf("chat A shows %v", a["injection"])
	}
	if c := getStatus(t, st, "C"); c["injection"] != nil {
		t.Fatalf("a chat without a request shows %v", c["injection"])
	}
	w.Notify("chat:B", nil) // B waits for its run; A does not
	if a, b := getStatus(t, st, "A"), getStatus(t, st, "B"); a["chip"] != "done" || b["chip"] != "generating" {
		t.Fatalf("A %v B %v", a["chip"], b["chip"])
	}
}

// A chat whose turn is being called is generating: the real worker reaches
// the provider and the status, read meanwhile, says so (F01).
func TestStatusWhileCalling(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	if _, err := pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "A", memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "첫째"), memRow(2, "user", "u2", "", "다음"))}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		w.Write([]byte(`{"message":{"content":"{\"memories\":[]}"}}`))
	}))
	defer model.Close()
	defer close(release)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	w := generate.NewWorker(db, nil, "", time.Minute)
	m := &models{db: db, box: box, worker: w, flagCritic: provider.Config{Provider: "ollama", Model: "m", URL: model.URL}}
	m.apply(store.ModelSettings{Retries: 0}, false)
	st := newStatus(db, m, w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe.WithSession("chat:A", func(s *lifecycle.Session) { w.Notify(s.Key, memory.Own(s)) })
	go w.Run(ctx)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was not called")
	}
	v := getStatus(t, st, "A")
	c := v["generation"].(map[string]any)["chat"].(map[string]any)
	if v["chip"] != "generating" || c["running"] != true || c["waiting"] != false {
		t.Fatalf("status during the call %v", v)
	}
}
