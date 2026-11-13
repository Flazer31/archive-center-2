package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Regressions, turned from their reproductions into the behavior they
// should have.

func r12Load(t *testing.T, p *pipeline.Pipeline, chat string, seq int) *lifecycle.Session {
	t.Helper()
	if _, err := p.Process([]observe.Record{attEvent(seq, "load", chat,
		memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", "리나가 정원에 왔다"),
		memRow(2, "user", "u2", "", "며칠 뒤"), memRow(3, "char", "a2", "g2", "리나가 정원에 다시 왔다"))}); err != nil {
		t.Fatal(err)
	}
	var s *lifecycle.Session
	p.WithSession("chat:"+chat, func(v *lifecycle.Session) { s = v })
	return s
}

func r12Day(n int) body.Day { return body.Day{Axis: "day", N: n} }

// R01: a move carries the facts recorded on the body screen and the
// person's settings, with the destination's person id.
func TestBodyMoveKeepsRecordedFactsAndSettings(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	x := r12Load(t, p, "X", 1)
	r12Load(t, p, "Y", 2)
	own := memory.Own(x)
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 정원에 왔다"}],"body_events":[{"kind":"period_start","who":"n1"}]}`)
	db.SetBodyTracking(body.Toggles{Cycle: true})
	before, err := db.BodyStates(x.Key, own)
	if err != nil || len(before) != 1 {
		t.Fatal(err, before)
	}
	spec := body.DefaultSpec()
	spec.CycleDays = 35
	db.SetBodyPerson(x.Key, body.Person{Who: before[0].Who, Spec: spec})
	if _, err := db.RecordBodyFact(x.Key, before[0].Who, "recovery_started", "Day 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ChatOperation("move", "chat:X", "chat:Y", false); err != nil {
		t.Fatal(err)
	}
	y, _, _ := db.LoadSession("chat:Y")
	after, err := db.BodyStates(y.Key, memory.Own(y))
	if err != nil || len(after) != 1 || after[0].Who == before[0].Who {
		t.Fatalf("after %+v %v", after, err)
	}
	kinds := []string{}
	for _, f := range after[0].Facts {
		kinds = append(kinds, fmt.Sprintf("%s %v", f.Kind, f.User))
	}
	person, _ := db.BodyPerson(y.Key, after[0].Who)
	if strings.Join(kinds, ",") != "period_start false,recovery_started true" || person.Spec.CycleDays != 35 {
		t.Fatalf("moved facts %v settings %+v", kinds, person.Spec)
	}
}

// R02: reading another chat's events as a past never writes that chat's
// rows: its own read decides with its own settings.
func TestBodyPastReaderLeavesSourceResults(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	a := r12Load(t, p, "A", 1)
	own := memory.Own(a)
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 정원에 왔다"}],"body_events":[{"kind":"period_start","who":"n1"}]}`)
	k, _ := db.CurrentKnown(a)
	who := k.Names[0].ID
	u3Save(t, db, own[1].Source, `{"elapsed_days":10,"memories":[{"id":"m1","kind":"event","body":"리나가 정원에 다시 왔다"}],"body_events":[{"kind":"conception_exposure","who":"`+who+`","exposure":{"classification":"potentially_conceiving","partner_compatibility":"compatible","contraception":"none"}}]}`)
	db.SetBodyTracking(body.Toggles{Pregnancy: true})
	attCall(t, attachHandler(p, db, nil), "POST", "/chats/attach", `{"chat_id":"X","chats":["A"]}`, 200)
	var x *lifecycle.Session
	p.WithSession("chat:X", func(v *lifecycle.Session) { x = v })
	spec := body.DefaultSpec()
	spec.CanConceive = false
	db.SetBodyPerson(x.Key, body.Person{Who: who, Spec: spec})
	if _, err := db.BodyStates(x.Key, memory.ScopeOf(x)); err != nil {
		t.Fatal(err)
	}
	states, err := db.BodyStates(a.Key, memory.ScopeOf(a))
	if err != nil || len(states) != 1 {
		t.Fatal(err, states)
	}
	if got := states[0].Facts[1].Result; got == nil || got.Status != "evaluated" {
		t.Fatalf("the source's own result: %+v", got)
	}
}

// R04: a complete body event before a cut-off one is kept.
func TestBodyEventsSalvagedFromCutAnswer(t *testing.T) {
	out, notes := memory.Parse(`{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 정원에 왔다"}],"body_events":[{"kind":"period_start","who":"n1"},{"kind":"recovery_started","who":"n1"`)
	if len(out.Memories) != 1 || len(out.BodyEvents) != 1 || out.BodyEvents[0].Kind != "period_start" {
		t.Fatalf("%+v %v", out.BodyEvents, notes)
	}
	// a complete answer reads each event once
	out, _ = memory.Parse(`{"body_events":[{"kind":"period_start","who":"n1"}]}`)
	if len(out.BodyEvents) != 1 {
		t.Fatalf("complete: %+v", out.BodyEvents)
	}
}

// R05 R06 R07 R08 R14: the body read keeps a fact's visibility, keeps
// story facts whatever their date axis, ends the cycle after a modeled
// term, keeps an established parent, and counts its header in its budget.
func TestBodyStateReviewRules(t *testing.T) {
	p := body.Person{Who: "N1", Spec: body.DefaultSpec()}
	on := body.Toggles{Cycle: true, Pregnancy: true}
	name := func(s string) string { return s }
	s, _, _ := body.Assess(body.Toggles{Cycle: true}, "seed", p, []body.Record{{ID: 1, Kind: "recovery_started", Visibility: "private", Day: r12Day(1)}}, r12Day(2))
	if lines := body.Lines([]body.State{s}, name); !strings.Contains(lines[0], "[story fact, private]: recovering") {
		t.Fatalf("R05 %v", lines)
	}
	s, _, _ = body.Assess(on, "seed", p, []body.Record{{ID: 1, Kind: "pregnancy_confirmed"}}, r12Day(10))
	if s.Pregnancy == nil || s.Cycle != nil {
		t.Fatalf("R06 undated: %+v", s)
	}
	s, _, _ = body.Assess(on, "seed", p, []body.Record{{ID: 1, Kind: "pregnancy_confirmed", Day: body.Day{Axis: "calendar", N: 20455}}}, r12Day(10))
	if s.Pregnancy == nil || s.Cycle != nil {
		t.Fatalf("R06 other axis: %+v", s)
	}
	x := body.Exposure{Version: body.ModelVersion, Status: "evaluated", Implanted: true, ExposureDay: 14, OvulationDay: 14, ImplantationDay: 22, BirthDay: 280}
	recs := []body.Record{{ID: 1, Kind: "period_start", Day: r12Day(0)}, {ID: 2, Kind: "conception_exposure", Day: r12Day(14), Result: &x}}
	if s, _, _ = body.Assess(on, "seed", p, recs, r12Day(281)); s.Cycle != nil {
		t.Fatalf("R07 after the modeled term, before a period: %+v", s)
	}
	recs = append(recs, body.Record{ID: 3, Kind: "period_start", Day: r12Day(300)})
	if s, _, _ = body.Assess(on, "seed", p, recs, r12Day(305)); s.Cycle == nil || s.CycleRef != r12Day(300) {
		t.Fatalf("R07 a period after it: %+v", s)
	}
	recs = []body.Record{{ID: 1, Kind: "pregnancy_confirmed", Day: r12Day(10), Paternity: "confirmed", Candidates: []string{"N2"}}, {ID: 2, Kind: "pregnancy_confirmed", Day: r12Day(20)}}
	if s, _, _ = body.Assess(on, "seed", p, recs, r12Day(21)); s.Pregnancy.Paternity != "confirmed" || s.Pregnancy.Since != r12Day(10) {
		t.Fatalf("R08 %+v", s.Pregnancy)
	}
	recs = append(recs, body.Record{ID: 3, Kind: "pregnancy_confirmed", Day: r12Day(30), Paternity: "unknown"})
	if s, _, _ = body.Assess(on, "seed", p, recs, r12Day(31)); s.Pregnancy.Paternity != "unknown" {
		t.Fatalf("R08 a stated unknown replaces: %+v", s.Pregnancy)
	}
	var many []body.State
	for i := 0; i < 200; i++ {
		many = append(many, body.State{Who: fmt.Sprintf("N%d", i), Recovery: true})
	}
	db := modelsDB(t)
	d, err := db.Deliver(nil, store.DeliveryRequest{Body: body.Lines(many, name)})
	if err != nil || d.BodyChars > body.Budget || d.BodyChars < body.Budget-60 {
		t.Fatalf("R14 body %d of %d: %v", d.BodyChars, body.Budget, err)
	}
}

// R11: a frozen result keeps its days relative to its exposure when a past
// moves the story's day numbers.
func TestBodyFrozenDaysFollowTheStory(t *testing.T) {
	p := body.Person{Who: "N1", Spec: body.DefaultSpec()}
	x := body.Exposure{Version: body.ModelVersion, Status: "evaluated", Implanted: true, ExposureDay: 14, OvulationDay: 14, ImplantationDay: 22, BirthDay: 280}
	recs := []body.Record{{ID: 1, Kind: "period_start", Day: r12Day(100)}, {ID: 2, Kind: "conception_exposure", Day: r12Day(114), Result: &x}}
	if s, _, _ := body.Assess(body.Toggles{Pregnancy: true}, "seed", p, recs, r12Day(114)); s.Pregnancy != nil {
		t.Fatalf("pregnant before implantation: %+v", s.Pregnancy)
	}
	if s, _, _ := body.Assess(body.Toggles{Pregnancy: true}, "seed", p, recs, r12Day(122)); s.Pregnancy == nil || s.Pregnancy.Since != r12Day(122) || s.Pregnancy.BirthDay != r12Day(380) {
		t.Fatalf("shifted: %+v", s.Pregnancy)
	}
}

// R12: the memory screen's dates are the dates injection reads, also with
// a turn filter and with an attached past.
func TestMemoryListDatesMatchInjection(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	a := r12Load(t, p, "A", 1)
	x := r12Load(t, p, "X", 2)
	for _, s := range []*lifecycle.Session{a, x} {
		own := memory.Own(s)
		u3Save(t, db, own[0].Source, `{"elapsed_days":0,"memories":[{"id":"m1","kind":"event","body":"정원을 보았다"}]}`)
		u3Save(t, db, own[1].Source, `{"elapsed_days":7,"memories":[{"id":"m1","kind":"event","body":"정원을 다시 보았다"}]}`)
	}
	h := memoriesHandler(p, db, nil, nil)
	check := func(path string, want int) {
		t.Helper()
		var s *lifecycle.Session
		p.WithSession("chat:X", func(v *lifecycle.Session) { s = v })
		k, _ := db.CurrentKnown(s)
		dates := map[string]string{}
		for _, m := range k.Memories {
			dates[m.ID] = m.Date
		}
		got := listed(t, callMem(t, h, "GET", path, ""))
		if len(got) != want {
			t.Fatalf("%s: %d memories", path, len(got))
		}
		for _, m := range got {
			if m["date"] != dates[m["id"].(string)] {
				t.Fatalf("%s: %s list %v injection %q", path, m["id"], m["date"], dates[m["id"].(string)])
			}
		}
	}
	check("/memory/list?chat_id=X", 2)
	check("/memory/list?chat_id=X&turn=2", 1)
	attCall(t, attachHandler(p, db, nil), "POST", "/chats/attach", `{"chat_id":"X","chats":["A"]}`, 200)
	check("/memory/list?chat_id=X", 2)
	check("/memory/list?chat_id=X&turn=2", 1)
}

// R10: remaking the failed turns waits for a generation that is under way:
// a retry the worker saves meanwhile is not undone.
type heldModel struct {
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (h *heldModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	h.mu.Lock()
	if h.entered != nil {
		close(h.entered)
		h.entered = nil
	}
	h.mu.Unlock()
	<-h.release
	return provider.Result{Content: `{"elapsed_days":0,"memories":[{"id":"m1","kind":"fact","body":"다시 만든 기록"}]}`}, nil
}

func TestRemakeFailedWaitsForTheWorker(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	x := r12Load(t, p, "X", 1)
	p.WithSession("chat:X", func(s *lifecycle.Session) {
		for _, turn := range s.ActiveTurns() {
			turn.Confirmed = true
		}
		db.SaveSession(s, nil, nil)
	})
	own := memory.Own(x)
	u3Save(t, db, own[1].Source, `{"elapsed_days":0,"memories":[{"id":"m1","kind":"event","body":"둘째"}]}`)
	j, _ := db.JobFor(own[0].Source)
	db.StartAttempt(j)
	db.FailJob(j, "m", "p", "[]", "provider down", time.Now()) // failed once: the worker retries it
	m := &heldModel{entered: make(chan struct{}), release: make(chan struct{})}
	worker := generate.NewWorker(db, m, "held", time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); worker.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	worker.Notify("chat:X", memory.Own(x))
	<-m.entered // the retry is under way
	done := make(chan []int)
	go func() {
		h := remakeFailedHandler(p, db, worker, nil)
		a := callMem(t, h, "POST", "/memory/turn/remake-failed", `{"chat_id":"X"}`)
		var turns []int
		for _, v := range a.V["turns"].([]any) {
			turns = append(turns, int(v.(float64)))
		}
		done <- turns
	}()
	select {
	case <-done:
		close(m.release)
		t.Fatal("remade while the worker was generating")
	case <-time.After(300 * time.Millisecond):
	}
	close(m.release) // the retry is stored
	turns := <-done
	if len(turns) != 0 {
		t.Fatalf("a turn the worker just made was remade: %v", turns)
	}
	if st, _ := db.SourceJobState(own[0].Source); st != "done" {
		t.Fatalf("job %s", st)
	}
}
