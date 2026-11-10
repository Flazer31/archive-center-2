package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Exercise the real observation scope after replacement, deletion and append.
// These observations do not change the lifecycle's existing acceptance rules.
func TestRecentQueryReadsRequestScope(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "첫 입력"), memRow(1, "char", "a1", "g1", "첫 응답"), memRow(2, "user", "u2", "", "현재 입력")}
	check := func(seq int, rs []string, expected string, completed int) {
		t.Helper()
		if _, err := p.Process([]observe.Record{memEvent(seq, "beforeRequest", "A", rs...)}); err != nil {
			t.Fatal(err)
		}
		if err := p.WithSession("chat:A", func(s *lifecycle.Session) {
			scope := memory.ScopeOf(s)
			if got := previousTurnText(scope); got != expected {
				t.Errorf("previous turn %q, want %q", got, expected)
			}
			n := 0
			for _, turn := range scope {
				if len(turn.Source.Responses) > 0 {
					n++
				}
			}
			if n != completed {
				t.Errorf("completed scope size %d, want %d", n, completed)
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(1, rows, "USER: 첫 입력\nREPLY: 첫 응답\n", 1)
	check(2, rows, "USER: 첫 입력\nREPLY: 첫 응답\n", 1) // provider retry
	rows[1] = memRow(1, "char", "a1", "g2", "리롤 응답")
	check(3, rows, "USER: 첫 입력\nREPLY: 리롤 응답\n", 1)
	check(4, rows[:1], "", 0) // assistant deletion
	rows[0] = memRow(0, "user", "u1", "", "편집한 입력")
	rows[1] = memRow(1, "char", "a1-new", "g3", "다시 생성한 응답")
	check(5, rows, "USER: 편집한 입력\nREPLY: 다시 생성한 응답\n", 1)
	rows = append(rows[:2], memRow(2, "user", "u3", "", "편집한 입력"), memRow(3, "char", "a3", "g4", "새 행 응답"), memRow(4, "user", "u4", "", "다음 입력"))
	check(6, rows, "USER: 편집한 입력\nREPLY: 새 행 응답\n", 2)
	rows = append(rows[:4], memRow(4, "user", "u4", "", "다음 입력"), memRow(5, "char", "a4", "g5", "정상 응답"), memRow(6, "user", "u5", "", "마지막 입력"))
	check(7, rows, "USER: 다음 입력\nREPLY: 정상 응답\n", 3)
}

func TestRecentQueryInjectionBatchAndFallback(t *testing.T) {
	for _, mode := range []string{"batch", "recent_empty", "batch_failed", "all_failed", "first_turn"} {
		t.Run(mode, func(t *testing.T) {
			h := hudFixture(t, "", 0)
			if mode != "first_turn" {
				scope, _ := injectionSeed(t, h, "A")
				ids := injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"리나는 약사다"}]}`)
				id := ids["memories/0"]
				hash := store.TextHash("synthetic query fixture")
				if err := h.db.SetDocHashes(map[string]string{id: hash}); err != nil {
					t.Fatal(err)
				}
				if err := h.db.SaveEmbeddings("voyage-4-large", []string{hash}, [][]float32{{1, 0, 0}}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "A", memRow(0, "user", "cur", "", "리나의 약속과 생각"))}); err != nil {
				t.Fatal(err)
			}
			var asked [][]string
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var b struct {
					Input     []string `json:"input"`
					InputType string   `json:"input_type"`
				}
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/embeddings" || b.InputType != "query" {
					t.Errorf("unexpected provider request %s %+v", r.URL.Path, b)
				}
				asked = append(asked, b.Input)
				if mode == "all_failed" || (mode == "batch_failed" && len(b.Input) == 2) {
					http.Error(w, "synthetic unavailable", 503)
					return
				}
				if len(b.Input) == 1 {
					w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
					return
				}
				if mode == "recent_empty" {
					w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]},{"index":1,"embedding":[]}]}`))
					return
				}
				w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1,0]},{"index":0,"embedding":[1,0,0]}]}`))
			}))
			defer fake.Close()
			q, err := provider.NewEmbedder(provider.EmbedConfig{Provider: "voyageai", Model: "voyage-4-large", URL: fake.URL, Key: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			srv := observe.NewServer(store.RawSink{DB: h.db})
			srv.After = h.pipe
			a := callMem(t, injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return q }, h.st), "POST", "/memory/inject", `{"chat_id":"A","input":"프리셋 지시문","payload":[{"role":"user"}]}`)
			if a.Code != 200 {
				t.Fatalf("inject %d %s", a.Code, a.Raw)
			}
			wantCalls := [][]string{{"리나의 약속과 생각", "USER: 입력 2\nREPLY: 응답 2\n"}}
			if mode == "batch_failed" || mode == "all_failed" {
				wantCalls = append(wantCalls, []string{"리나의 약속과 생각"})
			}
			if mode == "first_turn" {
				wantCalls = [][]string{{"리나의 약속과 생각"}}
			}
			if !reflect.DeepEqual(asked, wantCalls) {
				t.Fatalf("separate query texts/call count %q want %q", asked, wantCalls)
			}
			used := mode == "batch"
			if a.V["recent_query_used"] != used {
				t.Fatalf("used %s", a.Raw)
			}
			warning := ""
			if mode == "recent_empty" || mode == "batch_failed" {
				warning = "recent_query_vector_failed"
			}
			if mode == "all_failed" {
				warning = "query_vector_failed"
			}
			if got, _ := a.V["warning"].(string); got != warning {
				t.Fatalf("warning %q want %q", got, warning)
			}
			t.Logf("synthetic provider mode=%s calls=%d embed_ms=%v total_ms=%v recent_used=%v warning=%s", mode, len(asked), a.V["embed_ms"], a.V["total_ms"], used, warning)
			if mode != "first_turn" {
				scope := []memory.ScopedTurn{}
				h.pipe.WithSession("chat:A", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
				baselineReq := store.DeliveryRequest{Input: "리나의 약속과 생각", Payload: []store.TextPrint{{Role: "user"}}, Model: "voyage-4-large", Chars: 2000, Secret: 4000}
				if mode != "all_failed" {
					baselineReq.Query = []float32{1, 0, 0}
				}
				baseline, err := h.db.Deliver(scope, baselineReq)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "batch" && (a.V["block"] != baseline.Block || !reflect.DeepEqual(a.V["ids"], jsonIDs(baseline.IDs))) {
					t.Fatalf("fallback differs from input-only Deliver: %s", a.Raw)
				}
			}
			for _, st := range []*status{h.st, newStatus(h.db, nil, nil)} {
				view := callMem(t, st.injectionHandler(), "GET", "/injection?chat_id=A", "")
				if view.Code != 200 || view.V["recent_query_used"] != used {
					t.Fatalf("live/restarted diagnostic %d %s", view.Code, view.Raw)
				}
			}
		})
	}
}

func jsonIDs(ids []string) any { b, _ := json.Marshal(ids); var v any; json.Unmarshal(b, &v); return v }

// A singleton request remains the old input-only provider path.
func TestRecentQueryNoHistoryUsesOriginalQuery(t *testing.T) {
	var requests int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["input"] != "현재 입력" {
			t.Errorf("input-only wire body %+v", b)
		}
		w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	defer fake.Close()
	q, _ := provider.NewEmbedder(provider.EmbedConfig{Provider: "custom", Model: "m", URL: fake.URL})
	v, recent, err, recentErr := queryVectors(context.Background(), q, "현재 입력", "")
	if requests != 1 || err != nil || recentErr != nil || len(recent) != 0 || !reflect.DeepEqual(v, []float32{1, 0}) {
		t.Fatalf("original path %v %v %v %v calls=%d", v, recent, err, recentErr, requests)
	}
}

func TestRecentQuerySequentialInputFailureIsNotRetried(t *testing.T) {
	var requests int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "synthetic input failure", 503)
	}))
	defer fake.Close()
	q, _ := provider.NewEmbedder(provider.EmbedConfig{Provider: "gemini", Model: "m", URL: fake.URL, Key: "synthetic"})
	_, recent, inputErr, _ := queryVectors(context.Background(), q, "현재 입력", "USER: 직전\nREPLY: 응답\n")
	if requests != 1 || inputErr == nil || len(recent) != 0 {
		t.Fatalf("input failure must keep the existing one-call behavior: calls=%d error=%v", requests, inputErr)
	}
}

// Check the production query owner at its HTTP boundary. Every provider uses
// its existing request builder: Voyage batches, all others send single texts.
func TestRecentQueryProviderRequests(t *testing.T) {
	const input, recent = "현재 입력", "USER: 직전 질문\nREPLY: 직전 답변\n"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	serviceAccount, _ := json.Marshal(map[string]string{"client_email": "synthetic@example.invalid", "project_id": "synthetic-project", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	for _, tc := range []struct {
		name, provider, model, path string
		contextual, native, voyage  bool
	}{
		{name: "openai", provider: "openai", model: "m", path: "/v1/embeddings"},
		{name: "neuralwatt", provider: "neuralwatt", model: "m", path: "/v1/embeddings"},
		{name: "custom", provider: "custom", model: "m", path: "/v1/embeddings"},
		{name: "ollama-openai", provider: "ollama", model: "m", path: "/v1/embeddings"},
		{name: "ollama-native", provider: "ollama", model: "m", path: "/api/embed", native: true},
		{name: "gemini", provider: "gemini", model: "m", path: "/v1/models/m:embedContent"},
		{name: "vertex", provider: "vertex", model: "m", path: "/v1/m:embedContent"},
		{name: "voyage", provider: "voyageai", model: "voyage-4-large", path: "/v1/embeddings", voyage: true},
		{name: "voyage-context", provider: "voyageai", model: "voyage-context-4", path: "/v1/contextualizedembeddings", voyage: true, contextual: true},
	} {
		modes := []string{"success", "recent_failed", "recent_empty", "input_failed", "first_turn"}
		if tc.voyage {
			modes = append(modes, "recent_missing", "input_missing")
		}
		for _, mode := range modes {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				var asked [][]string
				tokenCalls := 0
				fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/token" && tc.provider == "vertex" {
						tokenCalls++
						json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-access", "expires_in": 3600})
						return
					}
					if r.Method != http.MethodPost || r.URL.Path != tc.path {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "invalid JSON", 400)
						return
					}
					var texts []string
					switch {
					case tc.contextual:
						groups, _ := body["inputs"].([]any)
						for _, group := range groups {
							chunks, _ := group.([]any)
							if len(chunks) != 1 {
								t.Errorf("queries must use separate singleton groups: %+v", body)
								continue
							}
							text, _ := chunks[0].(string)
							texts = append(texts, text)
						}
					case tc.voyage:
						items, _ := body["input"].([]any)
						for _, item := range items {
							text, _ := item.(string)
							texts = append(texts, text)
						}
					case tc.provider == "gemini" || tc.provider == "vertex":
						content, _ := body["content"].(map[string]any)
						parts, _ := content["parts"].([]any)
						if len(parts) == 1 {
							part, _ := parts[0].(map[string]any)
							text, _ := part["text"].(string)
							texts = []string{text}
						}
					default:
						text, ok := body["input"].(string)
						if ok {
							texts = []string{text}
						}
					}
					asked = append(asked, texts)
					if len(texts) == 0 || (!tc.voyage && len(texts) != 1) {
						t.Errorf("unexpected query body: %+v", body)
						http.Error(w, "unexpected texts", 400)
						return
					}
					if tc.voyage && (body["input_type"] != "query" || body["model"] != tc.model) {
						t.Errorf("query model/type: %+v", body)
					}
					if mode == "input_failed" || (mode == "recent_failed" && texts[len(texts)-1] == recent) {
						http.Error(w, "synthetic unavailable", 503)
						return
					}
					var data []any
					for i, text := range texts {
						if len(texts) == 2 && ((mode == "input_missing" && text == input) || (mode == "recent_missing" && text == recent)) {
							continue
						}
						vector := []float32{1, 0}
						if text == recent {
							vector = []float32{0, 1}
							if mode == "recent_empty" {
								vector = nil
							}
						} else if text != input {
							t.Errorf("unexpected query text %q", text)
						}
						switch {
						case tc.contextual:
							data = append(data, map[string]any{"index": i, "data": []any{map[string]any{"index": 0, "embedding": vector}}})
						case tc.native:
							json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vector}})
							return
						case tc.provider == "gemini" || tc.provider == "vertex":
							json.NewEncoder(w).Encode(map[string]any{"embedding": map[string]any{"values": vector}})
							return
						default:
							data = append(data, map[string]any{"index": i, "embedding": vector})
						}
					}
					// The existing Voyage parser must restore indexed response order.
					if len(data) == 2 {
						data[0], data[1] = data[1], data[0]
					}
					json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				defer fake.Close()
				providerKey := "synthetic"
				if tc.provider == "vertex" {
					providerKey = string(serviceAccount)
					transport := http.DefaultTransport
					defer func() { http.DefaultTransport = transport }()
					http.DefaultTransport = installDownloadTransport(func(r *http.Request) (*http.Response, error) {
						local := r.Clone(r.Context())
						if local.URL.Host == "oauth2.googleapis.com" && local.URL.Path == "/token" {
							local.URL.Scheme, local.URL.Host = "http", strings.TrimPrefix(fake.URL, "http://")
						}
						if local.URL.Host != strings.TrimPrefix(fake.URL, "http://") {
							return nil, errors.New("unexpected nonlocal fixture request")
						}
						return transport.RoundTrip(local)
					})
				}
				base := fake.URL + "/v1"
				if tc.name == "ollama-openai" {
					base += "/embeddings"
				}
				q, err := provider.NewEmbedder(provider.EmbedConfig{Provider: tc.provider, Model: tc.model, URL: base, Key: providerKey})
				if err != nil {
					t.Fatal(err)
				}
				history := recent
				if mode == "first_turn" {
					history = ""
				}
				v, r, inputErr, recentErr := queryVectors(context.Background(), q, input, history)
				wantCalls := [][]string{{input}, {recent}}
				if tc.voyage {
					wantCalls = [][]string{{input, recent}}
					if mode == "recent_failed" || mode == "input_failed" || mode == "recent_missing" || mode == "input_missing" {
						wantCalls = append(wantCalls, []string{input})
					}
				}
				if mode == "first_turn" || (mode == "input_failed" && !tc.voyage) {
					wantCalls = [][]string{{input}}
				}
				if !reflect.DeepEqual(asked, wantCalls) {
					t.Fatalf("query texts/calls %q want %q", asked, wantCalls)
				}
				if (tc.provider == "vertex" && tokenCalls != 1) || (tc.provider != "vertex" && tokenCalls != 0) {
					t.Fatalf("unexpected auth HTTP calls %d", tokenCalls)
				}
				if mode == "input_failed" {
					if inputErr == nil || len(v) != 0 || len(r) != 0 {
						t.Fatalf("input failure: %v %v %v", v, r, inputErr)
					}
					return
				}
				wantRecent := []float32{0, 1}
				failedRecent := mode == "recent_failed" || mode == "recent_empty" || mode == "recent_missing" || mode == "input_missing"
				if failedRecent || mode == "first_turn" {
					wantRecent = nil
				}
				if inputErr != nil || (recentErr != nil) != failedRecent || !reflect.DeepEqual(v, []float32{1, 0}) || !reflect.DeepEqual(r, wantRecent) {
					t.Fatalf("vectors/errors: %v %v %v %v", v, r, inputErr, recentErr)
				}
			})
		}
	}
}
