package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/body"
)

// The critic's body events as stored: an exposure with a named method is
// drawn with it, one whose contraception the text does not name is drawn
// as none, and emergency contraception is kept.
func TestStoredContraception(t *testing.T) {
	db := modelsDB(t)
	_, scope := bodyNowChat(t, db, "BC1", [3]string{"리나가 왔다", "하루가 지났다", "그날 밤"}, `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"하루가 지났다"}]}`)
	db.SetBodyTracking(body.Toggles{Cycle: true, Pregnancy: true})
	who := bodyState(t, db, "BC1", scope).Who
	u3Save(t, db, scope[2].Source, fmt.Sprintf(`{"elapsed_days":0,"memories":[{"id":"m1","kind":"event","body":"그날 밤"}],"body_events":[`+
		`{"kind":"conception_exposure","who":%q,"exposure":{"classification":"potentially_conceiving","partner_compatibility":"compatible","contraception":"present","method":"condom"}},`+
		`{"kind":"conception_exposure","who":%q,"exposure":{"classification":"potentially_conceiving","partner_compatibility":"compatible","contraception":"unknown"}},`+
		`{"kind":"emergency_contraception","who":%q,"when":"1 days after"}]}`, who, who, who))
	st := bodyState(t, db, "BC1", scope)
	var kinds []string
	var results []*body.Exposure
	for _, f := range st.Facts {
		kinds = append(kinds, f.Kind)
		if f.Kind == "conception_exposure" {
			results = append(results, f.Result)
		}
	}
	if len(results) != 2 || results[0] == nil || results[1] == nil {
		t.Fatalf("facts %v results %+v", kinds, results)
	}
	if r := results[0]; r.Status != "evaluated" || r.Contraception != "condom" || r.ContraceptionFactor != 0.15 {
		t.Fatalf("condom: %+v", r)
	}
	if r := results[1]; r.Status != "evaluated" || r.Contraception != "" {
		t.Fatalf("contraception not said: %+v", r)
	}
	last := st.Facts[len(st.Facts)-1]
	if last.Kind != "emergency_contraception" || last.Day.Text() != "Day 2" {
		t.Fatalf("emergency contraception not kept on its day: %v %+v", kinds, last)
	}
}

// A later exposure stored as skipped for the model's pregnancy under way
// (before 1.1) is read again once emergency contraception stops that
// pregnancy, and what it is read as is stored in its place (the store
// kept only empty results).
func TestStoredSkipReplaced(t *testing.T) {
	db := modelsDB(t)
	_, scope := bodyNowChat(t, db, "BS1", [3]string{"리나가 왔다", "열흘이 지났다", "그날 밤"}, `{"elapsed_days":10,"memories":[{"id":"m1","kind":"event","body":"열흘이 지났다"}]}`)
	db.SetBodyTracking(body.Toggles{Cycle: true, Pregnancy: true})
	who := bodyState(t, db, "BS1", scope).Who
	u3Save(t, db, scope[2].Source, fmt.Sprintf(`{"elapsed_days":0,"memories":[{"id":"m1","kind":"event","body":"그날 밤"}],"body_events":[`+
		`{"kind":"conception_exposure","who":%q,"exposure":{"classification":"potentially_conceiving","partner_compatibility":"compatible","contraception":"none"}},`+
		`{"kind":"conception_exposure","who":%q,"when":"3 days after","exposure":{"classification":"potentially_conceiving","partner_compatibility":"compatible","contraception":"none"}}]}`, who, who))
	conn, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// the first exposure (Day 10) drew a pregnancy that emergency
	// contraception would stop; the second (Day 13, its ovulation) was kept
	// as skipped
	draw := 0.1
	a, _ := json.Marshal(body.Exposure{Version: body.ModelVersion, Status: "evaluated", Implanted: true, ExposureDay: 10, OvulationDay: 13, ImplantationDay: 21, BirthDay: 279, EmergencyDraw: &draw})
	b, _ := json.Marshal(body.Exposure{Version: body.ModelVersion, Status: "not_applicable", Reason: "modeled_pregnancy_ongoing", ExposureDay: 13})
	var ids []int64
	rows, err := conn.Query(`SELECT id FROM body_events WHERE session_key='chat:BS1' AND kind='conception_exposure' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("exposures %v", ids)
	}
	conn.Exec(`UPDATE body_events SET result=? WHERE id=?`, string(a), ids[0])
	conn.Exec(`UPDATE body_events SET result=? WHERE id=?`, string(b), ids[1])
	stored := func() body.Exposure {
		var raw string
		if err := conn.QueryRow(`SELECT result FROM body_events WHERE id=?`, ids[1]).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var x body.Exposure
		json.Unmarshal([]byte(raw), &x)
		return x
	}
	// a result kept as not drawn by the rules before 1.1 is read again and
	// stored as drawn now
	before11, _ := json.Marshal(body.Exposure{Version: body.ModelVersion, Status: "unknown", Reason: "exposure_context_not_modeled", ExposureDay: 10})
	conn.Exec(`UPDATE body_events SET result=? WHERE id=?`, string(before11), ids[0])
	bodyState(t, db, "BS1", scope)
	var redrawn string
	if err := conn.QueryRow(`SELECT result FROM body_events WHERE id=?`, ids[0]).Scan(&redrawn); err != nil || !strings.Contains(redrawn, `"status":"evaluated"`) {
		t.Fatalf("an old skip stored again: %s %v", redrawn, err)
	}
	conn.Exec(`UPDATE body_events SET result=? WHERE id=?`, string(a), ids[0])
	conn.Exec(`UPDATE body_events SET result=? WHERE id=?`, string(b), ids[1])
	// without emergency contraception it stays skipped
	bodyState(t, db, "BS1", scope)
	if x := stored(); x.Reason != "modeled_pregnancy_ongoing" {
		t.Fatalf("before: %+v", x)
	}
	if _, err := db.RecordBodyFact("chat:BS1", who, "emergency_contraception", "Day 11"); err != nil {
		t.Fatal(err)
	}
	bodyState(t, db, "BS1", scope)
	first := stored()
	if first.Status != "evaluated" || first.Reason != "" || first.ExposureDay != 13 {
		t.Fatalf("after the stop, stored: %+v", first)
	}
	// kept as stored: read again, the same
	bodyState(t, db, "BS1", scope)
	if again := stored(); again.Implanted != first.Implanted || again.Status != first.Status || again.CycleIndex != first.CycleIndex {
		t.Fatalf("stored %+v, then %+v", first, again)
	}
}
