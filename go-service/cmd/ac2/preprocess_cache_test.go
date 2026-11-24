package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Expect exact packets, in any arrival order. Each run drains the script,
// so an unexpected HTTP call fails even if preprocessing recovers from it.
type decisionScript struct {
	t       *testing.T
	mu      sync.Mutex
	server  *httptest.Server
	state   string
	lines   map[string]string
	packets [][]string
	calls   int
	omit    map[string]bool
	fail    bool
	drop    string
}

func newDecisionScript(t *testing.T) *decisionScript {
	s := &decisionScript{t: t}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *decisionScript) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	var body struct {
		Model, State string
		Questions    map[string]struct {
			Type, Instructions string
			Criteria           map[string]string
		}
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Method != "POST" || r.URL.Path != "/v1/systemone" || body.Model != "clef:cloud" || body.State != s.state {
		s.t.Errorf("unexpected decision envelope: %s %s model %q state %q err %v", r.Method, r.URL.Path, body.Model, body.State, err)
		http.Error(w, "bad envelope", 400)
		return
	}
	var ids []string
	answers := map[string]any{}
	for id, q := range body.Questions {
		line, ok := s.lines[id]
		if !ok || q.Type != "noul" || q.Instructions != prepQuestion+line || q.Criteria["true"] != prepUnrelatedYes || q.Criteria["false"] != prepUnrelatedNo {
			s.t.Errorf("unexpected question %q: %+v", id, q)
			http.Error(w, "bad question", 400)
			return
		}
		ids = append(ids, id)
		if !s.omit[id] {
			v := 0.0
			if id == s.drop {
				v = 0.5
			}
			answers[id] = map[string]float64{"noul": v}
		}
	}
	slices.Sort(ids)
	for i, packet := range s.packets {
		if slices.Equal(packet, ids) {
			s.packets = slices.Delete(s.packets, i, i+1)
			if s.fail {
				http.Error(w, "unavailable", 503)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]int{"input_tokens": len(ids) * 10}})
			return
		}
	}
	s.t.Errorf("unexpected decision packet %v, pending %v", ids, s.packets)
	http.Error(w, "unexpected packet", 400)
}

func (s *decisionScript) run(deps *prepDeps, db *store.DB, scope []memory.ScopedTurn, dr store.DeliveryRequest, basic store.Delivery, packets ...[]string) (store.Delivery, int) {
	s.t.Helper()
	s.mu.Lock()
	s.state = prepState(dr.Input, scope, min(basic.Window, decisionTurns))
	s.packets = nil
	for _, packet := range packets {
		sorted := slices.Clone(packet)
		slices.Sort(sorted)
		s.packets = append(s.packets, sorted)
	}
	s.calls = 0
	s.mu.Unlock()
	d, rec := preprocess(context.Background(), deps, db, scope, dr, basic, dr.Input)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.packets) != 0 || s.calls != len(packets) {
		s.t.Fatalf("HTTP calls %d, wanted %d, missing packets %v", s.calls, len(packets), s.packets)
	}
	if s.fail {
		if rec.Ran != "go" || rec.Error == "" || rec.Reused != 0 || rec.InputTokens != 0 {
			s.t.Fatalf("failed request: %+v", rec)
		}
	} else {
		sent := 0
		for _, p := range packets {
			sent += len(p)
		}
		if rec.DecisionCalls != s.calls || rec.InputTokens != int64(sent*10) || rec.Asked != sent+rec.Reused {
			s.t.Fatalf("asked %d reused %d decision calls %d input tokens %d, sent %d lines in %d packets", rec.Asked, rec.Reused, rec.DecisionCalls, rec.InputTokens, sent, s.calls)
		}
	}
	return d, rec.Reused
}

func (s *decisionScript) deps() *prepDeps {
	s.t.Helper()
	dec, err := provider.NewDecider(provider.DecisionConfig{Provider: "ollama", URL: s.server.URL})
	if err != nil {
		s.t.Fatal(err)
	}
	return &prepDeps{mode: store.PreprocessAIJev, decider: dec}
}

func TestPreprocessDecisionCacheSelection(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "cache")
	var memories []map[string]string
	for i := 0; i < 70; i++ {
		tag := fmt.Sprintf("%02d", i)
		memories = append(memories, map[string]string{"id": "m" + tag, "kind": "fact", "about": "n1", "slot": "slot" + tag, "body": "기억 " + tag})
	}
	raw, err := json.Marshal(map[string]any{"entities": []map[string]string{{"id": "n1", "name": "리나"}}, "memories": memories})
	if err != nil {
		t.Fatal(err)
	}
	injectionSave(t, h.db, scope[0].Source, string(raw))
	dr := r18Request()
	wide, err := h.db.Deliver(scope, dr)
	if err != nil || len(wide.IDs) != len(memories) {
		t.Fatalf("wide fixture: %d, %v", len(wide.IDs), err)
	}
	dr.Chars = r18Size(wide, wide.IDs[0]) + r18Size(wide, wide.IDs[1])
	basic, err := h.db.Deliver(scope, dr)
	if err != nil || !slices.Equal(basic.IDs, wide.IDs[:2]) {
		t.Fatalf("basic fixture: %v, %v", basic.IDs, err)
	}
	s := newDecisionScript(t)
	s.lines, s.drop = wholeLines(wide), basic.IDs[1]
	if err := h.db.SetDecisionSettings(h.st.mods.box, store.DecisionSettings{Provider: "ollama", URL: s.server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetPreprocessSettings(h.st.mods.box, store.PreprocessSettings{Mode: store.PreprocessJev}); err != nil {
		t.Fatal(err)
	}
	run := func(packets ...[]string) (store.Delivery, int) {
		t.Helper()
		p, problem := h.st.mods.prepFor()
		if p == nil || p.decider == nil {
			t.Fatalf("prepFor: %q %+v", problem, p)
		}
		return s.run(p, h.db, scope, dr, basic, packets...)
	}
	rest := wide.IDs[2:]
	cold := [][]string{basic.IDs, rest[:64], rest[64:]}
	first, reused := run(cold...)
	if reused != 0 || !slices.Contains(first.IDs, basic.IDs[0]) || slices.Contains(first.IDs, s.drop) || !slices.Contains(first.IDs, rest[0]) || len(first.IDs) != 2 {
		t.Fatalf("cold selection: %v, reused %d", first.IDs, reused)
	}
	second, reused := run()
	if !slices.Equal(first.IDs, second.IDs) || first.Block != second.Block || reused != len(wide.IDs) {
		t.Fatalf("repeat: %v, reused %d", second.IDs, reused)
	}

	dr.Input += "!"
	// Leave holes across the original packets. A retry must form packets
	// from the remaining questions in order, keeping the basic lane apart.
	missing := slices.Delete(slices.Clone(rest), 1, 3)
	s.omit = map[string]bool{basic.IDs[0]: true}
	for _, id := range missing {
		s.omit[id] = true
	}
	run(cold...)
	s.omit = nil
	retry, reused := run(basic.IDs[:1], missing[:64], missing[64:])
	if !slices.Equal(retry.IDs, first.IDs) || reused != len(wide.IDs)-len(missing)-1 {
		t.Fatalf("partial retry: %v, reused %d", retry.IDs, reused)
	}
}

func TestPreprocessDecisionCacheFailureAndEviction(t *testing.T) {
	s := newDecisionScript(t)
	deps := s.deps()
	b := store.Delivery{IDs: []string{"a"}, Whole: []string{"line a"}}
	s.lines = wholeLines(b)
	dr := r18Request()
	s.fail = true
	d, _ := s.run(deps, nil, nil, dr, b, b.IDs)
	if !slices.Equal(d.IDs, b.IDs) {
		t.Fatalf("failed decision changed basic: %v", d.IDs)
	}
	s.fail = false
	s.run(deps, nil, nil, dr, b, b.IDs)
	// The same text under a different row ID takes the saved zero score.
	b.IDs[0] = "different"
	s.lines = wholeLines(b)
	if _, reused := s.run(deps, nil, nil, dr, b); reused != 1 {
		t.Fatalf("rekeyed line reused %d", reused)
	}
	first := dr.Input
	for i := 1; i <= decisionCacheScenes; i++ {
		dr.Input = first + strconv.Itoa(i)
		s.run(deps, nil, nil, dr, b, b.IDs)
	}
	dr.Input = first + "1"
	s.run(deps, nil, nil, dr, b)
	dr.Input = first
	s.run(deps, nil, nil, dr, b, b.IDs)
}

func TestDecisionCacheKeys(t *testing.T) {
	cfg := provider.DecisionConfig{Provider: "ollama", URL: "http://localhost:1", Model: "model"}
	key := func(cfg provider.DecisionConfig) decisionDigest {
		t.Helper()
		d, err := provider.NewDecider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return decisionStateKey(d, "scene")
	}
	base := key(cfg)
	other := cfg
	other.Provider = "ollama_cloud"
	other.Key = "test"
	if key(other) == base {
		t.Fatal("provider missing from scene key")
	}
	other = cfg
	other.URL += "/other"
	if key(other) == base {
		t.Fatal("endpoint missing from scene key")
	}
	other = cfg
	other.Model += "2"
	if key(other) == base {
		t.Fatal("model missing from scene key")
	}
	other = cfg
	other.Key = "rotated"
	other.URL += "/v1/systemone"
	if key(other) != base {
		t.Fatal("same effective connection missed")
	}
	if decisionHash("line", "yes", "no") == decisionHash("line", "ye", "sno") {
		t.Fatal("question field boundaries lost")
	}
}

func TestDecisionCacheAnswerLimit(t *testing.T) {
	var c decisionCache
	older, newer := decisionHash("older"), decisionHash("newer")
	q := decisionHash("shared question")
	c.remember(older, q, 0.5)
	c.remember(newer, q, 0)
	for i := 0; i < decisionCacheAnswers-2; i++ {
		c.remember(newer, decisionHash(strconv.Itoa(i)), 1)
	}
	if c.count != decisionCacheAnswers {
		t.Fatalf("cache filled to %d", c.count)
	}
	c.remember(newer, decisionHash("overflow"), 1)
	if _, ok := c.lookup(older, q); ok {
		t.Fatal("oldest scene survived overflow")
	}
	if v, ok := c.lookup(newer, q); !ok || v != 0 || c.count != decisionCacheAnswers {
		t.Fatalf("newer answer %v, %v, entries %d", v, ok, c.count)
	}
}

// Exercise the observed workload over a long session. Report the live cache
// size separately from allocations for hashing and constructing questions.
func BenchmarkDecisionCacheRetention(b *testing.B) {
	const turns, perTurn = 320, 330
	for b.Loop() {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		var c decisionCache
		for turn := 0; turn < turns; turn++ {
			scene := decisionHash(strconv.Itoa(turn))
			for i := 0; i < perTurn; i++ {
				c.remember(scene, decisionHash(strconv.Itoa(i)), 0.5)
			}
		}
		if c.count != perTurn*decisionCacheScenes || len(c.scenes) != decisionCacheScenes {
			b.Fatalf("retained %d answers in %d scenes", c.count, len(c.scenes))
		}
		b.ReportMetric(float64(c.count), "retained-answers")
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(&c)
		b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained-heap-B")
	}
}
