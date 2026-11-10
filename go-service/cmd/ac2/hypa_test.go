package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// hypaModel is the local recording critic: each call answers one memory
// that quotes the summary it was given.
type hypaModel struct {
	mu      sync.Mutex
	prompts []string
}

func (m *hypaModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	m.mu.Lock()
	m.prompts = append(m.prompts, prompt)
	n := len(m.prompts)
	m.mu.Unlock()
	return provider.Result{Content: fmt.Sprintf(`{"elapsed_days":0,"memories":[{"id":"m1","kind":"event","body":"요약에서 만든 기억 %d"}]}`, n)}, nil
}

func (m *hypaModel) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.prompts...)
}

// E12 (user 11-07: every summary, duplicates or not, when the button is
// pressed): the summaries become the turns of their own stored chat, made
// by the cold start and attached before the chat's own turns, ahead of a
// chat attached already; importing again makes only the summaries not made.
func TestHypaImport(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	if _, err := p.Process([]observe.Record{attEvent(1, "load", "Y", memRow(0, "user", "y1", "", "다른 채팅"), memRow(1, "char", "ya1", "", "다른 이야기"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process([]observe.Record{attEvent(2, "load", "X", memRow(0, "user", "x1", "", "지금 채팅"), memRow(1, "char", "xa1", "", "지금 이야기"))}); err != nil {
		t.Fatal(err)
	}
	attCall(t, attachHandler(p, db, nil), "POST", "/chats/attach", `{"chat_id":"X","chats":["Y"]}`, 200)
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.After = p
	m := &hypaModel{}
	worker := generate.NewWorker(db, m, "recording", time.Second)
	h := hypaHandler(p, worker, db, srv)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); worker.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	made := func(n int) {
		t.Helper()
		u3Wait(t, func() bool { k, err := db.DoneKeys("chat:hypa:X"); return err == nil && len(k) == n })
	}
	post := func(body string) memAnswer {
		t.Helper()
		return callMem(t, h, "POST", "/memory/hypa", body)
	}

	// refused: nothing to import, too many, the summaries' own chat
	for _, c := range []struct{ body, code string }{
		{`{"chat_id":"X","summaries":[{"text":"  "}]}`, "hypa_empty"},
		{`{"chat_id":"hypa:X","summaries":[{"text":"a"}]}`, "bad_request"},
		{`{"summaries":[{"text":"a"}]}`, "bad_request"},
	} {
		if a := post(c.body); a.Code != 400 || a.V["code"] != c.code {
			t.Fatalf("%s: %d %s", c.body, a.Code, a.Raw)
		}
	}
	var many []string
	for i := 0; i <= hypaMax; i++ {
		many = append(many, fmt.Sprintf(`{"text":"요약 %d"}`, i))
	}
	if a := post(`{"chat_id":"X","summaries":[` + strings.Join(many, ",") + `]}`); a.Code != 400 || a.V["code"] != "hypa_too_many" {
		t.Fatalf("501 summaries: %d %s", a.Code, a.Raw)
	}

	// a repeated text is imported twice (no condition, user 11-07)
	first := `{"chat_id":"X","chat_name":"모험","summaries":[{"text":"리나가 용을 만났다","important":true,"tags":["용"," "],"category":"사건"},{"text":"  "},{"text":"둘은 성으로 갔다"},{"text":"리나가 용을 만났다"}]`
	if a := post(first + `,"dry_run":true}`); a.Code != 200 || a.V["summaries"].(float64) != 3 || a.V["to_make"].(float64) != 3 {
		t.Fatalf("dry run %d %s", a.Code, a.Raw)
	}
	if _, ok, _ := db.LoadSession("chat:hypa:X"); ok || len(m.calls()) != 0 {
		t.Fatal("the dry run wrote or called")
	}
	a := post(first + `}`)
	if a.Code != 200 || len(a.V["turns"].([]any)) != 3 {
		t.Fatalf("import %d %s", a.Code, a.Raw)
	}
	made(3)
	calls := m.calls()
	if len(calls) != 3 || !strings.Contains(calls[0], "리나가 용을 만났다") || !strings.Contains(calls[0], "중요") || !strings.Contains(calls[0], "태그: 용") || !strings.Contains(calls[1], "둘은 성으로 갔다") {
		t.Fatalf("%d calls; first:\n%s", len(calls), calls[0][strings.LastIndex(calls[0], "THIS TURN"):])
	}
	// attached first, the chat attached already after it, then X's own
	order := func() []string {
		var from []string
		p.WithSession("chat:X", func(s *lifecycle.Session) {
			for _, st := range memory.ScopeOf(s) {
				from = append(from, st.Source.Session)
			}
		})
		return from
	}
	if got := strings.Join(order(), ","); got != "chat:hypa:X,chat:hypa:X,chat:hypa:X,chat:Y,chat:X" {
		t.Fatalf("read order %s", got)
	}
	att := attCall(t, attachHandler(p, db, nil), "GET", "/chats/attach?chat_id=X", "", 200)
	if b, _ := json.Marshal(att); !strings.Contains(string(b), `"chat_id":"hypa:X","turns":3,"memories":3`) {
		t.Fatalf("attached %s", b)
	}

	// again with one summary changed and one added: only those two are made,
	// the attachment holds the new set once
	second := `{"chat_id":"X","summaries":[{"text":"리나가 용을 만났다","important":true,"tags":["용"],"category":"사건"},{"text":"둘은 성으로 갔다"},{"text":"리나가 용과 싸웠다"},{"text":"성문이 닫혔다"}]`
	if a := post(second + `,"dry_run":true}`); a.V["to_make"].(float64) != 2 {
		t.Fatalf("second dry run %s", a.Raw)
	}
	if a := post(second + `}`); a.Code != 200 || len(a.V["turns"].([]any)) != 2 {
		t.Fatalf("second import %d %s", a.Code, a.Raw)
	}
	u3Wait(t, func() bool { return len(m.calls()) == 5 })
	if got := strings.Join(order(), ","); got != "chat:hypa:X,chat:hypa:X,chat:hypa:X,chat:hypa:X,chat:Y,chat:X" {
		t.Fatalf("read order after the second import %s", got)
	}
}

// Review 15: summaries imported again in a new order are read in that
// order, the memories kept (#1); a summary whose marks alone changed is
// counted as one to make, as the cold start makes it again (#3).
func TestHypaImportReview15(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	if _, err := p.Process([]observe.Record{attEvent(1, "load", "X", memRow(0, "user", "x1", "", "지금 채팅"), memRow(1, "char", "xa1", "", "지금 이야기"))}); err != nil {
		t.Fatal(err)
	}
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.After = p
	m := &hypaModel{}
	worker := generate.NewWorker(db, m, "recording", time.Second)
	h := hypaHandler(p, worker, db, srv)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); worker.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	post := func(body string) memAnswer {
		t.Helper()
		a := callMem(t, h, "POST", "/memory/hypa", body)
		if a.Code != 200 {
			t.Fatalf("%s: %d %s", body, a.Code, a.Raw)
		}
		return a
	}
	read := func() string {
		var texts []string
		p.WithSession("chat:X", func(s *lifecycle.Session) {
			for _, st := range memory.ScopeOf(s) {
				for _, r := range st.Source.Responses {
					texts = append(texts, r.Data)
				}
			}
		})
		return strings.Join(texts, "|")
	}
	post(`{"chat_id":"X","summaries":[{"text":"첫 번째 사건"},{"text":"두 번째 사건"}]}`)
	u3Wait(t, func() bool { return len(m.calls()) == 2 })
	post(`{"chat_id":"X","summaries":[{"text":"가장 이른 사건"},{"text":"두 번째 사건"},{"text":"첫 번째 사건"}]}`)
	u3Wait(t, func() bool { return len(m.calls()) == 3 })
	if got := read(); got != "가장 이른 사건|두 번째 사건|첫 번째 사건|지금 이야기" {
		t.Fatalf("read order %s", got)
	}
	// a new pipeline (a restart) reads the same order
	p2 := pipeline.New(db)
	var again []string
	p2.WithSession("chat:X", func(s *lifecycle.Session) {
		for _, st := range memory.ScopeOf(s) {
			for _, r := range st.Source.Responses {
				again = append(again, r.Data)
			}
		}
	})
	if strings.Join(again, "|") != "가장 이른 사건|두 번째 사건|첫 번째 사건|지금 이야기" {
		t.Fatalf("after a restart %v", again)
	}
	// marks only: one to make, and it is made
	a := post(`{"chat_id":"X","dry_run":true,"summaries":[{"text":"가장 이른 사건","important":true,"tags":["용"]},{"text":"두 번째 사건"},{"text":"첫 번째 사건"}]}`)
	if a.V["to_make"].(float64) != 1 {
		t.Fatalf("marks changed: to_make %v", a.V["to_make"])
	}
	a = post(`{"chat_id":"X","summaries":[{"text":"가장 이른 사건","important":true,"tags":["용"]},{"text":"두 번째 사건"},{"text":"첫 번째 사건"}]}`)
	if len(a.V["turns"].([]any)) != 1 || a.V["to_make"].(float64) != 1 {
		t.Fatalf("marks changed: %s", a.Raw)
	}
	// the call starts before its result is saved (review 16): wait for the
	// made turn, as the preview counts it, not for the call
	preview := `{"chat_id":"X","dry_run":true,"summaries":[{"text":"가장 이른 사건","important":true,"tags":["용"]},{"text":"두 번째 사건"},{"text":"첫 번째 사건"}]}`
	u3Wait(t, func() bool { return len(m.calls()) == 4 && post(preview).V["to_make"].(float64) == 0 })
	if len(m.calls()) != 4 {
		t.Fatalf("made again more than once: %d calls", len(m.calls()))
	}
}
