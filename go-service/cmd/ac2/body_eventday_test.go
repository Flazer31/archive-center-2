package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// Through the store: an event the critic dated with the story's own day
// number is on its turn's stored day; days before the turn count back
// from it.
func TestBodyEventOnStoredDays(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyCall(t, bodyHandler(p, db), "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	rows := []string{memRow(0, "user", "u1", "", "의원"), memRow(1, "char", "a1", "g1", "임신 진단"), memRow(2, "user", "u2", "", "한 달"), memRow(3, "char", "a2", "g2", "유산")}
	if _, err := p.Process([]observe.Record{attEvent(1, "output", "E", rows...)}); err != nil {
		t.Fatal(err)
	}
	var own []memory.ScopedTurn
	p.WithSession("chat:E", func(s *lifecycle.Session) { own = memory.Own(s) })
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"세나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"세나가 임신 진단을 받았다"}],"body_events":[{"kind":"pregnancy_confirmed","who":"n1","when":"day 100"}]}`)
	var s *lifecycle.Session
	p.WithSession("chat:E", func(v *lifecycle.Session) { s = v })
	k, _ := db.CurrentKnown(s)
	who := k.Names[0].ID
	u3Save(t, db, own[1].Source, `{"elapsed_days":30,"memories":[{"id":"m1","kind":"event","body":"세나가 아이를 잃었다"}],"body_events":[{"kind":"pregnancy_ended","who":"`+who+`","outcome":"loss"},{"kind":"recovery_started","who":"`+who+`","when":"2 days before"}]}`)
	states, err := db.BodyStates("chat:E", memory.ScopeOf(s))
	if err != nil || len(states) != 1 {
		t.Fatal(err, states)
	}
	days := map[string]body.Day{}
	for _, f := range states[0].Facts {
		days[f.Kind] = f.Day
	}
	if days["pregnancy_confirmed"] != (body.Day{Axis: "day", N: 0}) || days["pregnancy_ended"] != (body.Day{Axis: "day", N: 30}) || days["recovery_started"] != (body.Day{Axis: "day", N: 28}) {
		t.Fatalf("days: %+v", days)
	}
}

// A result frozen on the day an event was read on before (the story's
// "day N" as given) keeps that day, so its stored cycles stay in use;
// an event without a result moves to its turn's day.
func TestBodyFrozenResultKeepsItsDay(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyCall(t, bodyHandler(p, db), "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	rows := []string{memRow(0, "user", "u1", "", "밤"), memRow(1, "char", "a1", "g1", "관계")}
	if _, err := p.Process([]observe.Record{attEvent(1, "output", "F", rows...)}); err != nil {
		t.Fatal(err)
	}
	var own []memory.ScopedTurn
	p.WithSession("chat:F", func(s *lifecycle.Session) { own = memory.Own(s) })
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"세나","type":"person"},{"id":"n2","name":"도윤","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"둘이 밤을 보냈다"}],"body_events":[{"kind":"period_start","who":"n1","when":"day 90"},{"kind":"conception_exposure","who":"n1","when":"day 100","partners":["n2"],"exposure":{"classification":"potentially_conceiving","partner_compatibility":"compatible","contraception":"none"}}]}`)
	// freeze a result as an earlier reading did: on Day 100
	frozen := body.Conceive(body.DefaultSpec(), "seed", "N1", 90, body.ExposureInput{Day: 100, Classification: "potentially_conceiving", Compatibility: "compatible", Contraception: "none"})
	b, _ := json.Marshal(frozen)
	conn, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`UPDATE body_events SET result=? WHERE kind='conception_exposure'`, string(b)); err != nil {
		t.Fatal(err)
	}
	var s *lifecycle.Session
	p.WithSession("chat:F", func(v *lifecycle.Session) { s = v })
	states, err := db.BodyStates("chat:F", memory.ScopeOf(s))
	if err != nil || len(states) == 0 {
		t.Fatal(err, states)
	}
	days := map[string]body.Day{}
	for _, f := range states[0].Facts {
		days[f.Kind] = f.Day
	}
	if days["conception_exposure"] != (body.Day{Axis: "day", N: 100}) || days["period_start"] != (body.Day{Axis: "day", N: 0}) {
		t.Fatalf("days: %+v", days)
	}
}
