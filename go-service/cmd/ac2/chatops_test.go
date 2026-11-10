package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
	"archivecenter2/goservice/internal/store"
)

func u2LoadHTTP(t *testing.T, pipe *pipeline.Pipeline, chat string, seq int) *lifecycle.Session {
	t.Helper()
	ev := memEvent(seq, "load", chat, memRow(0, "user", chat+"u", "", "입력"), memRow(1, "char", chat+"a", "", "응답"))
	if _, err := pipe.Process([]observe.Record{ev}); err != nil {
		t.Fatal(err)
	}
	var s *lifecycle.Session
	if err := pipe.WithSession("chat:"+chat, func(v *lifecycle.Session) { s = v }); err != nil {
		t.Fatal(err)
	}
	return s
}

func u2GenerateHTTP(t *testing.T, db *store.DB, s *lifecycle.Session) store.Job {
	t.Helper()
	job, err := db.JobFor(memory.SourceOf(s.Key, s.ActiveTurns()[0]))
	if err != nil {
		t.Fatal(err)
	}
	output := `{"entities":[{"id":"n1","name":"시험 이름","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"문을 열었다","roles":{"actor":["n1"]}},{"id":"m2","kind":"fact","about":"n1","body":"열린 문","at":"m1"}]}`
	o, notes := memory.Parse(output)
	if _, err := db.SaveGeneration(job, store.Generation{Output: o, Response: output, Notes: notes}); err != nil {
		t.Fatal(err)
	}
	return job
}

func u2Post(t *testing.T, h http.HandlerFunc, path, body string) store.ChatOpResult {
	t.Helper()
	r := httptest.NewRecorder()
	h(r, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	if r.Code != 200 {
		t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
	}
	var out store.ChatOpResult
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestU2ChatOpsHTTPAndInvalidation(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	a := u2LoadHTTP(t, pipe, "A", 1)
	b := u2LoadHTTP(t, pipe, "B", 2)
	u2GenerateHTTP(t, db, a)
	worker := generate.NewWorker(db, nil, "", time.Second)
	worker.Notify(a.Key, memory.Own(a))
	worker.Notify(b.Key, memory.Own(b))
	st := newStatus(db, nil, worker)
	st.add(event{Chat: "A", Kind: "made"})
	st.add(event{Chat: "B", Kind: "made"})
	st.last["A"], st.last["B"] = &injection{Chat: "A"}, &injection{Chat: "B"}
	h := chatOpsHandler(pipe, db, worker, st)
	before := worker.Chat(a.Key)
	preview := u2Post(t, h, "/chats/delete", `{"chat_id":"A","dry_run":true}`)
	if preview.DeletedMemories == 0 || worker.Chat(a.Key) != before || st.last["A"] == nil {
		t.Fatal("preview invalidated runtime state")
	}
	copyPreview := u2Post(t, h, "/chats/copy", `{"from":"A","to":"B","dry_run":true}`)
	copied := u2Post(t, h, "/chats/copy", `{"from":"A","to":"B"}`)
	if !reflect.DeepEqual(copyPreview, copied) || copied.CopiedTurns != 1 {
		t.Fatalf("copy counts %+v %+v", copyPreview, copied)
	}
	u2Post(t, h, "/chats/delete", `{"chat_id":"A"}`)
	if state := worker.Chat(a.Key); state.Waiting || state.RetryWaiting || state.Running || state.GivenUp != 0 {
		t.Fatalf("worker retained deleted chat %+v", state)
	}
	if st.last["A"] != nil || st.last["B"] == nil || len(st.events) != 1 || st.events[0].Chat != "B" {
		t.Fatal("status purge ownership")
	}
	if err := pipe.WithSession(a.Key, func(s *lifecycle.Session) {
		if len(s.ActiveTurns()) != 0 {
			t.Fatal("pipeline cached deleted turns")
		}
	}); err != nil {
		t.Fatal(err)
	}
	list, err := db.ChatMemories(b.Key, memory.Own(b))
	if err != nil || len(list) != copied.CopiedMemories {
		t.Fatal("destination lost after delete")
	}
	// Re-observing a deleted RisuAI chat starts from stored empty state.
	u2LoadHTTP(t, pipe, "A", 3)
	u2Post(t, h, "/chats/move", `{"from":"B","to":"A"}`)
	if _, ok, err := db.LoadSession(b.Key); err != nil || ok {
		t.Fatal("HTTP move retained source")
	}
	for _, req := range []struct {
		method, path, body string
		code               int
	}{{"GET", "/chats/delete", "", 405}, {"POST", "/chats/copy", `{}`, 400}, {"POST", "/chats/move", `{"from":"A","to":"A"}`, 400}} {
		r := httptest.NewRecorder()
		h(r, httptest.NewRequest(req.method, req.path, strings.NewReader(req.body)))
		if r.Code != req.code {
			t.Fatalf("bad input %d %s", r.Code, r.Body.String())
		}
	}
}

// Both real index paths are used before and after copy. Only the embedding
// service is a loopback fake; a repeated call after copy fails the test.
func TestU2CopyReusesVectorsWithoutEmbeddingCalls(t *testing.T) {
	for _, model := range []string{"u2-plain", "voyage-context-4"} {
		t.Run(model, func(t *testing.T) {
			db := modelsDB(t)
			pipe := pipeline.New(db)
			a := u2LoadHTTP(t, pipe, "A", 1)
			b := u2LoadHTTP(t, pipe, "B", 2)
			j := u2GenerateHTTP(t, db, a)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req struct {
					Input     []string   `json:"input"`
					Inputs    [][]string `json:"inputs"`
					InputType string     `json:"input_type"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.InputType != "document" {
					t.Errorf("unexpected input type %q", req.InputType)
				}
				var data []map[string]any
				if provider.Contextual(model) {
					if r.URL.Path != "/v1/contextualizedembeddings" {
						t.Error("wrong group endpoint")
					}
					for i, g := range req.Inputs {
						var chunks []map[string]any
						for k, txt := range g {
							chunks = append(chunks, map[string]any{"index": k, "embedding": []float32{1, float32(len(txt)), 1}})
						}
						data = append(data, map[string]any{"index": i, "data": chunks})
					}
				} else {
					if r.URL.Path != "/v1/embeddings" {
						t.Error("wrong endpoint")
					}
					for i, txt := range req.Input {
						data = append(data, map[string]any{"index": i, "embedding": []float32{1, float32(len(txt)), 1}})
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer srv.Close()
			v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: model, HTTP: srv.Client()}
			initial, err := embed.Index(context.Background(), db, v, model, []int64{j.ID}, 64)
			if err != nil || initial.Embedded == 0 || calls.Load() == 0 {
				t.Fatalf("control did not index %+v %v", initial, err)
			}
			calls.Store(0)
			copied := u2Post(t, chatOpsHandler(pipe, db, nil, nil), "/chats/copy", `{"from":"A","to":"B"}`)
			jobs, err := db.CurrentJobs([]memory.Source{memory.SourceOf(b.Key, b.ActiveTurns()[0])})
			if err != nil {
				t.Fatal(err)
			}
			reindexed, err := embed.Index(context.Background(), db, v, model, jobs, 64)
			if err != nil || reindexed.Reused != copied.CopiedMemories || reindexed.Requests != 0 || calls.Load() != 0 {
				t.Fatalf("copy re-embedded: %+v calls=%d %v", reindexed, calls.Load(), err)
			}
		})
	}
}
