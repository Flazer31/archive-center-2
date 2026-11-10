package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// The real narrative caller must repeat the unedited current input after
// the dialog and memory, including when saved publisher rules are used.
func TestNarrativeGuideCurrentInputAtEnd(t *testing.T) {
	for _, head := range []string{"", "사용자가 저장한 편집 규칙"} {
		t.Run(head, func(t *testing.T) {
			f := newNarrFake(t)
			f.head = head
			f.guide = `{"connections":[]}`
			ai, err := provider.New(provider.Config{Provider: "custom", Model: "test", URL: f.srv.URL + "/v1"})
			if err != nil {
				t.Fatal(err)
			}
			input := "  다음 날 길드로 간다.\n\"기다려 줘.\"  "
			n := &narrDeps{head: head, ai: ai, s: store.NarrativeSettings{Level: store.GuideStrong, Chars: store.DefaultGuideChars}}
			narrative(context.Background(), n, store.Delivery{}, nil, input, time.Now())
			prompts := f.take()
			end := "[기억 줄]\n(없음)\n\n[이번 사용자 입력]\n" + input + "\n\n이 입력에서 이어질 이번 응답의 가이드를 지정된 JSON으로 작성한다."
			if len(prompts) != 1 || !strings.HasSuffix(prompts[0], end) {
				t.Fatalf("current input must remain verbatim at the end: %q", prompts)
			}
		})
	}
}

// narrFake is the publisher (an OpenAI-compatible chat server, the only
// external boundary): it records each prompt and answers the guide prompt
// with guide and the input prompts with input; fail answers 500.
type narrFake struct {
	mu           sync.Mutex
	guide, input string
	head         string // a saved publisher prompt the guide prompt starts with
	fail         bool
	prompts      []string
	srv          *httptest.Server
}

func newNarrFake(t *testing.T) *narrFake {
	f := &narrFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		p := body.Messages[len(body.Messages)-1].Content
		f.prompts = append(f.prompts, p)
		if f.fail {
			http.Error(w, "down", 500)
			return
		}
		answer := ""
		switch {
		case strings.HasPrefix(p, narrGuideHead), f.head != "" && strings.HasPrefix(p, f.head):
			answer = f.guide
		case strings.HasPrefix(p, narrClarifyHead), strings.HasPrefix(p, narrBlendHead):
			answer = f.input
		default:
			t.Errorf("unexpected prompt: %.80q", p)
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *narrFake) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.prompts
	f.prompts = nil
	return p
}

func narrSet(t *testing.T, h *hud, f *narrFake, level, input string, chars int) {
	t.Helper()
	if err := h.db.SetNarrativeSettings(h.st.mods.box, store.NarrativeSettings{Level: level, Input: input, Chars: chars, AIOwn: true,
		AI: provider.Config{Provider: "custom", Model: "fake-publisher", URL: f.srv.URL + "/v1"}}); err != nil {
		t.Fatal(err)
	}
}

func narrSeed(t *testing.T, h *hud, chat string) *observe.Server {
	t.Helper()
	scope, _ := injectionSeed(t, h, chat)
	injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나","type":"person"},{"id":"n2","name":"도윤","type":"person"}],"memories":[
	 {"id":"m1","kind":"fact","about":"n1","slot":"약속","mode":"상태","body":"리나는 해 지기 전에 돌아오겠다고 약속했다"},
	 {"id":"m2","kind":"fact","about":"n1","slot":"일","mode":"상태","body":"리나는 약국에서 일한다"},
	 {"id":"m3","kind":"fact","about":"n1","slot":"비밀","mode":"상태","body":"리나는 왕녀다","hide_from":["n2"]}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	return srv
}

func guideOf(a memAnswer) (after, before string) {
	g, _ := a.V["guide"].(map[string]any)
	after, _ = g["after_memory"].(string)
	before, _ = g["before_reply"].(string)
	return
}

// Production /memory/inject: the guide levels add one kind of item each,
// an item without a basis is dropped, a field the level lacks is thrown
// away, secret lines are never shown to the publisher, and the guide goes
// as titled messages beside an unchanged memory block.
func TestNarrativeGuideLevels(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := narrSeed(t, h, "N")
	f := newNarrFake(t)
	off := injectionRequest(t, srv, h, "N")
	block := off.V["block"].(string)
	if off.V["guide"] != nil || len(f.take()) != 0 {
		t.Fatal("off: a guide or a call")
	}
	if !strings.Contains(block, "왕녀") || !strings.Contains(block, "약속했다") {
		t.Fatalf("fixture block: %q", block)
	}
	// every kind answered, one item without a basis, one with an unknown line
	f.guide = `{"connections":[{"text":"리나의 약속이 아직 남았다","refs":["L1"]},{"text":"근거 없는 말","refs":[]},{"text":"없는 줄","refs":["L99"]}],
	 "reactions":[{"who":"리나","text":"약속을 떠올린다","refs":["L1"]}],
	 "beats":[{"part":"시작","text":"리나가 시계를 본다","refs":["D"]}]} 덧붙인 말`
	narrSet(t, h, f, store.GuideWeak, store.InputOff, store.DefaultGuideChars)
	weak := injectionRequest(t, srv, h, "N")
	prompts := f.take()
	if len(prompts) != 1 {
		t.Fatalf("weak calls: %d", len(prompts))
	}
	if strings.Contains(prompts[0], "왕녀") || !strings.Contains(prompts[0], "약속했다") || strings.Contains(prompts[0], "reactions") || strings.Contains(prompts[0], "beats") {
		t.Fatalf("weak prompt (secret shown or fields beyond the level): %q", prompts[0])
	}
	after, before := guideOf(weak)
	if weak.V["block"] != block || before != "" || !strings.HasPrefix(after, store.GuideTitle) || !strings.Contains(after, "리나의 약속이 아직 남았다") ||
		strings.Contains(after, "근거 없는") || strings.Contains(after, "없는 줄") || strings.Contains(after, "떠올린다") || !strings.Contains(after, "지난 일과 인물 상태에 대한 배경 메모다") {
		t.Fatalf("weak guide: %q / %q", after, before)
	}
	rec := lastTrace(t, h, "N").Summary.Narrative
	if rec == nil || rec.Guide != "made" || rec.Kept != 1 || rec.Dropped != 2 || rec.GuideText == "" {
		t.Fatalf("weak record: %+v", rec)
	}

	narrSet(t, h, f, store.GuideMid, store.InputOff, store.DefaultGuideChars)
	mid := injectionRequest(t, srv, h, "N")
	f.take()
	after, before = guideOf(mid)
	if before != "" || !strings.Contains(after, "리나: 약속을 떠올린다") || strings.Contains(after, "시계") || !strings.Contains(after, "모두 본문에 넣을 필요는 없다") {
		t.Fatalf("mid guide: %q / %q", after, before)
	}

	narrSet(t, h, f, store.GuideStrong, store.InputOff, store.DefaultGuideChars)
	strong := injectionRequest(t, srv, h, "N")
	f.take()
	after, before = guideOf(strong)
	if !strings.HasPrefix(before, store.GuideTitle) || !strings.Contains(before, "시작: 리나가 시계를 본다") || !strings.Contains(before, "아래 순서에 따라 응답한다") || !strings.Contains(after, "약속을 떠올린다") {
		t.Fatalf("strong guide: %q / %q", after, before)
	}

	// the ceiling: room for the beat only (beats first at strong); the rest
	// is skipped, never cut mid-item
	beat := "[이번 응답 전개 계획 — 현재 입력의 방향과 속도 안에서 아래 순서에 따라 응답한다. 사용자 캐릭터의 미선택 행동은 대신 정하지 않는다. 아래는 앞으로 쓸 응답의 지침이며, 이미 일어난 사건이 아니다. 계획 문구를 본문에서 해설하지 않는다]\n- 시작: 리나가 시계를 본다"
	// counted as sent, title and fingerprint included (review 20 C-01)
	narrSet(t, h, f, store.GuideStrong, store.InputOff, len([]rune(store.GuideMessage(beat))))
	tight := injectionRequest(t, srv, h, "N")
	f.take()
	after, before = guideOf(tight)
	rec = lastTrace(t, h, "N").Summary.Narrative
	if after != "" || !strings.Contains(before, "시계") || rec.Kept != 1 || rec.OverBudget != 2 || rec.Chars > rec.CharsMax {
		t.Fatalf("tight guide: %q / %q %+v", after, before, rec)
	}
	if n := len([]rune(after)) + len([]rune(before)); n > rec.CharsMax || n != rec.Chars {
		t.Fatalf("sent guide %d chars, record %d, ceiling %d", n, rec.Chars, rec.CharsMax)
	}

	// the publisher fails: the request goes as without support
	f.fail = true
	failed := injectionRequest(t, srv, h, "N")
	f.take()
	rec = lastTrace(t, h, "N").Summary.Narrative
	if failed.V["guide"] != nil || failed.V["block"] != block || rec.Guide != "ai_failed" || rec.GuideError == "" {
		t.Fatalf("failed: %v %+v", failed.V["guide"], rec)
	}
	// a guide with no grounded item is none
	f.fail = false
	f.guide = `{"connections":[{"text":"근거 없음"}]}`
	narrSet(t, h, f, store.GuideWeak, store.InputOff, store.DefaultGuideChars)
	none := injectionRequest(t, srv, h, "N")
	f.take()
	if none.V["guide"] != nil || lastTrace(t, h, "N").Summary.Narrative.Guide != "nothing_kept" {
		t.Fatalf("ungrounded only: %v", none.V["guide"])
	}
}

// With no delivered memory, the publisher still reads the dialog and can
// send a dialog-grounded guide through the production injection handler.
func TestNarrativeGuideWithoutMemory(t *testing.T) {
	h := hudFixture(t, "", 0)
	injectionSeed(t, h, "Empty")
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	f := newNarrFake(t)
	text := "리나의 약속과 생각을 이어서 살핀다"
	f.guide = `{"connections":[{"text":"` + text + `","refs":["D"]}]}`
	narrSet(t, h, f, store.GuideWeak, store.InputOff, store.DefaultGuideChars)
	a := injectionRequest(t, srv, h, "Empty")
	if block, _ := a.V["block"].(string); block != "" {
		t.Fatalf("fixture delivered memory: %q", block)
	}
	prompts := f.take()
	if len(prompts) != 1 {
		t.Fatalf("dialog-only publisher calls: %d, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "[기억 줄]\n(없음)\n\n[이번 사용자 입력]\n") || !strings.Contains(prompts[0], "리나의 약속과 생각") {
		t.Fatalf("dialog-only prompt: %q", prompts[0])
	}
	if after, before := guideOf(a); !strings.Contains(after, text) || !strings.HasPrefix(after, store.GuideTitle) || before != "" {
		t.Fatalf("dialog-grounded guide: %q / %q", after, before)
	}
	if r := lastTrace(t, h, "Empty").Summary.Narrative; r == nil || r.Guide != "made" || r.AICalls != 1 || r.Kept != 1 {
		t.Fatalf("dialog-only record: %+v", r)
	}
}

// The input improvement: clarify shows no memory, blend shows the
// non-secret lines; a changed answer replaces the host row's text, an
// unchanged or failed one sends the input as it is. The texts stay off the
// status and HUD paths and are on /injection.
func TestNarrativeInputImprovement(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := narrSeed(t, h, "I")
	f := newNarrFake(t)
	f.input = `{"input":"리나에게 약속과 지금 생각을 묻는다"}`
	narrSet(t, h, f, store.GuideOff, store.InputClarify, store.DefaultGuideChars)
	a := injectionRequest(t, srv, h, "I")
	prompts := f.take()
	if len(prompts) != 1 || strings.Contains(prompts[0], "[기억 줄]") || strings.Contains(prompts[0], "약국") || !strings.Contains(prompts[0], "[사용자 입력]\n리나의 약속과 생각") {
		t.Fatalf("clarify prompt: %q", prompts)
	}
	rw, _ := a.V["input_rewrite"].(map[string]any)
	if rw["from"] != "리나의 약속과 생각" || rw["to"] != "리나에게 약속과 지금 생각을 묻는다" || a.V["guide"] != nil {
		t.Fatalf("clarify rewrite: %v", a.V["input_rewrite"])
	}
	// the stored input is the user's own; the improvement is in the record
	tr := lastTrace(t, h, "I")
	if tr.Summary.Input != "리나의 약속과 생각" || tr.Summary.Narrative.InputResult != "changed" || tr.Summary.Narrative.InputAfter != "리나에게 약속과 지금 생각을 묻는다" {
		t.Fatalf("stored: %q %+v", tr.Summary.Input, tr.Summary.Narrative)
	}
	st := callMem(t, h.st.handler(), "GET", "/status?chat_id=I", "")
	if strings.Contains(st.Raw, "지금 생각을 묻는다") || !strings.Contains(st.Raw, `"input_result":"changed"`) {
		t.Fatalf("status carries the text or no summary: %s", st.Raw)
	}
	if v := injectionGet(t, h, "I"); !strings.Contains(v.Raw, "지금 생각을 묻는다") {
		t.Fatalf("/injection lacks the improved input: %s", v.Raw)
	}

	// Blending (brought back 2026-11-10): the non-secret delivered lines are
	// shown under [기억 줄] with the head that forbids copying them; the
	// secret line is not shown.
	f.input = `{"input":"약속을 지킨 리나에게 묻는다","refs":["L1"]}`
	narrSet(t, h, f, store.GuideOff, store.InputBlend, store.DefaultGuideChars)
	b := injectionRequest(t, srv, h, "I")
	prompts = f.take()
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], narrBlendHead) || !strings.Contains(prompts[0], "[기억 줄]") || !strings.Contains(prompts[0], "약속했다") || strings.Contains(prompts[0], "왕녀") {
		t.Fatalf("blend prompt: %q", prompts)
	}
	if !strings.Contains(narrBlendHead, "기억 줄의 문장을 옮겨 적지 않는다") {
		t.Fatal("the blend head lost the rule against copying memory lines")
	}
	if rw, _ := b.V["input_rewrite"].(map[string]any); rw["to"] != "약속을 지킨 리나에게 묻는다" {
		t.Fatalf("blend rewrite: %v", b.V["input_rewrite"])
	}
	if r := lastTrace(t, h, "I").Summary.Narrative; r.Input != store.InputBlend || r.InputResult != "changed" {
		t.Fatalf("blend record: %+v", r)
	}

	for _, answer := range []string{`{"unchanged":true}`, `{"input":"리나의 약속과 생각"}`, `글만`} {
		f.input = answer
		c := injectionRequest(t, srv, h, "I")
		f.take()
		if c.V["input_rewrite"] != nil {
			t.Fatalf("%s: rewrite %v", answer, c.V["input_rewrite"])
		}
	}
	if r := lastTrace(t, h, "I").Summary.Narrative; r.InputResult != "ai_failed" {
		t.Fatalf("answer without JSON: %+v", r)
	}

	// guide and input at once: two calls, both applied
	f.guide = `{"connections":[{"text":"약속이 남았다","refs":["L1"]}]}`
	f.input = `{"input":"다듬은 입력"}`
	narrSet(t, h, f, store.GuideWeak, store.InputClarify, store.DefaultGuideChars)
	both := injectionRequest(t, srv, h, "I")
	if n := len(f.take()); n != 2 {
		t.Fatalf("both: %d calls", n)
	}
	if after, _ := guideOf(both); after == "" || both.V["input_rewrite"] == nil {
		t.Fatalf("both: %v %v", both.V["guide"], both.V["input_rewrite"])
	}

	// an auxiliary request gets nothing and calls nothing
	body, _ := json.Marshal(map[string]any{"chat_id": "I", "input": "번역", "type": "translate"})
	aux := callMem(t, injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st), "POST", "/memory/inject", string(body))
	if aux.V["skipped"] != "request_type" || len(f.take()) != 0 {
		t.Fatalf("aux: %s", aux.Raw)
	}
}

// Settings: defaults, known choices only, the key sealed and shown by its
// last 4, another provider starting without it.
func TestNarrativeSettings(t *testing.T) {
	h := hudFixture(t, "", 0)
	hd := narrativeHandler(h.st.mods)
	g := callMem(t, hd, "GET", "/settings/narrative", "")
	if g.Code != 200 || g.V["level"] != "" || g.V["input"] != "" || g.V["chars"] != float64(3000) || g.V["ai_own"] != false {
		t.Fatalf("defaults: %s", g.Raw)
	}
	inputs, _ := g.V["inputs"].([]any)
	if len(inputs) != 3 || inputs[0].(map[string]any)["id"] != store.InputOff || inputs[1].(map[string]any)["id"] != store.InputClarify || inputs[2].(map[string]any)["id"] != store.InputBlend {
		t.Errorf("input choices: %v", inputs)
	}
	for _, bad := range []string{`{"level":"max"}`, `{"input":"rewrite"}`, `{"chars":-1}`} {
		if a := callMem(t, hd, "POST", "/settings/narrative", bad); a.Code != 400 {
			t.Fatalf("%s: %d %s", bad, a.Code, a.Raw)
		}
	}
	a := callMem(t, hd, "POST", "/settings/narrative", `{"level":"mid","chars":1200,"input":"clarify","ai_own":true,"ai":{"provider":"openai","model":"m","key":"sk-test-abcd1234"}}`)
	if a.Code != 200 || a.V["level"] != "mid" || a.V["chars"] != float64(1200) || strings.Contains(a.Raw, "sk-test") {
		t.Fatalf("save: %s", a.Raw)
	}
	ai := a.V["ai"].(map[string]any)
	if ai["key_set"] != true || ai["key_last4"] != "1234" {
		t.Fatalf("key view: %v", ai)
	}
	s, err := h.db.NarrativeSettings(h.st.mods.box)
	if err != nil || s.AI.Key != "sk-test-abcd1234" || s.Level != store.GuideMid || s.Input != store.InputClarify {
		t.Fatalf("stored: %+v %v", s.Level, err)
	}
	a = callMem(t, hd, "POST", "/settings/narrative", `{"ai_own":false,"ai":{"provider":"anthropic","model":"m"}}`)
	if a.V["ai"].(map[string]any)["key_set"] != false {
		t.Fatalf("another provider kept the key: %s", a.Raw)
	}
}

// One deadline for the whole answer (review 20 B-03): a preprocessing AI and
// a publisher that never answer in time still let the memory go by the
// deadline, as Go chose it, with no guide and the input as written.
func TestInjectSharesOneDeadline(t *testing.T) {
	was := narrDeadline
	narrDeadline = 2 * time.Second
	defer func() { narrDeadline = was }()
	h := hudFixture(t, "", 0)
	srv := narrSeed(t, h, "T")
	off := injectionRequest(t, srv, h, "T")
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		http.Error(w, "late", 500)
	}))
	defer slow.Close()
	ai := provider.Config{Provider: "custom", Model: "slow", URL: slow.URL + "/v1"}
	if err := h.db.SetPreprocessSettings(h.st.mods.box, store.PreprocessSettings{Mode: store.PreprocessAI, AIOwn: true, AI: ai}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetNarrativeSettings(h.st.mods.box, store.NarrativeSettings{Level: store.GuideWeak, Input: store.InputClarify, Chars: 3000, AIOwn: true, AI: ai}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	a := injectionRequest(t, srv, h, "T")
	took := time.Since(start)
	if took > 4*time.Second {
		t.Fatalf("answer took %v past a 2 s deadline", took)
	}
	tr := lastTrace(t, h, "T")
	if a.V["block"] != off.V["block"] || a.V["guide"] != nil || a.V["input_rewrite"] != nil || tr.Summary.Preprocess.Ran != "go" {
		t.Fatalf("late answers changed the delivery: %v %v %+v", a.V["guide"], a.V["input_rewrite"], tr.Summary.Preprocess)
	}
	if r := tr.Summary.Narrative; r.Guide == "made" || r.InputResult == "changed" {
		t.Fatalf("narrative: %+v", r)
	}
}

// The options the preprocessing tab does not show come back in the view and
// stay when the view is sent back (review 20 A-02).
func TestNarrativeSettingsKeepOptions(t *testing.T) {
	h := hudFixture(t, "", 0)
	for _, path := range []string{"/settings/narrative", "/settings/preprocess"} {
		hd := narrativeHandler(h.st.mods)
		if path == "/settings/preprocess" {
			hd = preprocessHandler(h.st.mods)
		}
		set := `{"ai_own":true,"ai":{"provider":"openai","model":"m","key":"sk-x-1234","timeout_ms":12345,"temperature":0,"max_tokens":4567,"extra_headers_json":"{\"X\":\"1\"}","extra_body_json":"{\"top_p\":1}","service_tier":"flex","claude_prompt_cache":"ephemeral_5m"}}`
		if a := callMem(t, hd, "POST", path, set); a.Code != 200 {
			t.Fatalf("%s: %s", path, a.Raw)
		}
		v := callMem(t, hd, "GET", path, "").V["ai"].(map[string]any)
		back, _ := json.Marshal(map[string]any{"ai_own": true, "ai": v})
		if a := callMem(t, hd, "POST", path, string(back)); a.Code != 200 {
			t.Fatalf("%s echo: %s", path, a.Raw)
		}
		view := callMem(t, hd, "GET", path, "").V
		got := view["ai"].(map[string]any)
		// the screen shows these options beside the critic's (user 11-08), with
		// the tier providers and the whole call's bound from here
		bound := map[string]float64{"/settings/narrative": narrTimeout.Seconds(), "/settings/preprocess": prepTimeout.Seconds()}[path]
		if view["deadline_s"] != bound || fmt.Sprint(view["service_tier_providers"]) != fmt.Sprint(tierProviders()) {
			t.Fatalf("%s view: deadline %v tiers %v", path, view["deadline_s"], view["service_tier_providers"])
		}
		for k, want := range map[string]any{"timeout_ms": float64(12345), "temperature": float64(0), "max_tokens": float64(4567), "extra_headers_json": `{"X":"1"}`, "extra_body_json": `{"top_p":1}`, "service_tier": "flex", "claude_prompt_cache": "ephemeral_5m", "key_set": true} {
			if got[k] != want {
				t.Fatalf("%s %s: %v, want %v", path, k, got[k], want)
			}
		}
	}
}

// User 11-09: the publisher prompt is editable like the critic's. The saved
// rules replace the default head of the real guide request; the level's items
// and the output shape stay the code's; a reset goes back to the default.
func TestPublisherPromptEditable(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := narrSeed(t, h, "P")
	f := newNarrFake(t)
	f.guide = `{"connections":[{"text":"리나의 약속이 아직 남았다","refs":["L1"]}],"reactions":[{"who":"리나","text":"약속을 떠올린다","refs":["L1"]}]}`
	ed := publisherPromptHandler(h.db)
	code, v := u5Call(t, ed, "GET", "")
	if code != 200 || v["content"] != narrGuideHead || v["custom"] != false {
		t.Fatalf("default view: %d %v", code, v)
	}
	custom := "너는 느와르 서사 도우미다. 긴장을 놓치지 않게 짧게 쓴다.\n- 근거를 refs에 적는다."
	b, _ := json.Marshal(map[string]any{"content": custom})
	if code, v = u5Call(t, ed, "POST", string(b)); code != 200 || v["custom"] != true || v["content"] != custom {
		t.Fatalf("save: %d %v", code, v)
	}
	f.head = custom
	narrSet(t, h, f, store.GuideMid, store.InputOff, store.DefaultGuideChars)
	got := injectionRequest(t, srv, h, "P")
	prompts := f.take()
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], custom+"\n") || strings.Contains(prompts[0], "너는 롤플레이의 다음 응답을 돕는 서사 편집자다.") ||
		!strings.Contains(prompts[0], narrFieldText["connections"]) || !strings.Contains(prompts[0], narrFieldText["reactions"]) || strings.Contains(prompts[0], "beats") {
		t.Fatalf("custom guide prompt: %q", prompts)
	}
	if after, _ := guideOf(got); !strings.Contains(after, "리나: 약속을 떠올린다") {
		t.Fatalf("guide from the custom prompt: %q", after)
	}
	// the critic's prompt is its own
	if c, _ := h.db.CriticPrompt(); c != "" {
		t.Fatalf("publisher prompt leaked into the critic's: %q", c)
	}
	if code, v = u5Call(t, ed, "POST", `{"reset":true}`); code != 200 || v["content"] != narrGuideHead || v["custom"] != false {
		t.Fatalf("reset: %d %v", code, v)
	}
	f.head = ""
	injectionRequest(t, srv, h, "P")
	if prompts = f.take(); len(prompts) != 1 || !strings.HasPrefix(prompts[0], narrGuideHead) {
		t.Fatalf("after reset: %q", prompts)
	}
}
