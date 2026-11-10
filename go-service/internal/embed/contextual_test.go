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
	"sync"
	"testing"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// fakeContext answers the contextualized endpoint, recording each request's
// groups. A chunk's vector is its length, the size of its group and the
// group's length, so a stored vector shows which chunk and group it came
// from.
type fakeContext struct {
	mu     sync.Mutex
	groups [][][]string // per request
}

func (f *fakeContext) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/contextualizedembeddings" {
			t.Errorf("request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Model     string     `json:"model"`
			Inputs    [][]string `json:"inputs"`
			InputType string     `json:"input_type"`
		}
		json.Unmarshal(b, &req)
		if req.Model != "voyage-context-4" || req.InputType != "document" {
			t.Errorf("request %s %s", req.Model, req.InputType)
		}
		f.mu.Lock()
		f.groups = append(f.groups, req.Inputs)
		f.mu.Unlock()
		var data []map[string]any
		for i, g := range req.Inputs {
			all := 0
			for _, c := range g {
				all += len([]rune(c))
			}
			var rows []map[string]any
			for k, c := range g {
				rows = append(rows, map[string]any{"index": k, "embedding": []float32{float32(len([]rune(c))), float32(len(g)), float32(all)}})
			}
			data = append(data, map[string]any{"index": i, "data": rows})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"total_tokens": 5}})
	}))
}

// With a contextualized model a turn's memories are embedded as one group,
// in one request, after the turn's input and response; each memory's vector
// is its own chunk's; a turn whose memories all have vectors is not sent
// again.
func TestContextualModelEmbedsATurnAsOneGroup(t *testing.T) {
	db := testDB(t)
	f := &fakeContext{}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "test-key", HTTP: srv.Client()}
	out := `{"memories":[{"id":"m1","kind":"event","body":"주막에 갔다"},{"id":"m2","kind":"fact","body":"비가 왔다"}]}`
	job := saveTurn(t, db, out)

	r, err := Index(context.Background(), db, v, "voyage-context-4", []int64{job}, 64)
	if err != nil || r.Requests != 1 || r.Embedded != 2 || r.Memories != 2 {
		t.Fatalf("index: %+v %v", r, err)
	}
	g := f.groups[0]
	if len(g) != 1 || len(g[0]) != 4 || g[0][0] != "입력" || g[0][1] != out || g[0][2] != "event 주막에 갔다" || g[0][3] != "fact 비가 왔다" {
		t.Fatalf("group %q", g)
	}
	k, _ := db.KnownOf([]int64{job})
	hashes := docHashes(t, db, k)
	vec, ok, err := db.Embedding("voyage-context-4", hashes[0])
	if err != nil || !ok || vec[0] != float32(len([]rune("event 주막에 갔다"))) || vec[1] != 4 {
		t.Fatalf("stored %v %v %v", vec, ok, err)
	}
	if hashes[0] == store.TextHash("event 주막에 갔다") {
		t.Fatal("keyed by the text alone")
	}
	if r, _ := Index(context.Background(), db, v, "voyage-context-4", []int64{job}, 64); r.Requests != 0 || r.Reused != 2 {
		t.Fatalf("second run: %+v", r)
	}
}

// The same memory text in two turns gets a vector for each turn; each turn
// is its own request (several turns together can pass a request's limit
// though each fits, ASTRA-RECHECK-71), and a turn already embedded is left
// out.
func TestContextualVectorsFollowTheirTurn(t *testing.T) {
	db := testDB(t)
	f := &fakeContext{}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "test-key", HTTP: srv.Client()}
	j1 := saveTurn(t, db, `{"memories":[{"id":"m1","kind":"event","body":"주막에 갔다"}]}`)
	j2 := saveTurn(t, db, `{"memories":[{"id":"m1","kind":"event","body":"주막에 갔다"}],"scenes":[]}`)
	j3 := saveTurn(t, db, `{"memories":[{"id":"m1","kind":"fact","body":"비가 왔다"}]}`)
	if r, err := Index(context.Background(), db, v, "voyage-context-4", []int64{j1}, 64); err != nil || r.Embedded != 1 {
		t.Fatalf("first turn: %+v %v", r, err)
	}
	r, err := Index(context.Background(), db, v, "voyage-context-4", []int64{j1, j2, j3}, 64)
	if err != nil || r.Embedded != 2 || r.Reused != 1 || r.Requests != 2 {
		t.Fatalf("three turns: %+v %v", r, err)
	}
	if len(f.groups) != 3 || len(f.groups[1]) != 1 || len(f.groups[2]) != 1 || f.groups[1][0][2] != "event 주막에 갔다" || f.groups[2][0][2] != "fact 비가 왔다" {
		t.Fatalf("requests %q", f.groups)
	}
	k, _ := db.KnownOf([]int64{j1, j2})
	if h := docHashes(t, db, k); h[0] == h[1] {
		t.Fatal("the same text in two turns shares a key")
	}
	if r, _ := Index(context.Background(), db, v, "voyage-context-4", []int64{j1, j2, j3}, 1); r.Requests != 0 {
		t.Fatalf("all embedded, sent again: %+v", r)
	}
}

// changedStore reads a turn's memories with one of them changed, as a
// later reading of the same turn could (ASTRA-RECHECK-71 2).
type changedStore struct {
	*store.DB
	body string
}

func (c changedStore) KnownOf(jobs []int64) (memory.Known, error) {
	k, err := c.DB.KnownOf(jobs)
	if err == nil && len(k.Memories) > 1 {
		k.Memories[1].Body = c.body
	}
	return k, err
}

// A memory's vector depends on its whole group: when another memory of the
// turn changes, the turn is embedded again and every memory of it gets the
// new group's vector.
func TestContextualVectorFollowsItsWholeGroup(t *testing.T) {
	db := testDB(t)
	f := &fakeContext{}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "test-key", HTTP: srv.Client()}
	job := saveTurn(t, db, `{"memories":[{"id":"m1","kind":"event","body":"주막에 갔다"},{"id":"m2","kind":"fact","body":"비가 왔다"}]}`)
	if r, err := Index(context.Background(), db, v, "voyage-context-4", []int64{job}, 64); err != nil || r.Embedded != 2 {
		t.Fatalf("first: %+v %v", r, err)
	}
	st := changedStore{db, "비가 그쳤다 그리고 바람이 불었다"}
	r, err := Index(context.Background(), st, v, "voyage-context-4", []int64{job}, 64)
	if err != nil || r.Requests != 1 || r.Embedded != 2 || r.Reused != 0 {
		t.Fatalf("after a change: %+v %v", r, err)
	}
	if got := f.groups[1][0][2]; got != "event 주막에 갔다" {
		t.Fatalf("resent group %q", f.groups[1])
	}
	// the unchanged memory now reads the vector made with the new group
	all := 0
	for _, c := range f.groups[1][0] {
		all += len([]rune(c))
	}
	k, _ := st.KnownOf([]int64{job})
	vec, ok, err := db.Embedding("voyage-context-4", docHashes(t, db, k)[0])
	if err != nil || !ok || vec[2] != float32(all) {
		t.Fatalf("M1 vector %v %v %v, want the new group's (%d)", vec, ok, err, all)
	}
}

// docHashes reads the stored vector key of each memory of k, in order,
// from the test's database.
func docHashes(t *testing.T, _ *store.DB, k memory.Known) []string {
	t.Helper()
	name := "ac2_test_embed_" + regexp.MustCompile(`\W`).ReplaceAllString(t.Name(), "_")
	raw, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+name)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var out []string
	for _, m := range k.Memories {
		var h string
		if err := raw.QueryRow(`SELECT doc_hash FROM memories WHERE CONCAT('M', id)=?`, m.ID).Scan(&h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

// A memory with no text of its own (only a quote and the record it is to)
// is not sent: the provider refuses an empty input, and with it the turn's
// other memories (L T18, 10-24).
func TestMemoryWithoutTextIsNotEmbedded(t *testing.T) {
	db := testDB(t)
	f := &fakeContext{}
	srv := f.server(t)
	defer srv.Close()
	ctxV := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "test-key", HTTP: srv.Client()}
	job := saveTurn(t, db, `{"memories":[{"id":"m1","kind":"event","body":"주막에 갔다"},{"id":"m2","q":"그 주막 말이오","to":"m1"}]}`)
	if k, _ := db.KnownOf([]int64{job}); len(k.Memories) != 2 {
		t.Fatalf("fixture: %d memories stored", len(k.Memories))
	}
	r, err := Index(context.Background(), db, ctxV, "voyage-context-4", []int64{job}, 64)
	if err != nil || r.Embedded != 1 || r.Memories != 1 {
		t.Fatalf("contextual: %+v %v", r, err)
	}
	for _, c := range f.groups[0][0] {
		if c == "" {
			t.Fatalf("an empty chunk was sent: %q", f.groups[0])
		}
	}
	plain := &fakeVoyage{}
	ps := plain.server(t)
	defer ps.Close()
	v := provider.Voyage{URL: ps.URL + "/v1/embeddings", Model: "voyage-test", Key: "test-key", HTTP: ps.Client()}
	if r, err := Index(context.Background(), db, v, "voyage-test", []int64{job}, 64); err != nil || r.Embedded != 1 {
		t.Fatalf("plain: %+v %v", r, err)
	}
	for _, in := range plain.requests[0]["input"].([]any) {
		if in == "" {
			t.Fatalf("an empty text was sent: %v", plain.requests[0])
		}
	}
}
