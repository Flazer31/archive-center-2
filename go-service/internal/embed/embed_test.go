package embed

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, err := sql.Open("mysql", base)
	if err != nil {
		t.Fatal(err)
	}
	name := "ac2_test_embed_" + regexp.MustCompile(`\W`).ReplaceAllString(t.Name(), "_")
	boot.Exec("DROP DATABASE IF EXISTS " + name)
	if _, err := boot.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE " + name); boot.Close() })
	db, err := store.Open(base + name + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeVoyage answers the embeddings endpoint, recording each request; it
// fails while down is set. The vector of a text is its length, so a stored
// vector can be checked against its text.
type fakeVoyage struct {
	mu       sync.Mutex
	requests []map[string]any
	down     bool
}

func (f *fakeVoyage) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.URL.Path != "/v1/embeddings" {
			t.Errorf("request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		down := f.down
		f.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"detail":"down"}`)
			return
		}
		var data []map[string]any
		for i, in := range req["input"].([]any) {
			data = append(data, map[string]any{"index": i, "embedding": []float32{float32(len([]rune(in.(string)))), 1}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"total_tokens": 7 * len(data)}})
	}))
}

func saveTurn(t *testing.T, db *store.DB, out string) int64 {
	t.Helper()
	s := lifecycle.NewSession("chat:E")
	s.Apply(lifecycle.Snapshot{SessionKey: s.Key, Source: lifecycle.SourceOutputEvent, TargetIndex: -1, Rows: []lifecycle.Row{
		{Index: 0, ChatID: "u-" + out, Role: "user", Data: "입력"}, {Index: 1, ChatID: "a-" + out, Role: "char", GenerationID: "g1", Data: out}}})
	j, err := db.JobFor(memory.SourceOf(s.Key, s.ActiveTurns()[0]))
	if err != nil {
		t.Fatal(err)
	}
	var o memory.Output
	json.Unmarshal([]byte(out), &o)
	if _, err := db.SaveGeneration(j, store.Generation{Output: o, Response: out}); err != nil {
		t.Fatal(err)
	}
	return j.ID
}

// Memories are embedded as documents with names read for ids; a text
// embedded once is reused (the same text in two memories is sent once, and
// a second run sends nothing); a failed request keeps the memory and the
// missing vectors are sent on a later run.
func TestIndexEmbedsOnceReusesAndFillsAfterFailure(t *testing.T) {
	db := testDB(t)
	f := &fakeVoyage{}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "voyage-test", Key: "test-key", HTTP: srv.Client()}
	job := saveTurn(t, db, `{"entities":[{"id":"n1","name":"한얼"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"돈","mode":"상태","body":"20냥"},{"id":"m2","kind":"event","body":"주막에 갔다"},{"id":"m3","kind":"event","body":"주막에 갔다"}]}`)

	f.down = true
	r, err := Index(context.Background(), db, v, "voyage-test", []int64{job}, 1)
	if err == nil || r.Embedded != 0 || r.Memories != 3 {
		t.Fatalf("failed run: %+v %v", r, err)
	}
	if k, _ := db.KnownOf([]int64{job}); len(k.Memories) != 3 {
		t.Fatal("a failed index touched the memory")
	}
	f.down = false
	r, err = Index(context.Background(), db, v, "voyage-test", []int64{job}, 1)
	if err != nil || r.Embedded != 2 || r.Requests != 2 || r.Reused != 0 || r.Tokens != 14 {
		t.Fatalf("filled run: %+v %v", r, err)
	}
	first := f.requests[1]
	if first["model"] != "voyage-test" || first["input_type"] != "document" || first["input"].([]any)[0] != "fact/상태 한얼 돈: 20냥" {
		t.Fatalf("request %v", first)
	}
	r, _ = Index(context.Background(), db, v, "voyage-test", []int64{job}, 1)
	if r.Embedded != 0 || r.Reused != 3 || len(f.requests) != 3 {
		t.Fatalf("second run sent again: %+v, %d requests", r, len(f.requests))
	}
	vec, ok, err := db.Embedding("voyage-test", store.TextHash("event 주막에 갔다"))
	if err != nil || !ok || vec[0] != float32(len([]rune("event 주막에 갔다"))) {
		t.Fatalf("stored vector %v %v %v", vec, ok, err)
	}
	if !strings.Contains(f.requests[2]["input"].([]any)[0].(string), "주막에 갔다") {
		t.Fatalf("second text %v", f.requests[2])
	}
}

// A memory about a name written in an earlier turn gives the same document
// indexed alone or with that turn (the name is read by id), so it is not
// embedded twice.
func TestDocumentDoesNotDependOnIndexedJobs(t *testing.T) {
	db := testDB(t)
	f := &fakeVoyage{}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "m", Key: "test-key", HTTP: srv.Client()}
	j1 := saveTurn(t, db, `{"entities":[{"id":"n1","name":"민재"}]}`)
	k1, _ := db.KnownOf([]int64{j1})
	j2 := saveTurn(t, db, `{"memories":[{"id":"m1","kind":"fact","about":"`+k1.Names[0].ID+`","slot":"돈","body":"20냥"}]}`)
	if r, err := Index(context.Background(), db, v, "m", []int64{j2}, 8); err != nil || r.Embedded != 1 {
		t.Fatalf("alone: %+v %v", r, err)
	}
	if got := f.requests[0]["input"].([]any)[0]; got != "fact 민재 돈: 20냥" {
		t.Fatalf("document %q", got)
	}
	if r, _ := Index(context.Background(), db, v, "m", []int64{j1, j2}, 8); r.Embedded != 0 || r.Reused != 1 {
		t.Fatalf("with the name's turn: %+v", r)
	}
}
