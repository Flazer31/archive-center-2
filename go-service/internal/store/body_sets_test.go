package store

import (
	"testing"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/memory"
)

// bodySets: a chain of disguises is read as its end (not a middle name
// that is a disguise itself, even when it is the earliest); a true name is
// read before an alias's target; a name the user kept before both; names
// no relation joins stay apart.
func TestBodySetsPicksPerson(t *testing.T) {
	links := []bodyLink{{"N2", "N1", "disguise"}, {"N1", "N3", "disguise"}, {"N5", "N4", "alias"}, {"N6", "N4", "true_name"}}
	got := bodySets(nil, links)
	for id, want := range map[string]string{"N1": "N3", "N2": "N3", "N3": "N3", "N4": "N6", "N5": "N6", "N6": "N6"} {
		if got[id] != want {
			t.Fatalf("%s read as %s, want %s: %v", id, got[id], want, got)
		}
	}
	if _, ok := got["N7"]; ok {
		t.Fatalf("a name no relation joins: %v", got)
	}
	kept := bodySets(map[string]string{"N3": "N2"}, links)
	if kept["N1"] != "N2" || kept["N3"] != "N2" {
		t.Fatalf("the name the user kept: %v", kept)
	}
}

func TestBodyStatesFrozenDayAxis(t *testing.T) {
	db := testDB(t)
	if err := db.SetBodyTracking(body.Toggles{Pregnancy: true}); err != nil {
		t.Fatal(err)
	}
	for _, calendar := range []string{"1970/01/08", ""} {
		src := memory.Source{Session: "chat:axis" + calendar, Turn: 1, Responses: []memory.SourceResponse{{Seq: 1, Data: "확인했다"}}}
		if calendar != "" {
			src.Dates = map[string]string{"clock": calendar}
		}
		j, _, _ := a24Save(t, db, src, `{"entities":[{"id":"n1","name":"리나"}],"elapsed_days":0,"body_events":[{"kind":"conception_exposure","who":"n1","when":"Day 7"}]}`)
		const frozen = `{"status":"evaluated","exposure_day":7}`
		if _, err := db.sql.Exec("UPDATE body_events SET result=? WHERE job_id=?", frozen, j.ID); err != nil {
			t.Fatal(err)
		}
		states, err := db.BodyStates(src.Session, []memory.ScopedTurn{{Source: src, Confirmed: true}})
		if err != nil || len(states) != 1 || len(states[0].Facts) != 1 {
			t.Fatalf("read frozen event: %+v, %v", states, err)
		}
		want := body.DayOf("Day 7")
		if calendar != "" {
			want = body.DayOf(calendar)
		}
		record := states[0].Facts[0]
		if record.Day != want || record.Result == nil || record.Result.ExposureDay != 7 {
			t.Errorf("stored date %q: day %+v, want %+v; result %+v", calendar, record.Day, want, record.Result)
		}
	}
}
