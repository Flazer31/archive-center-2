package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// prepFakes are the two external boundaries of preprocessing: a System One
// server (the decision model) and an OpenAI-compatible chat server (the AI).
// Both record what they were asked; "unrelated" says which memory bodies each
// calls unrelated. fail makes one answer 500.
type prepFakes struct {
	mu                 sync.Mutex
	decUnrelated       map[string]bool    // body substring -> unrelated (0.9), else 0.1
	decP               map[string]float64 // body substring -> this probability
	aiUnrelated        map[string]bool
	decFail, aiFail    bool
	decCalls, aiCalls  int
	decAsked           []string   // question texts
	decRequests        [][]string // question texts, per request
	decState, aiPrompt string
	dec, ai            *httptest.Server
}

func newPrepFakes(t *testing.T) *prepFakes {
	f := &prepFakes{decUnrelated: map[string]bool{}, aiUnrelated: map[string]bool{}, decP: map[string]float64{}}
	f.dec = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.decCalls++
		f.decState = body.State
		if r.URL.Path != "/v1/systemone" || f.decFail {
			http.Error(w, "down", 500)
			return
		}
		answers := map[string]any{}
		var req []string
		for _, q := range body.Questions {
			req = append(req, q.Instructions)
		}
		f.decRequests = append(f.decRequests, req)
		for id, q := range body.Questions {
			f.decAsked = append(f.decAsked, q.Instructions)
			p := 0.1
			for s := range f.decUnrelated {
				if strings.Contains(q.Instructions, s) {
					p = 0.9
				}
			}
			for s, v := range f.decP {
				if strings.Contains(q.Instructions, s) {
					p = v
				}
			}
			answers[id] = map[string]any{"type": "noul", "noul": p}
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]any{"input_tokens": 10 * len(body.Questions)}})
	}))
	f.ai = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.aiCalls++
		if len(body.Messages) > 0 {
			f.aiPrompt = body.Messages[len(body.Messages)-1].Content
		}
		if f.aiFail {
			http.Error(w, "down", 500)
			return
		}
		var refs []string
		for _, m := range regexp.MustCompile(`(?m)^(L\d+) (.*)$`).FindAllStringSubmatch(f.aiPrompt, -1) {
			for s := range f.aiUnrelated {
				if strings.Contains(m[2], s) {
					refs = append(refs, m[1])
				}
			}
		}
		out, _ := json.Marshal(map[string]any{"unrelated": refs})
		// prose after the JSON, as some models add (the first object is read)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(out) + "\n이유: 다른 장면"}}}})
	}))
	t.Cleanup(func() { f.dec.Close(); f.ai.Close() })
	return f
}

func (f *prepFakes) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decCalls, f.aiCalls, f.decAsked, f.decRequests, f.decState, f.aiPrompt = 0, 0, nil, nil, "", ""
	f.decFail, f.aiFail = false, false
	f.decUnrelated, f.aiUnrelated = map[string]bool{}, map[string]bool{}
}

func prepSet(t *testing.T, h *hud, f *prepFakes, mode string) {
	t.Helper()
	if err := h.db.SetDecisionSettings(h.st.mods.box, store.DecisionSettings{Provider: "ollama", URL: f.dec.URL}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetPreprocessSettings(h.st.mods.box, store.PreprocessSettings{Mode: mode, AIOwn: true, AI: provider.Config{Provider: "custom", Model: "fake-ai", URL: f.ai.URL + "/v1"}}); err != nil {
		t.Fatal(err)
	}
}

func lastTrace(t *testing.T, h *hud, chat string) store.DeliveryTrace {
	t.Helper()
	ts, err := h.db.DeliveryTraces("chat:"+chat, 1)
	if err != nil || len(ts) == 0 {
		t.Fatalf("stored delivery: %v %v", len(ts), err)
	}
	return ts[0]
}

func prepIDs(a memAnswer) []string {
	var out []string
	ids, _ := a.V["ids"].([]any) // null when nothing is delivered
	for _, v := range ids {
		out = append(out, v.(string))
	}
	return out
}

// Production /memory/inject with real MariaDB and the four modes; only the
// decision model and the AI are fakes (recording, failing on request).
// Expectations come from the off delivery's own order and sizes.
func TestPreprocessModesThroughInject(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "P")
	bodies := []string{"리나는 약국에서 일한다", "리나는 낚시를 좋아한다", "리나는 약속을 해 지기 전에 지킨다", "리나는 도윤과 오래 알았다. " + strings.Repeat("함께 지낸 날들의 기록이 길다. ", 20)}
	var ms []string
	for i, b := range bodies {
		ms = append(ms, fmt.Sprintf(`{"id":"m%d","kind":"fact","about":"n1","slot":"s%d","mode":"상태","body":"%s"}`, i+1, i+1, b))
	}
	injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[`+strings.Join(ms, ",")+`]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	f := newPrepFakes(t)

	// the off delivery with room for every record gives Go's order and sizes
	mode, marker, err := h.db.MemoryPlacement()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetMemorySettings(100000, 4000, mode, marker); err != nil {
		t.Fatal(err)
	}
	all := injectionRequest(t, srv, h, "P")
	order := prepIDs(all)
	if len(order) != 4 {
		t.Fatalf("fixture: %v", order)
	}
	line := map[string]string{}
	for i, l := range strings.Split(all.V["block"].(string), "\n")[1:5] {
		line[order[i]] = l
	}
	bodyOf := func(id string) string {
		for _, b := range bodies {
			if strings.Contains(line[id], b) {
				return b
			}
		}
		t.Fatalf("no body for %s", id)
		return ""
	}
	// a ceiling with room for the first two records (in Go's own sizes)
	size := map[string]int{}
	d := lastTrace(t, h, "P")
	for _, r := range d.Trace {
		size[r.ID] = r.Size
	}
	// room for the first and the larger of the next two: the third fits in
	// the second's place; the long fourth never fits
	room := size[order[0]] + max(size[order[1]], size[order[2]])
	if err := h.db.SetMemorySettings(room, 4000, mode, marker); err != nil {
		t.Fatal(err)
	}
	off := injectionRequest(t, srv, h, "P")
	basic := prepIDs(off)
	if len(basic) != 2 || basic[0] != order[0] || basic[1] != order[1] || off.V["preprocess"] != nil {
		t.Fatalf("off: %v", basic)
	}
	if f.decCalls+f.aiCalls != 0 {
		t.Fatal("off called a model")
	}
	unrelated, next := bodyOf(order[1]), order[2]

	// JEV: the second basic record is unrelated; the third (outside the
	// basic delivery) takes its place within the same ceiling
	f.reset()
	f.decUnrelated[unrelated] = true
	prepSet(t, h, f, store.PreprocessJev)
	jev := injectionRequest(t, srv, h, "P")
	got := prepIDs(jev)
	if len(got) != 2 || got[0] != order[0] || got[1] != next || f.decCalls == 0 || f.aiCalls != 0 {
		t.Fatalf("jev: %v (want %s %s), calls %d/%d", got, order[0], next, f.decCalls, f.aiCalls)
	}
	if len(f.decAsked) != 4 || !strings.Contains(f.decState, "### 현재 사용자 입력\n리나의 약속과 생각") {
		t.Fatalf("jev asked %d lines (want the wide list, 4); state %q", len(f.decAsked), f.decState)
	}
	// the basic delivery's two lines go in a request of their own
	own := false
	for _, q := range f.decRequests {
		own = own || (len(q) == 2 && strings.Contains(strings.Join(q, " | "), bodyOf(order[0])) && strings.Contains(strings.Join(q, " | "), unrelated))
	}
	if !own || len(f.decRequests) != 2 {
		t.Fatalf("jev requests (want the basic lines apart): %d %v", len(f.decRequests), f.decRequests)
	}
	pre := jev.V["preprocess"].(map[string]any)
	if pre["ran"] != store.PreprocessJev || pre["removed"] != float64(1) || pre["added"] != float64(1) || pre["asked"] != float64(4) {
		t.Fatalf("jev record: %v", pre)
	}
	stored := lastTrace(t, h, "P")
	if stored.Summary.Preprocess == nil || stored.Summary.Preprocess.Ran != store.PreprocessJev {
		t.Fatalf("stored record: %+v", stored.Summary.Preprocess)
	}
	for _, r := range stored.Trace {
		if r.ID == order[1] && (r.Result != "not_chosen" || r.Unrelated == nil || *r.Unrelated != 0.9) {
			t.Fatalf("trace of the left-out record: %+v", r)
		}
	}

	// AI: the AI is shown the basic delivery only and its line is left out;
	// nothing is added
	f.reset()
	f.aiUnrelated[unrelated] = true
	prepSet(t, h, f, store.PreprocessAI)
	ai := injectionRequest(t, srv, h, "P")
	if got := prepIDs(ai); len(got) != 1 || got[0] != order[0] || f.aiCalls != 1 || f.decCalls != 0 {
		t.Fatalf("ai: %v calls %d/%d", got, f.decCalls, f.aiCalls)
	}
	if !strings.Contains(f.aiPrompt, "L2 ") || strings.Contains(f.aiPrompt, "L3 ") || !strings.Contains(f.aiPrompt, "[대화]\n### 현재 사용자 입력") {
		t.Fatalf("ai prompt does not show the two basic lines: %q", f.aiPrompt)
	}

	// AI + JEV: left out only when both call it unrelated
	f.reset()
	f.aiUnrelated[unrelated] = true
	f.aiUnrelated[bodyOf(order[0])] = true // the AI alone: kept
	f.decUnrelated[unrelated] = true
	prepSet(t, h, f, store.PreprocessAIJev)
	both := injectionRequest(t, srv, h, "P")
	if got := prepIDs(both); len(got) != 1 || got[0] != order[0] || f.aiCalls != 1 || f.decCalls == 0 || len(f.decAsked) != 2 {
		t.Fatalf("ai+jev: %v calls %d/%d asked %d", got, f.decCalls, f.aiCalls, len(f.decAsked))
	}
	if pre := both.V["preprocess"].(map[string]any); pre["ran"] != store.PreprocessAIJev || pre["removed"] != float64(1) {
		t.Fatalf("ai+jev record: %v", pre)
	}

	// failures keep the basic delivery: JEV with the decision model down
	f.reset()
	f.decFail = true
	prepSet(t, h, f, store.PreprocessJev)
	down := injectionRequest(t, srv, h, "P")
	if down.V["block"] != off.V["block"] {
		t.Fatalf("jev failure changed the delivery: %v", prepIDs(down))
	}
	if pre := down.V["preprocess"].(map[string]any); pre["ran"] != "go" || pre["fallback"] != "decision_failed" {
		t.Fatalf("jev failure record: %v", pre)
	}
	// AI + JEV with the AI down: the decision model alone decides
	f.reset()
	f.aiFail = true
	f.decUnrelated[unrelated] = true
	prepSet(t, h, f, store.PreprocessAIJev)
	half := injectionRequest(t, srv, h, "P")
	if got := prepIDs(half); len(got) != 1 || got[0] != order[0] {
		t.Fatalf("ai+jev with the AI down: %v", got)
	}
	if pre := half.V["preprocess"].(map[string]any); pre["ran"] != store.PreprocessJev || pre["fallback"] != "ai_failed" {
		t.Fatalf("ai+jev half record: %v", pre)
	}
	// AI with the AI down
	f.reset()
	f.aiFail = true
	prepSet(t, h, f, store.PreprocessAI)
	if a := injectionRequest(t, srv, h, "P"); a.V["block"] != off.V["block"] {
		t.Fatalf("ai failure changed the delivery: %v", prepIDs(a))
	}
	// back off: no call, the basic delivery
	f.reset()
	prepSet(t, h, f, store.PreprocessOff)
	if a := injectionRequest(t, srv, h, "P"); a.V["block"] != off.V["block"] || a.V["preprocess"] != nil || f.decCalls+f.aiCalls != 0 {
		t.Fatal("off again is not the basic delivery")
	}
}

// The AI borrows the critic's connection unless it has its own; a mode that
// needs a missing connection says so and falls back (nothing is called).
func TestPreprocessConnections(t *testing.T) {
	h := hudFixture(t, "", 0)
	m := h.st.mods
	if p, _ := m.prepFor(); p != nil {
		t.Fatalf("off by default: %+v", p)
	}
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: store.PreprocessAI}); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.prepFor(); p == nil || p.ai != nil || p.aiProblem != "ai_off" {
		t.Fatalf("no critic to borrow: %+v", p)
	}
	m.flagCritic = provider.Config{Provider: "ollama", Model: "fake", URL: "http://127.0.0.1:1"}
	if p, _ := m.prepFor(); p == nil || p.ai == nil {
		t.Fatalf("borrowing the critic: %+v", p)
	}
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: store.PreprocessJev}); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.prepFor(); p == nil || p.decider != nil || p.decisionProblem != "decision_off" || p.ai != nil {
		t.Fatalf("jev without a decision model: %+v", p)
	}
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: "cards"}); err == nil {
		t.Fatal("unknown mode saved")
	}
}
