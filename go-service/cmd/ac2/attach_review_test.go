package main

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Regressions on C22. The model is a local recording
// boundary: no HTTP, model or embedding call.
type pastRecorder struct {
	mu      sync.Mutex
	prompts []string
}

func (r *pastRecorder) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
	return provider.Result{Content: `{"elapsed_days":0,"memories":[{"id":"m1","kind":"fact","body":"새로 만든 기록"}]}`}, nil
}

func (r *pastRecorder) known(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.prompts) != 1 {
		t.Fatalf("model called %d times", len(r.prompts))
	}
	p := r.prompts[0]
	k := p[strings.LastIndex(p, "\nKNOWN:"):]
	return k[:strings.Index(k, "\nTHIS TURN (")]
}

func pastScope(t *testing.T, p *pipeline.Pipeline, key string) []memory.ScopedTurn {
	t.Helper()
	var scope []memory.ScopedTurn
	if err := p.WithSession(key, func(s *lifecycle.Session) { scope = memory.ScopeOf(s) }); err != nil {
		t.Fatal(err)
	}
	return scope
}

// runUntilMade runs the production worker until the chat's turn is made.
func runUntilMade(t *testing.T, db *store.DB, worker *generate.Worker, src memory.Source) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); worker.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	u3Wait(t, func() bool { j, e := db.JobFor(src); return e == nil && j.State == "done" })
}

// R01: every way a turn is made (the next input, cold start, remake,
// clear, remaking several) reads the attached past.
func TestAttachedPastAtEveryGenerationEntry(t *testing.T) {
	for _, mode := range []string{"next-input", "backfill", "remake", "clear", "bulk-remake"} {
		t.Run(mode, func(t *testing.T) {
			db := modelsDB(t)
			p := pipeline.New(db)
			y := u2LoadHTTP(t, p, "Y", 1)
			u2GenerateHTTP(t, db, y) // Y: "문을 열었다"
			if _, err := p.Process([]observe.Record{attEvent(2, "load", "X", memRow(0, "user", "xu", "", "문을 열었던 기록"), memRow(1, "char", "xa", "", "문을 열었다는 새 이야기"))}); err != nil {
				t.Fatal(err)
			}
			attCall(t, attachHandler(p, db, nil), "POST", "/chats/attach", `{"chat_id":"X","chats":["Y"]}`, 200)
			m := &pastRecorder{}
			worker := generate.NewWorker(db, m, "recording", time.Second)
			switch mode {
			case "backfill":
				if a := callMem(t, backfillHandler(p, worker, db), "POST", "/memory/backfill", `{"chat_id":"X"}`); a.Code != 200 {
					t.Fatal(a.Raw)
				}
			case "remake", "clear":
				if a := callMem(t, turnsHandler(p, db, worker, nil), "POST", "/memory/turn/"+mode, `{"chat_id":"X","turn":1}`); a.Code != 200 {
					t.Fatal(a.Raw)
				}
			case "bulk-remake":
				if _, err := remakeTurns(p, db, worker, "X", []int{1}); err != nil {
					t.Fatal(err)
				}
			default:
				p.WithSession("chat:X", func(s *lifecycle.Session) {
					s.ActiveTurns()[0].Confirmed = true
					if err := db.SaveSession(s, nil, nil); err != nil {
						t.Fatal(err)
					}
					worker.Notify(s.Key, memory.WithPast(s))
				})
			}
			runUntilMade(t, db, worker, pastScope(t, p, "chat:X")[1].OwnSource())
			if k := m.known(t); !strings.Contains(k, "문을 열었다") {
				t.Fatalf("%s made the turn without the attached past:%s", mode, k)
			}
		})
	}
}

// R02: generation reads the attached past as earlier than the chat's own
// turns: the chat's own scene is the current one, not the past's later
// local turn number.
func TestAttachedPastIsEarlierInGeneration(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	if _, err := p.Process([]observe.Record{attEvent(1, "load", "Y", memRow(0, "user", "y1", "", "먼 옛날"), memRow(1, "char", "ya1", "", "먼 기록"), memRow(2, "user", "y2", "", "그 뒤"), memRow(3, "char", "ya2", "", "옛 장면"))}); err != nil {
		t.Fatal(err)
	}
	var ys *lifecycle.Session
	p.WithSession("chat:Y", func(s *lifecycle.Session) { ys = s })
	u3Save(t, db, memory.Own(ys)[1].Source, `{"elapsed_days":0,"memories":[{"id":"m1","kind":"event","scene":"s1","body":"과거 무도회"}],"scenes":[{"id":"s1","change":"open"}]}`)
	if _, err := p.Process([]observe.Record{attEvent(2, "load", "X", memRow(0, "user", "x1", "", "도서관에 왔다"), memRow(1, "char", "xa1", "", "도서관 장면"), memRow(2, "user", "x2", "", "계속"), memRow(3, "char", "xa2", "", "도서관에서 무도회를 떠올린 이야기"))}); err != nil {
		t.Fatal(err)
	}
	var xs *lifecycle.Session
	p.WithSession("chat:X", func(s *lifecycle.Session) { xs = s })
	u3Save(t, db, memory.Own(xs)[0].Source, `{"elapsed_days":0,"memories":[{"id":"m1","kind":"event","scene":"s1","body":"현재 도서관"}],"scenes":[{"id":"s1","change":"open"}]}`)
	attCall(t, attachHandler(p, db, nil), "POST", "/chats/attach", `{"chat_id":"X","chats":["Y"]}`, 200)
	m := &pastRecorder{}
	worker := generate.NewWorker(db, m, "recording", time.Second)
	p.WithSession("chat:X", func(s *lifecycle.Session) {
		s.ActiveTurns()[1].Confirmed = true
		if err := db.SaveSession(s, nil, nil); err != nil {
			t.Fatal(err)
		}
		worker.Notify(s.Key, memory.WithPast(s))
	})
	scope := pastScope(t, p, "chat:X")
	runUntilMade(t, db, worker, scope[len(scope)-1].OwnSource())
	k := m.known(t)
	if !strings.Contains(k, "현재 도서관") {
		t.Fatalf("the chat's own current scene is not read as the latest:%s", k)
	}
	m.mu.Lock()
	prompt := m.prompts[0]
	m.mu.Unlock()
	// this chat's second turn comes after the past's two turns
	if !strings.Contains(prompt, "THIS TURN (T4)") {
		t.Fatalf("the turn is not numbered in story order:\n%s", prompt[strings.LastIndex(prompt, "\nKNOWN:"):])
	}
}

// R03: a message of the request that happens to have the past's text does
// not make the past a turn the request holds.
func TestAttachedPastIsNotRequestHistory(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	y := u2LoadHTTP(t, p, "Y", 1)
	u2GenerateHTTP(t, db, y)
	if _, err := p.Process([]observe.Record{attEvent(2, "load", "X", memRow(0, "user", "xu", "", "자기 질문"), memRow(1, "char", "xa", "", "자기 답변"))}); err != nil {
		t.Fatal(err)
	}
	attCall(t, attachHandler(p, db, nil), "POST", "/chats/attach", `{"chat_id":"X","chats":["Y"]}`, 200)
	fp := func(s, role string) store.TextPrint {
		n, h := ingest.Fingerprint(s)
		return store.TextPrint{Len: n, H: h, Role: role}
	}
	req := store.DeliveryRequest{Input: "열린 문을 떠올린다", Chars: 2000, Secret: 4000}
	// a system message with Y's reply text ("응답") before the chat's own turn
	req.Payload = []store.TextPrint{fp("응답", "system"), fp("자기 질문", "user"), fp("자기 답변", "assistant"), fp(req.Input, "user")}
	d, err := db.Deliver(pastScope(t, p, "chat:X"), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 1 || len(d.IDs) == 0 {
		t.Fatalf("the past was taken as held by the request: window %d (%s), ids %v", d.Window, d.WindowFrom, d.IDs)
	}
}

// R05: deleting a chat finds a chat that reads it through another chat's
// past; only the deleted chat's part goes, the rest of that past stays.
func TestDeletingAChatReadThroughAnotherPast(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	y := u2LoadHTTP(t, p, "Y", 1)
	u2GenerateHTTP(t, db, y)
	x := u2LoadHTTP(t, p, "X", 2)
	u2GenerateHTTP(t, db, x)
	h := attachHandler(p, db, nil)
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["Y"]}`, 200)
	if got := attCall(t, h, "POST", "/chats/attach", `{"chat_id":"Z","chats":["X"]}`, 200); got.Turns != 2 || got.Memories != 4 {
		t.Fatalf("Z reads Y through X: %+v", got)
	}
	ops := chatOpsHandler(p, db, nil, nil)
	preview := u2Post(t, ops, "/chats/delete", `{"chat_id":"Y","dry_run":true}`)
	if strings.Join(preview.PastReaders, ",") != "X,Z" {
		t.Fatalf("readers %v", preview.PastReaders)
	}
	u2Post(t, ops, "/chats/delete", `{"chat_id":"Y"}`)
	if got := attCall(t, h, "GET", "/chats/attach?chat_id=Z", "", 200); !reflect.DeepEqual(got, attOne("X", 1, 2, false)) {
		t.Fatalf("Z after deleting Y: %+v", got)
	}
	for _, turn := range pastScope(t, p, "chat:Z") {
		if turn.Source.Session == "chat:Y" {
			t.Fatal("Z still reads the deleted chat")
		}
	}
	if got := attCall(t, h, "GET", "/chats/attach?chat_id=X", "", 200); len(got.Chats) != 0 || got.Turns != 0 {
		t.Fatalf("X after deleting Y: %+v", got)
	}
}
