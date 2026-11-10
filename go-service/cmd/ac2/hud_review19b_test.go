package main

import (
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Review 19, second check: the defects Sol reproduced on the first fixes,
// asserted fixed (probe-02 in review 19), with its two passing controls.
func TestHUDReview19OlderRetryIsFollowed(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[]}`}})
	}))
	defer func() { finish(); model.Close() }()
	h := hudFixture(t, model.URL, 0)
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "OLDRETRY", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two")), memEvent(2, "beforeRequest", "OLDRETRY", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"), memRow(3, "char", "a2", "g2", "answer2"), memRow(4, "user", "u3", "", "three"))})
	if err != nil {
		t.Fatal(err)
	}
	var src, second memory.Source
	h.pipe.WithSession("chat:OLDRETRY", func(s *lifecycle.Session) {
		src = memory.SourceOf(s.Key, s.Turns[0])
		second = memory.SourceOf(s.Key, s.Turns[1])
	})
	j, err := h.db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.StartAttempt(j); err != nil {
		t.Fatal(err)
	}
	if err = h.db.FailJob(j, "synthetic", "", "", "provider 503: SYNTHETIC-PRIVATE-BODY", time.Now()); err != nil {
		t.Fatal(err)
	}
	injectionSave(t, h.db, second, `{"memories":[]}`)
	response := callMem(t, h.handler(), "POST", "/hud/retry", `{"chat_id":"OLDRETRY","turn":1}`)
	if response.Code != 200 {
		t.Fatal(response.Raw)
	}
	if fs := response.V["failures"].([]any); len(fs) != 1 || fs[0].(map[string]any)["state"] != "waiting" || fs[0].(map[string]any)["retry"] != false || response.V["active"] != true {
		t.Fatalf("older retry asked: %v / %v", response.V["failures"], response.V["active"])
	}
	// a new observation of the chat keeps the earlier turn's asked retry
	if _, err := h.pipe.Process([]observe.Record{memEvent(3, "beforeRequest", "OLDRETRY", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"), memRow(3, "char", "a2", "g2", "answer2"), memRow(4, "user", "u3", "", "three"))}); err != nil {
		t.Fatal(err)
	}
	if fs := hudGet(t, h, "OLDRETRY")["failures"].([]any); len(fs) != 1 || fs[0].(map[string]any)["state"] != "waiting" {
		t.Fatalf("asked retry after a new observation: %v", fs)
	}
	hudRun(t, h.worker)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stand-in not reached")
	}
	view := hudGet(t, h, "OLDRETRY")
	fs := view["failures"].([]any)
	if len(fs) != 1 || fs[0].(map[string]any)["state"] != "running" || fs[0].(map[string]any)["retry"] != false || view["active"] != true {
		t.Fatalf("older retry running: %v", view)
	}
	finish()
	hudWait(t, func() bool { j, _ := h.db.JobFor(src); return j.State == "done" })
}

func TestHUDReview19OlderFailureBeyondManySkipped(t *testing.T) {
	h := hudFixture(t, "", 0)
	rows := []string{}
	for i := 1; i <= 53; i++ {
		rows = append(rows, memRow(len(rows), "user", fmt.Sprint("u", i), "", fmt.Sprint("input ", i)))
		if i < 53 {
			rows = append(rows, memRow(len(rows), "char", fmt.Sprint("a", i), fmt.Sprint("g", i), fmt.Sprint("answer ", i)))
		}
	}
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "CAP", rows...)})
	if err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	h.pipe.WithSession("chat:CAP", func(s *lifecycle.Session) {
		for _, turn := range s.Turns {
			srcs = append(srcs, memory.SourceOf(s.Key, turn))
		}
	})
	for i := 0; i < 51; i++ {
		j, e := h.db.JobFor(srcs[i])
		if e != nil {
			t.Fatal(e)
		}
		if e = h.db.StartAttempt(j); e != nil {
			t.Fatal(e)
		}
		if e = h.db.FailJob(j, "synthetic", "", "", "provider 503: SYNTHETIC-PRIVATE-BODY", time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	h.pipe.WithSession("chat:CAP", func(s *lifecycle.Session) {
		for i := 1; i < 51; i++ {
			s.Turns[i].Off = true
		}
	})
	view := hudGet(t, h, "CAP")
	fs := view["failures"].([]any)
	current, e := h.db.HUDJob(srcs[0])
	if e != nil || current.State != "failed" {
		t.Fatal(current, e)
	}
	if len(fs) != 1 || fs[0].(map[string]any)["turn"] != float64(1) {
		t.Fatalf("older failure behind 50 OOC failures: %v", fs)
	}
}

func TestHUDReview19RearmedUnsavedFailureIsTold(t *testing.T) {
	h := hudFixture(t, "http://127.0.0.1:1", 0)
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "REARMFAIL", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"))})
	if err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	h.pipe.WithSession("chat:REARMFAIL", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.Turns[0]) })
	j, err := h.db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.StartAttempt(j); err != nil {
		t.Fatal(err)
	}
	if err = h.db.FailJob(j, "synthetic", "", "", "synthetic old failure", time.Now()); err != nil {
		t.Fatal(err)
	}
	response := callMem(t, h.handler(), "POST", "/hud/retry", `{"chat_id":"REARMFAIL","turn":1}`)
	if response.Code != 200 {
		t.Fatal(response.Raw)
	}
	conn, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Exec(`CREATE TRIGGER review19_attempt BEFORE UPDATE ON gen_jobs FOR EACH ROW BEGIN IF NEW.attempts>OLD.attempts THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic attempt write denied'; END IF; END`)
	if err != nil {
		t.Fatal(err)
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.chats["chat:REARMFAIL"].turns[1].state == "failed"
	})
	view := hudGet(t, h, "REARMFAIL")
	row := hudTurnView(t, view, 1)
	gen := hudStageView(t, row, "generation")
	if (gen["state"] != "failed" && gen["state"] != "retry_waiting") || gen["detail"] == nil || row["retry"] != true {
		t.Fatalf("rearmed unsaved failure: %v", view)
	}
}

// Since 11-08 the failure detail is the error text as recorded; the credential
// inside it is masked and another chat's failure is still filtered out.
func TestHUDReview19OlderFailuresFilteredAndShownWhole(t *testing.T) {
	h := hudFixture(t, "", 0)
	rows := []string{}
	for i := 1; i <= 7; i++ {
		rows = append(rows, memRow(len(rows), "user", fmt.Sprint("u", i), "", fmt.Sprint("input ", i)))
		if i < 7 {
			rows = append(rows, memRow(len(rows), "char", fmt.Sprint("a", i), fmt.Sprint("g", i), fmt.Sprint("answer ", i)))
		}
	}
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "FILTER", rows...)})
	if err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	h.pipe.WithSession("chat:FILTER", func(s *lifecycle.Session) {
		for _, turn := range s.Turns {
			srcs = append(srcs, memory.SourceOf(s.Key, turn))
		}
	})
	for i := 0; i < 6; i++ {
		j, e := h.db.JobFor(srcs[i])
		if e != nil {
			t.Fatal(e)
		}
		h.db.StartAttempt(j)
		if e = h.db.FailJob(j, "synthetic", "", "", "provider 503: SYNTHETIC-PRIVATE-BODY sk-synthetic123456", time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	h.pipe.WithSession("chat:FILTER", func(s *lifecycle.Session) {
		s.Turns[1].Off = true
		s.Turns[2].Inactive = true
		s.Turns[3].Members[0].Data = "edited current source"
	})
	other := srcs[0]
	other.Session = "chat:OTHER"
	j, e := h.db.JobFor(other)
	if e != nil {
		t.Fatal(e)
	}
	h.db.StartAttempt(j)
	h.db.FailJob(j, "synthetic", "", "", "other private error", time.Now())
	view := hudGet(t, h, "FILTER")
	fs := view["failures"].([]any)
	if len(fs) != 2 || fs[0].(map[string]any)["turn"] != float64(5) || fs[1].(map[string]any)["turn"] != float64(1) {
		t.Fatalf("filter result %v", fs)
	}
	b, _ := json.Marshal(view)
	if !strings.Contains(string(b), "provider 503: SYNTHETIC-PRIVATE-BODY") {
		t.Fatalf("failure text dropped: %s", b)
	}
	if strings.Contains(string(b), "sk-synthetic") || strings.Contains(string(b), "other private error") {
		t.Fatalf("credential shown or another chat's failure mixed in: %s", b)
	}
}

func TestHUDReview19DoneWinsOverLateFailure(t *testing.T) {
	h := hudFixture(t, "", 0)
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "DONE", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"))})
	if err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	h.pipe.WithSession("chat:DONE", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.Turns[0]) })
	injectionSave(t, h.db, src, `{"memories":[]}`)
	h.progress(generate.Progress{Kind: "generation_finished", Session: src.Session, Turn: src.Turn, Source: src.Key(), Err: errors.New("provider 503: synthetic late failure")})
	row := hudTurnView(t, hudGet(t, h, "DONE"), 1)
	gen := hudStageView(t, row, "generation")
	if gen["state"] != "done" || gen["detail"] != nil || row["retry"] != false {
		t.Fatal(row)
	}
}
