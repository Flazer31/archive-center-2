package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// Merged names: the merges are read
// as the memory readers read them (every chat's, any chain), and the
// settings a merged person is computed with are the ones the screen shows.

// mergeChat loads chat into p with one name per turn, each with a body
// event, and returns the people's ids in order.
func mergeChat(t *testing.T, p *pipeline.Pipeline, db *store.DB, chat string, events ...string) []string {
	t.Helper()
	names := []string{"리나", "린", "리"}
	var rows []string
	var ids []string
	for i, kind := range events {
		rows = append(rows, memRow(2*i, "user", "u"+string(rune('1'+i)), "", "다음"), memRow(2*i+1, "char", "a"+string(rune('1'+i)), "g"+string(rune('1'+i)), names[i]+"가 쉬었다"))
		if _, err := p.Process([]observe.Record{attEvent(i+1, "output", chat, rows...)}); err != nil {
			t.Fatal(err)
		}
		var own []memory.ScopedTurn
		p.WithSession("chat:"+chat, func(s *lifecycle.Session) { own = memory.Own(s) })
		u3Save(t, db, own[i].Source, `{"elapsed_days":`+string(rune('0'+i))+`,"entities":[{"id":"n1","name":"`+names[i]+`","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"`+names[i]+`가 쉬었다"}],"body_events":[{"kind":"`+kind+`","who":"n1","q":"`+names[i]+`"}]}`)
	}
	// the ids from the names (a body read here would already keep a model
	// start under each name)
	var s *lifecycle.Session
	p.WithSession("chat:"+chat, func(v *lifecycle.Session) { s = v })
	k, err := db.CurrentKnown(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names[:len(events)] {
		for _, kn := range k.Names {
			if kn.Name == n {
				ids = append(ids, kn.ID)
			}
		}
	}
	if len(ids) != len(events) {
		t.Fatalf("fixture: one person per name: %v", ids)
	}
	return ids
}

// The settings kept under a merged name are what the kept name is computed
// with and what the screen shows; a model start chosen is kept with them,
// not copied under the kept name.
func TestBodyMergedSettingsOnScreen(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyCall(t, bodyHandler(p, db), "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	ids := mergeChat(t, p, db, "S", "recovery_started", "recovery_ended")
	keep, merge := ids[0], ids[1]
	spec := body.DefaultSpec()
	spec.CycleDays = 35
	if err := db.SetBodyPerson("chat:S", body.Person{Who: merge, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MergeNames("chat:S", keep, merge); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	bodyHandler(p, db)(rec, httptest.NewRequest("GET", "/body?chat_id=S", nil))
	var v struct {
		People []bodyPerson `json:"people"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.People) != 1 || v.People[0].Who != keep || v.People[0].Spec.CycleDays != 35 {
		t.Fatalf("the screen shows the settings the state is computed with: %+v", v.People)
	}
	if v.People[0].State.Cycle == nil || v.People[0].State.RefKind != "model_initialization" {
		t.Fatalf("fixture: a model start: %+v", v.People[0].State)
	}
	own, err := db.BodyPerson("chat:S", keep)
	if err != nil || own.Spec.CycleDays != body.DefaultSpec().CycleDays || own.Ref.Known() {
		t.Fatalf("settings copied under the kept name: %+v %v", own, err)
	}
	if kept, err := db.BodyPerson("chat:S", merge); err != nil || !kept.Ref.Known() || kept.Spec.CycleDays != 35 {
		t.Fatalf("the model start is kept with the settings it used: %+v %v", kept, err)
	}
}

// A chain of merges is one person.
func TestBodyMergeChain(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyCall(t, bodyHandler(p, db), "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	ids := mergeChat(t, p, db, "C", "period_start", "recovery_started", "recovery_ended")
	if _, err := db.MergeNames("chat:C", ids[1], ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MergeNames("chat:C", ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	if v := bodyGet(t, bodyHandler(p, db), "C"); len(v.People) != 1 || v.People[0].Who != ids[0] || len(v.People[0].Facts) != 3 {
		t.Fatalf("a merge chain is one person: %v", bodyKinds(v))
	}
}

// A merge made in a chat is followed where that chat is attached as a past,
// as the memory readers follow it.
func TestBodyMergeFollowedInAttachedPast(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyCall(t, bodyHandler(p, db), "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	ids := mergeChat(t, p, db, "A", "period_start", "recovery_started")
	if _, err := db.MergeNames("chat:A", ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	attCall(t, attachHandler(p, db, nil), "POST", "/chats/attach", `{"chat_id":"X","chats":["A"]}`, 200)
	var x *lifecycle.Session
	p.WithSession("chat:X", func(v *lifecycle.Session) { x = v })
	states, err := db.BodyStates(x.Key, memory.ScopeOf(x))
	if err != nil || len(states) != 1 || states[0].Who != ids[0] || len(states[0].Facts) != 2 {
		t.Fatalf("the attached past reads the merged person as one: %v %+v", err, states)
	}
}
