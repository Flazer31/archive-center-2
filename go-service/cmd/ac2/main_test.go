package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

type nopModel struct{}

func (nopModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	return provider.Result{Content: `{"memories":[]}`}, nil
}

// A chat loaded with earlier history is not made by itself; POST
// /memory/backfill asks for its history turns (oldest first, at most
// max_turns) through the observation path's session.
func TestBackfillEndpoint(t *testing.T) {
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, _ := sql.Open("mysql", base)
	boot.Exec("DROP DATABASE IF EXISTS ac2_test_backfill")
	if _, err := boot.Exec("CREATE DATABASE ac2_test_backfill CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE ac2_test_backfill"); boot.Close() })
	db, err := store.Open(base + "ac2_test_backfill?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pipe := pipeline.New(db)
	worker := generate.NewWorker(db, nopModel{}, "nop", time.Minute)
	row := func(i int, role, id, text string) string {
		b, _ := json.Marshal(map[string]any{"i": i, "role": role, "chat_id": id, "data": map[string]any{"text": text}})
		return string(b)
	}
	ev := `{"boot_id":"b","seq":1,"hook":"load","stored_chat":{"chat_id":"H","rows":[` + strings.Join([]string{
		row(0, "user", "u1", "하나"), row(1, "char", "a1", "첫째"), row(2, "user", "u2", "둘"), row(3, "char", "a2", "둘째"), row(4, "user", "u3", "셋"), row(5, "char", "a3", "셋째")}, ",") + `]}}`
	if _, err := pipe.Process([]observe.Record{{Event: json.RawMessage(ev)}}); err != nil {
		t.Fatal(err)
	}
	h := backfillHandler(pipe, worker, db)
	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/memory/backfill", strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d %s", rec.Code, rec.Body.String())
		}
		return out
	}
	if got := post(`{"chat_id":"H","max_turns":2,"order":"oldest"}`); len(got["turns"].([]any)) != 2 || got["turns"].([]any)[0] != float64(1) {
		t.Fatalf("oldest two: %v", got)
	}
	if got := post(`{"chat_id":"H","max_turns":1,"order":"recent"}`); got["turns"].([]any)[0] != float64(3) {
		t.Fatalf("most recent: %v", got)
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/memory/backfill", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing chat: %d", rec.Code)
	}

	// progress: the last run's turns by their latest job
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/memory/backfill?chat_id=H", nil))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusOK {
			t.Fatalf("progress %d %s", rec.Code, rec.Body.String())
		}
		return out
	}
	num := func(v map[string]any, k string) int { f, _ := v[k].(float64); return int(f) }
	if p := get(); num(p, "total") != 1 || num(p, "waiting") != 1 || num(p, "done") != 0 || num(p, "asked") != 3 {
		t.Fatalf("progress before %v", p)
	}
	// cancel drops every turn still asked for
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/memory/backfill/cancel", strings.NewReader(`{"chat_id":"H"}`)))
	var c map[string]any
	json.Unmarshal(rec.Body.Bytes(), &c)
	if rec.Code != 200 || num(c, "cancelled") != 3 {
		t.Fatalf("cancel %d %v", rec.Code, c)
	}
	if p := get(); num(p, "waiting") != 0 || num(p, "cancelled") != 1 || num(p, "asked") != 0 {
		t.Fatalf("progress after cancel %v", p)
	}
	// a made turn counts as made; a failed one with its calls used up as failed
	conn, _ := sql.Open("mysql", base+"ac2_test_backfill?parseTime=true")
	defer conn.Close()
	add := func(turn int, state string, attempts int, notes string) {
		if _, err := conn.Exec(`INSERT INTO gen_jobs (session_key, turn_id, source_key, source, state, attempts, model, notes, created_at) VALUES ('chat:H',?,?,'{}',?,?,'m',?,NOW())`, turn, fmt.Sprint("k", turn, state), state, attempts, notes); err != nil {
			t.Fatal(err)
		}
	}
	post(`{"chat_id":"H","max_turns":0,"order":"oldest"}`)
	add(1, "done", 1, "[]")
	add(2, "failed", 1+worker.Retries, `["model said no"]`)
	p := get()
	f, _ := p["failures"].([]any)
	if num(p, "total") != 3 || num(p, "done") != 1 || num(p, "failed") != 1 || num(p, "waiting") != 1 || len(f) != 1 || f[0].(map[string]any)["error"] != "model said no" {
		t.Fatalf("progress with jobs %v", p)
	}
}

// POST /memory/inject answers the chat's memory for a request through the
// observation path's session: a record of an earlier turn is delivered,
// one of a turn the request holds is not, and without a query vector the
// answer says so. GET/POST /settings/memory read and set the ceilings.
func TestInjectEndpoint(t *testing.T) {
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, _ := sql.Open("mysql", base)
	boot.Exec("DROP DATABASE IF EXISTS ac2_test_inject")
	if _, err := boot.Exec("CREATE DATABASE ac2_test_inject CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE ac2_test_inject"); boot.Close() })
	db, err := store.Open(base + "ac2_test_inject?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pipe := pipeline.New(db)
	row := func(i int, role, id, text string) string {
		b, _ := json.Marshal(map[string]any{"i": i, "role": role, "chat_id": id, "data": map[string]any{"text": text}})
		return string(b)
	}
	ev := `{"boot_id":"b","seq":1,"hook":"load","stored_chat":{"chat_id":"H","rows":[` + strings.Join([]string{
		row(0, "user", "u1", "리나를 만났다"), row(1, "char", "a1", "첫째"), row(2, "user", "u2", "둘"), row(3, "char", "a2", "둘째")}, ",") + `]}}`
	if _, err := pipe.Process([]observe.Record{{Event: json.RawMessage(ev)}}); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	pipe.WithSession("chat:H", func(s *lifecycle.Session) {
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
	save(srcs[0], `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"리나는 약사다"}]}`)
	save(srcs[1], `{"memories":[{"id":"m1","kind":"fact","about":"N1","slot":"기분","body":"리나는 기분이 좋다"}]}`)
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.After = pipe
	h := injectHandler(srv, pipe, db, func() *provider.Embedder { return nil }, nil)
	fp := func(s string) map[string]any {
		n, hh := ingest.Fingerprint(s)
		return map[string]any{"len": n, "h": hh}
	}
	body, _ := json.Marshal(map[string]any{"chat_id": "H", "input": "리나에게 묻는다", "payload": []any{fp("둘"), fp("둘째"), fp("리나에게 묻는다")}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/memory/inject", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Block      string   `json:"block"`
		IDs        []string `json:"ids"`
		Window     int      `json:"window"`
		WindowFrom string   `json:"window_from"`
		Ranked     string   `json:"ranked"`
		Warning    string   `json:"warning"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Window != 1 || got.WindowFrom != "payload" || len(got.IDs) != 1 {
		t.Fatalf("answer %s", rec.Body.String())
	}
	if !strings.Contains(got.Block, "리나는 약사다") || strings.Contains(got.Block, "기분이 좋다") {
		t.Fatalf("block %q", got.Block)
	}
	if got.Ranked != "recency" || got.Warning != "query_vector_off" {
		t.Fatalf("no query provider: ranked %s warning %s", got.Ranked, got.Warning)
	}
	sh := settingsHandler(db)
	rec = httptest.NewRecorder()
	sh(rec, httptest.NewRequest(http.MethodPost, "/settings/memory", strings.NewReader(`{"memory_chars":5}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"memory_chars":5`) || !strings.Contains(rec.Body.String(), `"secret_chars":4000`) {
		t.Fatalf("settings %d %s", rec.Code, rec.Body.String())
	}
	// a negative ceiling is refused with its code and nothing changes
	rec = httptest.NewRecorder()
	sh(rec, httptest.NewRequest(http.MethodPost, "/settings/memory", strings.NewReader(`{"memory_chars":-1,"secret_chars":100}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"invalid_budget"`) {
		t.Fatalf("negative ceiling: %d %s", rec.Code, rec.Body.String())
	}
	if c, s, _ := db.MemoryBudgets(); c != 5 || s != 4000 {
		t.Fatalf("refused setting changed the ceilings: %d %d", c, s)
	}
	rec = httptest.NewRecorder()
	sh(rec, httptest.NewRequest(http.MethodPost, "/settings/memory", strings.NewReader(`{"memory_chars":`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("broken body: %d %s", rec.Code, rec.Body.String())
	}
	// the new ceiling is used by the next request: nothing fits in 5 chars
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/memory/inject", strings.NewReader(string(body))))
	var small struct {
		Block string   `json:"block"`
		IDs   []string `json:"ids"`
	}
	json.Unmarshal(rec.Body.Bytes(), &small)
	if len(small.IDs) != 0 || small.Block != "" {
		t.Fatalf("ceiling 5: %s", rec.Body.String())
	}

	// a request whose observation carries a turn the backend has not seen:
	// the observation is applied first, so that turn is the request's
	// window and both earlier turns' records are delivered
	db.SetMemoryBudgets(2000, 4000)
	ev2 := `{"boot_id":"b","seq":2,"hook":"beforeRequest","stored_chat":{"chat_id":"H","rows":[` + strings.Join([]string{
		row(0, "user", "u1", "리나를 만났다"), row(1, "char", "a1", "첫째"), row(2, "user", "u2", "둘"), row(3, "char", "a2", "둘째"), row(4, "user", "u3", "셋"), row(5, "char", "a3", "셋째")}, ",") + `]}}`
	obsBody, _ := json.Marshal(map[string]any{"chat_id": "H", "input": "리나에게 묻는다", "payload": []any{fp("셋"), fp("셋째"), fp("리나에게 묻는다")},
		"observation": map[string]any{"contract": observe.ContractVersion, "plugin_version": "t", "dropped_before": 0, "events": []json.RawMessage{json.RawMessage(ev2)}}})
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/memory/inject", strings.NewReader(string(obsBody))))
	var fresh struct {
		IDs         []string `json:"ids"`
		Window      int      `json:"window"`
		WindowFrom  string   `json:"window_from"`
		Observation string   `json:"observation"`
	}
	json.Unmarshal(rec.Body.Bytes(), &fresh)
	if rec.Code != http.StatusOK || fresh.Observation != "" || fresh.Window != 1 || fresh.WindowFrom != "payload" || len(fresh.IDs) != 2 {
		t.Fatalf("observation carried with the request: %d %s", rec.Code, rec.Body.String())
	}
}
