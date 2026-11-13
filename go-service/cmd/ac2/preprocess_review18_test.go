package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Regressions. Each scenario is one once reproduced; the assertions are the
// corrected behavior.

func r18Request() store.DeliveryRequest {
	return store.DeliveryRequest{Input: "리나의 약속과 생각", Payload: []store.TextPrint{{Role: "user"}}, Chars: 100000, Secret: 100000}
}

func r18Has(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func r18Size(d store.Delivery, id string) int {
	for _, r := range d.Trace {
		if r.ID == id {
			return r.Size
		}
	}
	return 0
}

// B-02: a record holding line breaks is one model line, matched to its id.
func TestPreprocessMultilineRecord(t *testing.T) {
	h := hudFixture(t, "", 0)
	s, _ := injectionSeed(t, h, "ML")
	ids := injectionSave(t, h.db, s[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"첫 기억의 앞부분\n첫 기억의 뒷부분"},{"id":"m2","kind":"fact","about":"n1","body":"둘째 기억 전체"}]}`)
	r := r18Request()
	b, err := h.db.Deliver(s, r)
	if err != nil || len(b.IDs) != 2 {
		t.Fatalf("fixture: %v %v", b.IDs, err)
	}
	first, second := ids["memories/0"], ids["memories/1"]
	lines := deliveryLines(b)
	if !strings.Contains(lines[first], "뒷부분") || !strings.Contains(lines[second], "둘째 기억 전체") {
		t.Fatalf("lines by id: %q", lines)
	}
	// the AI calls the second record unrelated, by its own number
	f := newPrepFakes(t)
	f.aiUnrelated["둘째 기억 전체"] = true
	ai, err := provider.New(provider.Config{Provider: "custom", Model: "fake", URL: f.ai.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := preprocess(context.Background(), &prepDeps{mode: store.PreprocessAI, ai: ai}, h.db, s, r, b, r.Input)
	if !reflect.DeepEqual(d.IDs, []string{first}) {
		t.Fatalf("removed the wrong record: %v (want %s kept)", d.IDs, first)
	}
}

// B-03: a pinned record is neither removed nor counted; the summary comes
// from the delivery sent.
func TestPreprocessPinnedAccounting(t *testing.T) {
	h := hudFixture(t, "", 0)
	s, _ := injectionSeed(t, h, "PIN")
	ids := injectionSave(t, h.db, s[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"a","body":"리나는 빨간 약속을 기억한다"},{"id":"m2","kind":"fact","about":"n1","slot":"b","body":"리나는 파란 약속을 기억한다"},{"id":"m3","kind":"fact","about":"n1","slot":"c","body":"리나는 노란 약속을 기억한다"}]}`)
	pin := ids["memories/0"]
	if _, err := h.db.SetMemoryHold("chat:PIN", pin, store.HoldPin); err != nil {
		t.Fatal(err)
	}
	r := r18Request()
	w, _ := h.db.Deliver(s, r)
	if len(w.IDs) != 3 {
		t.Fatalf("fixture %v", w.IDs)
	}
	r.Chars = r18Size(w, w.IDs[0]) + r18Size(w, w.IDs[1])
	b, _ := h.db.Deliver(s, r)
	if len(b.IDs) != 2 || !r18Has(b.IDs, pin) {
		t.Fatalf("fixture basic %v", b.IDs)
	}
	f := newPrepFakes(t)
	f.decUnrelated["빨간"] = true // the pinned one
	dec, err := provider.NewDecider(provider.DecisionConfig{Provider: "ollama", URL: f.dec.URL})
	if err != nil {
		t.Fatal(err)
	}
	d, rec := preprocess(context.Background(), &prepDeps{mode: store.PreprocessJev, decider: dec}, h.db, s, r, b, r.Input)
	if !reflect.DeepEqual(d.IDs, b.IDs) || rec.Removed != 0 || rec.Added != 0 {
		t.Fatalf("pinned: %v -> %v removed %d added %d", b.IDs, d.IDs, rec.Removed, rec.Added)
	}
}

// B-03: Go renders the final set again; an addition chosen first is not
// pushed out by a later one that grew there (its basis definition left out).
func TestPreprocessFinalRenderKeepsPriority(t *testing.T) {
	h := hudFixture(t, "", 0)
	s, _ := injectionSeed(t, h, "BAS")
	ids := injectionSave(t, h.db, s[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"view","owner":"n1","body":"버릴 생각","basis":{"ref":"m4"}},{"id":"m2","kind":"view","owner":"n1","body":"남길 생각","basis":{"ref":"m4"}},{"id":"m3","kind":"fact","about":"n1","slot":"a","body":"우선할 기억 `+strings.Repeat("기록", 30)+`"},{"id":"m4","kind":"event","body":"근거 원문 `+strings.Repeat("긴 근거", 70)+`"}]}`)
	a, later, first := ids["memories/0"], ids["memories/1"], ids["memories/2"]
	r := r18Request()
	w, _ := h.db.Deliver(s, r)
	r.Chars = r18Size(w, a)
	b, _ := h.db.Deliver(s, r)
	if !reflect.DeepEqual(b.IDs, []string{a}) || r18Size(w, later)+r18Size(w, first) > r.Chars {
		t.Fatalf("fixture basic=%v", b.IDs)
	}
	f := newPrepFakes(t)
	f.decP["버릴 생각"], f.decP["남길 생각"], f.decP["우선할 기억"], f.decP["근거 원문"] = 0.9, 0.2, 0.1, 0.9
	dec, err := provider.NewDecider(provider.DecisionConfig{Provider: "ollama", URL: f.dec.URL})
	if err != nil {
		t.Fatal(err)
	}
	d, rec := preprocess(context.Background(), &prepDeps{mode: store.PreprocessJev, decider: dec}, h.db, s, r, b, r.Input)
	if !r18Has(d.IDs, first) || d.Chars > r.Chars {
		t.Fatalf("the addition chosen first (%s) was pushed out: %v chars %d/%d", first, d.IDs, d.Chars, r.Chars)
	}
	removed, added := 0, 0
	for _, id := range b.IDs {
		if !r18Has(d.IDs, id) {
			removed++
		}
	}
	for _, id := range d.IDs {
		if !r18Has(b.IDs, id) {
			added++
		}
	}
	if rec.Removed != removed || rec.Added != added {
		t.Fatalf("summary %d/%d, sent %d/%d (%v -> %v)", rec.Removed, rec.Added, removed, added, b.IDs, d.IDs)
	}
}

// B-04: a failed settings read delivers the basic delivery and says so.
func TestPreprocessReadFailureRecorded(t *testing.T) {
	h := hudFixture(t, "", 0)
	s, _ := injectionSeed(t, h, "RF")
	injectionSave(t, h.db, s[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"리나는 약사다"}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	off := injectionRequest(t, srv, h, "RF")
	if err := h.db.SetPreprocessSettings(h.st.mods.box, store.PreprocessSettings{Mode: store.PreprocessAI}); err != nil {
		t.Fatal(err)
	}
	c, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Exec(`UPDATE settings SET value='not-json' WHERE name='prep_ai_options'`); err != nil {
		t.Fatal(err)
	}
	a := injectionRequest(t, srv, h, "RF")
	pre, _ := a.V["preprocess"].(map[string]any)
	if a.V["block"] != off.V["block"] || pre == nil || pre["fallback"] != "store_failed" || pre["ran"] != "go" {
		t.Fatalf("read failure: %v", a.V["preprocess"])
	}
	if tr := lastTrace(t, h, "RF"); tr.Summary.Preprocess == nil || tr.Summary.Preprocess.Fallback != "store_failed" {
		t.Fatalf("stored: %+v", tr.Summary.Preprocess)
	}
}

// A-01 (through /memory/inject): the AI's address redirects; the key and the
// story do not go to the other server and the basic delivery goes.
func TestPreprocessAIRedirectNotFollowed(t *testing.T) {
	h := hudFixture(t, "", 0)
	s, _ := injectionSeed(t, h, "RD")
	injectionSave(t, h.db, s[0].Source, `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"합성 비밀의 열쇠는 월장석","hide_from":["n2"]}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	off := injectionRequest(t, srv, h, "RD")
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"unrelated":[]}`}}}})
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if err := h.db.SetPreprocessSettings(h.st.mods.box, store.PreprocessSettings{Mode: store.PreprocessAI, AIOwn: true, AI: provider.Config{Provider: "custom", Model: "fake", URL: redirect.URL + "/v1", Key: "synthetic-redirect-key"}}); err != nil {
		t.Fatal(err)
	}
	a := injectionRequest(t, srv, h, "RD")
	pre, _ := a.V["preprocess"].(map[string]any)
	if calls != 0 || a.V["block"] != off.V["block"] || pre == nil || pre["fallback"] != "ai_failed" {
		t.Fatalf("redirect followed: calls %d record %v", calls, pre)
	}
	if strings.Contains(a.Raw, "synthetic-redirect-key") {
		t.Fatal("key in the answer")
	}
}

// A-02: an address holding the key that cannot be parsed: the error, the
// answer and the stored record do not show the key.
func TestPreprocessMalformedURLKeyMasked(t *testing.T) {
	h := hudFixture(t, "", 0)
	s, _ := injectionSeed(t, h, "KEY")
	injectionSave(t, h.db, s[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"연결 오류 시험"}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	for _, tc := range []struct{ mode, provider, path string }{{store.PreprocessAI, "custom", "/v1"}, {store.PreprocessAI, "ollama", ""}, {store.PreprocessJev, "", ""}} {
		key := "synthetic-review-url-key-1357"
		url := "http://127.0.0.1:1/" + key + "/bad%ZZ"
		if err := h.db.SetDecisionSettings(h.st.mods.box, store.DecisionSettings{Provider: "ollama", URL: url, Key: key}); err != nil {
			t.Fatal(err)
		}
		cfg := provider.Config{Provider: tc.provider, Model: "fake", URL: url + tc.path, Key: key}
		if tc.provider == "" {
			cfg = provider.Config{}
		}
		if err := h.db.SetPreprocessSettings(h.st.mods.box, store.PreprocessSettings{Mode: tc.mode, AIOwn: tc.provider != "", AI: cfg}); err != nil {
			t.Fatal(err)
		}
		a := injectionRequest(t, srv, h, "KEY")
		rec := lastTrace(t, h, "KEY").Summary.Preprocess
		if rec == nil || rec.Ran != "go" || strings.Contains(a.Raw, key) || strings.Contains(rec.Error, key) {
			t.Fatalf("%s %s: key shown: %+v", tc.mode, tc.provider, rec)
		}
	}
}
