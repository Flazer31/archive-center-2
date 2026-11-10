package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

func TestReadWordsSavedEditedAndLegacy(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	job, err := db.JobFor(turnSource(s, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveGeneration(job, Generation{Response: turn1Output, Output: parse(t, turn1Output)}); err != nil {
		t.Fatal(err)
	}
	read := func() memory.Known {
		k, err := db.KnownOf([]int64{job.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range k.Memories {
			if !reflect.DeepEqual(m.BodyWords, memory.Words(m.Body)) {
				t.Fatalf("projection %s", m.ID)
			}
		}
		return k
	}
	k := read()
	want, _ := json.Marshal(memory.Select(k, "한얼 30냥 후련함", nil))
	if _, err := db.sql.Exec(`UPDATE memories SET read_words=NULL WHERE job_id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	legacy := read()
	got, _ := json.Marshal(memory.Select(legacy, "한얼 30냥 후련함", nil))
	if string(got) != string(want) {
		t.Fatal("legacy projection changed selection")
	}
	body := "흑요석 단검을 보관했다"
	if err := db.EditMemory(job.Session, k.Memories[0].ID, &body, nil); err != nil {
		t.Fatal(err)
	}
	read()
}
