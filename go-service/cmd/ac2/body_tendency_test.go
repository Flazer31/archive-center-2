package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// Body tendencies through the store and the screen (1.1): a person's
// intensity is drawn once and kept, the screen shows and edits it, and the
// delivery carries the tendency under its own header.
func TestBodyTendencyIntensityKeptAndDelivered(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	h := bodyHandler(p, db)
	bodyCall(t, h, "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	ids := mergeChat(t, p, db, "T", "period_start")
	who := ids[0]
	get := func() bodyPerson {
		t.Helper()
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/body?chat_id=T", nil))
		var v struct {
			People []bodyPerson `json:"people"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || len(v.People) != 1 {
			t.Fatalf("GET body: %v %s", err, rec.Body.String())
		}
		return v.People[0]
	}
	first := get()
	kept, err := db.BodyPerson("chat:T", who)
	if err != nil || first.Intensity == nil || kept.Intensity == nil || *kept.Intensity != *first.Intensity {
		t.Fatalf("drawn once and kept: screen %+v stored %+v %v", first.Intensity, kept.Intensity, err)
	}
	if again := get(); *again.Intensity != *first.Intensity {
		t.Fatalf("drawn again: %+v vs %+v", again.Intensity, first.Intensity)
	}
	var tendency string
	for _, l := range first.State.Lines {
		if strings.HasPrefix(l, "- body tendency ") {
			tendency = l
		}
	}
	if tendency == "" {
		t.Fatalf("the screen shows the tendency: %v", first.State.Lines)
	}
	// the screen edits it; the cycle settings and model start stay
	in := *first.Intensity
	in.Cramps = 3
	b, _ := json.Marshal(map[string]any{"chat_id": "T", "who": who, "spec": first.Spec, "intensity": in})
	bodyCall(t, h, "POST", "/body/person", string(b), 200)
	if after := get(); after.Intensity == nil || *after.Intensity != in {
		t.Fatalf("edited intensity: %+v", after.Intensity)
	}
	b, _ = json.Marshal(map[string]any{"chat_id": "T", "who": who, "spec": first.Spec})
	bodyCall(t, h, "POST", "/body/person", string(b), 200)
	if after := get(); after.Intensity == nil || *after.Intensity != in {
		t.Fatalf("a save without intensity keeps it: %+v", after.Intensity)
	}
	bad, _ := json.Marshal(map[string]any{"chat_id": "T", "who": who, "spec": first.Spec, "intensity": body.Intensity{Cramps: 4}})
	bodyCall(t, h, "POST", "/body/person", string(bad), 400)
	// delivered: the facts, then the tendencies under their own header
	var s *lifecycle.Session
	p.WithSession("chat:T", func(v *lifecycle.Session) { s = v })
	scope, turn := memory.ScopeOf(s), tendencyTurnOf(s)
	lines, err := bodyLines(db, "chat:T", scope, turn, "")
	if err != nil {
		t.Fatal(err)
	}
	at := -1
	for i, l := range lines {
		if l == body.TendencyHeader {
			at = i
		}
	}
	if !body.TendencyTurn(turn) {
		t.Fatalf("fixture: a turn with the tendencies (%d turns)", turn)
	}
	if at < 1 || at+1 >= len(lines) || !strings.HasPrefix(lines[at+1], "- body tendency ") || !strings.HasPrefix(lines[0], "- body ") {
		t.Fatalf("delivered lines: %v", lines)
	}
	// the next turns carry the facts only; the screen still shows the
	// tendency (one turn in body.TendencyEvery)
	for _, more := range []int{1, 2} {
		got, err := bodyLines(db, "chat:T", scope, turn+more, "")
		if err != nil || slices.Contains(got, body.TendencyHeader) || len(got) != at {
			t.Fatalf("turn %d: %v %v", turn+more, got, err)
		}
	}
}

// The turn the tendencies count is the chat's own: turning a turn off in
// the host or attaching a past does not move it (the read scope's length
// did), through the production observation path.
func TestBodyTendencyTurnIsTheChats(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	var rows []string
	for i := 0; i < 4; i++ {
		rows = append(rows, memRow(2*i, "user", fmt.Sprintf("u%d", i), "", "입력"), memRow(2*i+1, "char", fmt.Sprintf("a%d", i), fmt.Sprintf("g%d", i), "답"))
	}
	read := func(seq int, rows []string) (int, int) {
		t.Helper()
		if _, err := p.Process([]observe.Record{attEvent(seq, "output", "Q", rows...)}); err != nil {
			t.Fatal(err)
		}
		var turn, scope int
		p.WithSession("chat:Q", func(s *lifecycle.Session) { turn, scope = tendencyTurnOf(s), len(memory.ScopeOf(s)) })
		return turn, scope
	}
	on, onScope := read(1, rows)
	off := append([]string(nil), rows...)
	off[0] = `{"i":0,"role":"user","chat_id":"u0","generation_id":"","disabled":"true","data":{"text":"입력"}}`
	offTurn, offScope := read(2, off)
	if on != 4 || offTurn != on || offScope == onScope {
		t.Fatalf("turn %d / %d with the first turned off (scope %d / %d)", on, offTurn, onScope, offScope)
	}
}
