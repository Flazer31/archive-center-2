package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// C30 (AC1's missing-turn rescan): the turns whose memory could not be
// made, retries used up included, are listed first and then made again by
// the production worker; a made turn and a turn turned off are left alone.
func TestRemakeFailedTurns(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "첫 입력"), memRow(1, "char", "a1", "g1", "첫 응답"),
		memRow(2, "user", "u2", "", "둘째 입력"), memRow(3, "char", "a2", "g2", "둘째 응답"),
		memRow(4, "user", "u3", "", "셋째 입력"), memRow(5, "char", "a3", "g3", "셋째 응답"),
		memRow(6, "user", "u4", "", "넷째 입력"), memRow(7, "char", "a4", "g4", "넷째 응답")} // the fourth: waiting, not failed
	if _, err := p.Process([]observe.Record{attEvent(1, "load", "X", rows...)}); err != nil {
		t.Fatal(err)
	}
	var own []memory.ScopedTurn
	p.WithSession("chat:X", func(s *lifecycle.Session) {
		for _, turn := range s.ActiveTurns() {
			turn.Confirmed = true
		}
		if err := db.SaveSession(s, nil, nil); err != nil {
			t.Fatal(err)
		}
		own = memory.Own(s)
	})
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"memories":[{"id":"m1","kind":"fact","body":"첫 기록"}]}`) // made
	j, err := db.JobFor(own[1].Source)                                                                      // failed with every retry used
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		db.StartAttempt(j)
		if err := db.FailJob(j, "m", "p", "[]", "provider down", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Process([]observe.Record{attEvent(2, "load", "X", append(rows[:4:4], `{"i":4,"role":"user","chat_id":"u3","generation_id":"","disabled":true,"data":{"text":"셋째 입력"}}`, rows[5], rows[6], rows[7])...)}); err != nil {
		t.Fatal(err)
	}
	// the turn turned off has a failed job of its current source too
	p.WithSession("chat:X", func(s *lifecycle.Session) { own = memory.Own(s) })
	if !own[2].Off {
		t.Fatal("the third turn is not turned off")
	}
	j3off, _ := db.JobFor(own[2].Source)
	db.StartAttempt(j3off)
	db.FailJob(j3off, "m", "p", "[]", "provider down", time.Now())
	m := &pastRecorder{}
	worker := generate.NewWorker(db, m, "recording", time.Second)
	h := remakeFailedHandler(p, db, worker, nil)
	call := func(body string) []int {
		t.Helper()
		r := httptest.NewRecorder()
		h(r, httptest.NewRequest(http.MethodPost, "/memory/turn/remake-failed", strings.NewReader(body)))
		if r.Code != 200 {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		var out struct{ Turns []int }
		json.Unmarshal(r.Body.Bytes(), &out)
		return out.Turns
	}
	if got := call(`{"chat_id":"X","dry_run":true}`); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("preview %v", got)
	}
	if st, _ := db.SourceJobState(own[1].Source); st != "failed" {
		t.Fatalf("preview changed the job: %s", st)
	}
	if got := call(`{"chat_id":"X"}`); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("remade %v", got)
	}
	runUntilMade(t, db, worker, own[1].Source)
	if got := call(`{"chat_id":"X","dry_run":true}`); len(got) != 0 {
		t.Fatalf("still failed %v", got)
	}
	r := httptest.NewRecorder()
	h(r, httptest.NewRequest(http.MethodPost, "/memory/turn/remake-failed", strings.NewReader(`{}`)))
	if r.Code != 400 {
		t.Fatalf("no chat: %d", r.Code)
	}
}
