package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// A birth the critic recorded is read with its outcome through the store,
// starts the recovery by itself (not well the next day), and shows on the
// screen; an intensity kept before the recovery speed gets one drawn, the
// rest kept.
func TestBodyBirthStartsRecovery(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	h := bodyHandler(p, db)
	bodyCall(t, h, "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	u1, a1 := memRow(0, "user", "u1", "", "출산"), memRow(1, "char", "a1", "g1", "리나가 아이를 낳았다")
	if _, err := p.Process([]observe.Record{attEvent(1, "output", "B", u1, a1)}); err != nil {
		t.Fatal(err)
	}
	var own []memory.ScopedTurn
	p.WithSession("chat:B", func(s *lifecycle.Session) { own = memory.Own(s) })
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 아이를 낳았다"}],"body_events":[{"kind":"pregnancy_ended","who":"n1","outcome":"birth","q":"아이를 낳았다"}]}`)
	var s *lifecycle.Session
	p.WithSession("chat:B", func(v *lifecycle.Session) { s = v })
	k, _ := db.CurrentKnown(s)
	who := k.Names[0].ID
	// an intensity kept before 1.1.0-test.6 has no recovery speed
	old := body.Intensity{Cramps: 3, PMS: 1, Nausea: 0, Discomfort: 2}
	if err := db.SetBodyPerson("chat:B", body.Person{Who: who, Spec: body.DefaultSpec(), Intensity: &old}); err != nil {
		t.Fatal(err)
	}
	states, err := db.BodyStates("chat:B", memory.ScopeOf(s))
	if err != nil || len(states) != 1 {
		t.Fatal(err, states)
	}
	st := states[0]
	if st.Recovering == nil || st.Recovering.Outcome != "birth" || st.Tendency == nil || !strings.HasPrefix(st.Tendency.Phase, "after the birth (day 1)") || !strings.Contains(strings.Join(st.Tendency.Words, ","), "bleeding") {
		t.Fatalf("the birth's recovery: %+v tendency %+v", st.Recovering, st.Tendency)
	}
	kept, err := db.BodyPerson("chat:B", who)
	if err != nil || kept.Intensity == nil || kept.Intensity.Recovery < 1 || kept.Intensity.Recovery > 3 || kept.Intensity.Cramps != 3 || kept.Intensity.Nausea != 0 {
		t.Fatalf("recovery speed drawn, the rest kept: %+v %v", kept.Intensity, err)
	}
	rec := httptestGetBody(t, h, "B")
	if !strings.Contains(rec, `"outcome":"birth"`) || !strings.Contains(rec, `"recovery_speed":`) {
		t.Fatalf("screen: %s", rec)
	}
	var v struct {
		People []bodyPerson `json:"people"`
	}
	json.Unmarshal([]byte(rec), &v)
	if len(v.People) != 1 || !strings.Contains(strings.Join(v.People[0].State.Lines, "|"), "after the birth") {
		t.Fatalf("screen lines: %+v", v.People)
	}
}

func httptestGetBody(t *testing.T, h http.HandlerFunc, chat string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/body?chat_id="+chat, nil))
	if rec.Code != 200 {
		t.Fatalf("GET body: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
