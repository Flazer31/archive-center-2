package embed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"archivecenter2/goservice/internal/provider"
)

// limitedContext answers the contextualized endpoint like Voyage with a
// document limit: a group longer than limit characters is refused (400);
// status, when set, answers every request with it instead.
type limitedContext struct {
	mu     sync.Mutex
	limit  int
	status int
	sizes  []int // the size of each group sent, in order
}

func (f *limitedContext) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Inputs [][]string `json:"inputs"`
		}
		json.Unmarshal(b, &req)
		size := 0
		for _, c := range req.Inputs[0] {
			size += len([]rune(c))
		}
		f.mu.Lock()
		f.sizes = append(f.sizes, size)
		f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			w.Write([]byte(`{"detail":"refused"}`))
			return
		}
		if size > f.limit {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"detail":"The total number of tokens in the list must not exceed 32,000."}`))
			return
		}
		var rows []map[string]any
		for k, c := range req.Inputs[0] {
			rows = append(rows, map[string]any{"index": k, "embedding": []float32{float32(len([]rune(c))), float32(len(req.Inputs[0]))}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "data": rows}}, "usage": map[string]any{"total_tokens": size}})
	}))
}

// A turn over the document limit is sent again with its context cut until it
// fits, and every memory gets its own vector; the turns after it are indexed
// too (1.3 user report: one large turn stopped a chat's vectors for good and
// read as a connection problem).
func TestLongTurnIsSentWithItsContextCut(t *testing.T) {
	db := testDB(t)
	f := &limitedContext{limit: 400}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "test-key", HTTP: srv.Client()}
	long := saveTurn(t, db, `{"pad":"`+strings.Repeat("가", 1500)+`","memories":[{"id":"m1","kind":"event","body":"주막에 갔다"},{"id":"m2","kind":"fact","body":"비가 왔다"}]}`)
	next := saveTurn(t, db, `{"memories":[{"id":"m1","kind":"event","body":"장터에 갔다"}]}`)
	r, err := Index(context.Background(), db, v, "voyage-context-4", []int64{long, next}, 64)
	if err != nil || r.Embedded != 3 || r.Shortened == 0 {
		t.Fatalf("index: %+v %v (sizes %v)", r, err, f.sizes)
	}
	k, _ := db.KnownOf([]int64{long, next})
	for i, h := range docHashes(t, db, k) {
		vec, ok, err := db.Embedding("voyage-context-4", h)
		want := float32(len([]rune(map[int]string{0: "event 주막에 갔다", 1: "fact 비가 왔다", 2: "event 장터에 갔다"}[i])))
		if err != nil || !ok || vec[0] != want {
			t.Fatalf("memory %d: %v %v %v", i, vec, ok, err)
		}
	}
	// the turn that fit was sent once, whole
	if last := f.sizes[len(f.sizes)-1]; last > 400 {
		t.Fatalf("sizes %v", f.sizes)
	}
}

// Memories that alone pass the limit are sent in parts, each part without
// the turn's text.
func TestManyMemoriesAreSentInParts(t *testing.T) {
	db := testDB(t)
	f := &limitedContext{limit: 60}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "test-key", HTTP: srv.Client()}
	saved := documentLimit
	defer func() { documentLimit = saved }()
	documentLimit = 60
	var mems []string
	for i := 0; i < 6; i++ {
		mems = append(mems, `{"id":"m`+string(rune('1'+i))+`","kind":"event","body":"`+strings.Repeat("나", 15)+string(rune('가'+i))+`"}`)
	}
	job := saveTurn(t, db, `{"memories":[`+strings.Join(mems, ",")+`]}`)
	r, err := Index(context.Background(), db, v, "voyage-context-4", []int64{job}, 64)
	if err != nil || r.Embedded != 6 {
		t.Fatalf("index: %+v %v (sizes %v)", r, err, f.sizes)
	}
}

// A refusal that cutting cannot help (a bad key: every request refused)
// stops after the turn's context is gone, and reads as a refusal; a server
// error is returned at once, not sent again cut.
func TestRefusalAndServerErrorStop(t *testing.T) {
	db := testDB(t)
	f := &limitedContext{status: http.StatusUnauthorized}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "test-key", HTTP: srv.Client()}
	a := saveTurn(t, db, `{"pad":"`+strings.Repeat("가", 100)+`","memories":[{"id":"m1","kind":"event","body":"주막에 갔다"}]}`)
	b := saveTurn(t, db, `{"memories":[{"id":"m1","kind":"event","body":"장터에 갔다"}]}`)
	r, err := Index(context.Background(), db, v, "voyage-context-4", []int64{a, b}, 64)
	if err == nil || !provider.Rejected(err) || r.Embedded != 0 || r.Requests != 5 {
		t.Fatalf("bad key: %+v %v", r, err)
	}
	f.mu.Lock()
	f.status, f.sizes = http.StatusInternalServerError, nil
	f.mu.Unlock()
	r, err = Index(context.Background(), db, v, "voyage-context-4", []int64{a, b}, 64)
	if err == nil || provider.Rejected(err) || r.Requests != 1 || r.Shortened != 0 {
		t.Fatalf("server error: %+v %v", r, err)
	}
	f.mu.Lock()
	f.status = http.StatusTooManyRequests
	f.mu.Unlock()
	if _, err := Index(context.Background(), db, v, "voyage-context-4", []int64{a, b}, 64); err == nil || provider.Rejected(err) {
		t.Fatalf("rate limit read as a refusal: %v", err)
	}
}
