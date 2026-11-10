package store

import (
	"reflect"
	"sort"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// A shared goal written for two new names keeps both members through the
// temporary-id resolution, the stored detail and the reference index.
func TestToSeveralStored(t *testing.T) {
	db := testDB(t)
	j, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"entities":[{"id":"n1","name":"세인"},{"id":"n2","name":"모몬"}],
 "memories":[{"id":"m1","kind":"fact","about":"n1","mode":"공동 목표","body":"함께 마을을 지킨다","to":["n1","n2"]}]}`)
	if len(it.Memories) != 1 || len(it.Names) != 2 {
		t.Fatalf("rows %+v notes %v", it, notes)
	}
	ids := map[string]string{}
	for _, n := range it.Names {
		ids[n.Name] = n.ID
	}
	want := memory.Who{ids["세인"], ids["모몬"]}
	if got := it.Memories[0].To; !reflect.DeepEqual(got, want) {
		t.Fatalf("to stored as %v, want %v (notes %v)", got, want, notes)
	}
	rows, err := db.sql.Query(`SELECT ref FROM search_refs WHERE job_id=? AND field='to' ORDER BY ref`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sorted := append(memory.Who{}, want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(memory.Who(refs), sorted) {
		t.Fatalf("to index %v, want %v", refs, sorted)
	}
}
