package main

import (
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/store"
)

// RisuAI's /cut or /del takes the front of the chat away (its text and
// memory must stay). Through the production pipeline and /memory/inject: the
// memory of the turn cut away is still delivered, the next input is a new
// turn, and a later screen delete of the last turn still takes that turn's
// memory out.
func TestFrontCutKeepsMemory(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, rows := injectionSeed(t, h, "C")
	injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"약속","mode":"상태","body":"리나는 해 지기 전에 돌아오겠다고 약속했다"}]}`)
	injectionSave(t, h.db, scope[1].Source, `{"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"일","mode":"상태","body":"리나는 약국에서 일한다"}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	has := func(a memAnswer, s string) bool { return strings.Contains(fmt.Sprint(a.V["block"]), s) }
	if a := injectionRequest(t, srv, h, "C"); !has(a, "약속했다") {
		t.Fatalf("fixture: %v", a.V["block"])
	}
	turnsBefore := len(scope)
	// /del: the first turn's rows leave the chat
	if _, err := h.pipe.Process([]observe.Record{memEvent(2, "beforeRequest", "C", rows[2:]...)}); err != nil {
		t.Fatal(err)
	}
	a := injectionRequest(t, srv, h, "C")
	if !has(a, "약속했다") {
		t.Fatalf("the memory of the turn cut away is gone: %v", a.V["block"])
	}
	active := 0
	h.pipe.WithSession("chat:C", func(s *lifecycle.Session) { active = len(s.ActiveTurns()) })
	if active != turnsBefore {
		t.Fatalf("active turns %d, want %d", active, turnsBefore)
	}
	// a later screen delete of the last answered turn (and the input after
	// it) still removes that turn's memory
	if _, err := h.pipe.Process([]observe.Record{memEvent(3, "beforeRequest", "C", rows[2:4]...)}); err != nil {
		t.Fatal(err)
	}
	h.pipe.WithSession("chat:C", func(s *lifecycle.Session) { active = len(s.ActiveTurns()) })
	if active != 2 {
		t.Fatalf("after the tail delete: %d active turns, want 2", active)
	}
}
