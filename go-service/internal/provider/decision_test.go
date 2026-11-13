package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeSystemOne answers like Ollama's /v1/systemone: noul = 0.9 for ids
// ending in "y", 0.1 otherwise; a request with more than limit questions is
// refused as too long, as the real service does past 65,536 tokens.
type fakeSystemOne struct {
	mu       sync.Mutex
	limit    int
	requests []map[string]any
	auth     []string
	choice   bool // answer as TypeSafe choice
}

func (f *fakeSystemOne) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if r.URL.Path != "/v1/systemone" {
		http.Error(w, "not found", 404)
		return
	}
	qs, _ := body["questions"].(map[string]any)
	if f.limit > 0 && len(qs) > f.limit {
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"error":"Request requires %d tokens; the maximum is 65536"}`, 60000+len(qs)*100)
		return
	}
	answers := map[string]any{}
	for id := range qs {
		p := 0.1
		if strings.HasSuffix(id, "y") {
			p = 0.9
		}
		if f.choice {
			answers[id] = map[string]any{"type": "choice", "choice": "yes", "probabilities": map[string]float64{"yes": p, "no": 1 - p}, "confidence": 0.5}
		} else {
			answers[id] = map[string]any{"type": "noul", "noul": p}
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": body["model"], "answers": answers, "usage": map[string]any{"input_tokens": 10 * len(qs), "output_tokens": 0}})
}

func questions(n int) []Question {
	var qs []Question
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("q%03d", i)
		if i%3 == 0 {
			id += "y"
		}
		qs = append(qs, Question{ID: id, Text: "필요한가? " + id, Yes: "필요", No: "불필요"})
	}
	return qs
}

func TestDecisionAskBatchesAndSplits(t *testing.T) {
	f := &fakeSystemOne{limit: 40}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d, err := NewDecider(DecisionConfig{Provider: "ollama", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model() != "clef:cloud" {
		t.Fatalf("default model %q", d.Model())
	}
	qs := questions(130)
	a, err := d.Ask(context.Background(), "[직전 두 턴]\n대화", qs)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Yes) != 130 {
		t.Fatalf("answered %d of 130", len(a.Yes))
	}
	for _, q := range qs {
		want := 0.1
		if strings.HasSuffix(q.ID, "y") {
			want = 0.9
		}
		if a.Yes[q.ID] != want {
			t.Fatalf("%s: %v", q.ID, a.Yes[q.ID])
		}
	}
	// 130 = 64 + 64 + 2: the two 64s are refused (over 40) and asked in 32s
	if a.Rejected != 2 || a.Calls != 5 || a.InputTokens != 1300 {
		t.Fatalf("calls %d rejected %d tokens %d", a.Calls, a.Rejected, a.InputTokens)
	}
	r := f.requests[0]
	q := r["questions"].(map[string]any)["q000y"].(map[string]any)
	if r["model"] != "clef:cloud" || r["state"] != "[직전 두 턴]\n대화" || q["type"] != "noul" || q["criteria"].(map[string]any)["true"] != "필요" {
		t.Fatalf("request shape %v", r)
	}
	if f.auth[0] != "" {
		t.Fatalf("a local Ollama without a key sends no Authorization: %q", f.auth[0])
	}
}

func TestDecisionTypeSafeAndKeys(t *testing.T) {
	f := &fakeSystemOne{choice: true}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if _, err := NewDecider(DecisionConfig{Provider: "typesafe", URL: srv.URL}); !errors.Is(err, ErrDecisionNoKey) {
		t.Fatalf("typesafe without a key: %v", err)
	}
	if _, err := NewDecider(DecisionConfig{Provider: "ollama_cloud"}); !errors.Is(err, ErrDecisionNoKey) {
		t.Fatalf("ollama.com without a key: %v", err)
	}
	if _, err := NewDecider(DecisionConfig{Provider: "nope", Key: "k"}); !errors.Is(err, ErrDecisionUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	d, err := NewDecider(DecisionConfig{Provider: "typesafe", URL: srv.URL + "/v1/systemone", Key: "sk-secret-123"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := d.Ask(context.Background(), "s", questions(3))
	if err != nil || a.Yes["q000y"] != 0.9 || a.Yes["q001"] != 0.1 {
		t.Fatalf("choice answers %v %v", a.Yes, err)
	}
	if q := f.requests[0]["questions"].(map[string]any)["q000y"].(map[string]any); q["type"] != "choice" || q["criteria"].(map[string]any)["yes"] != "필요" {
		t.Fatalf("typesafe asks a yes/no choice: %v", q)
	}
	if f.auth[0] != "Bearer sk-secret-123" || f.requests[0]["model"] != "jev-1.13.0" {
		t.Fatalf("auth %q model %v", f.auth[0], f.requests[0]["model"])
	}
	// a failure other than length is reported, with the key masked
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"bad key sk-secret-123"}`)
	}))
	defer bad.Close()
	d2, _ := NewDecider(DecisionConfig{Provider: "typesafe", URL: bad.URL, Key: "sk-secret-123"})
	a, err = d2.Ask(context.Background(), "s", questions(2))
	if err == nil || strings.Contains(err.Error(), "sk-secret-123") || len(a.Yes) != 0 || a.Calls != 0 {
		t.Fatalf("unauthorized: %v %+v", err, a)
	}
}

// A body read error is masked, a redirect is not followed so the key
// goes nowhere else.
func TestDecisionReview16(t *testing.T) {
	const key = "sk-review16-secret"
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, buf, _ := hj.Hijack()
		defer conn.Close()
		// a chunked body whose trailer line is not a header: the read fails
		// with that line (the key) in its text
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n" + r.Header.Get("Authorization") + "\r\n\r\n")
		buf.Flush()
	}))
	defer broken.Close()
	d, _ := NewDecider(DecisionConfig{Provider: "typesafe", URL: broken.URL, Key: key})
	_, err := d.Ask(context.Background(), "s", questions(1))
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("read error: %v", err)
	}

	var gotAuth atomic.Value
	gotAuth.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"answers":{"q000y":{"type":"noul","noul":0.9}}}`)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/systemone", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	d, _ = NewDecider(DecisionConfig{Provider: "typesafe", URL: redirect.URL, Key: key})
	a, err := d.Ask(context.Background(), "s", questions(1))
	if err == nil || len(a.Yes) != 0 || gotAuth.Load() != "" {
		t.Fatalf("redirect followed: %v %+v auth %q", err, a, gotAuth.Load())
	}
}
