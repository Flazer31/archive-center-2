package store

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

func TestParseLinksInMemoriesStored(t *testing.T) {
	db := testDB(t)
	const raw = `{"entities":[{"id":"n1","name":"문지기"}],"memories":[
 {"id":"l2","from":"m1","rel":"causes","to":"m2","q":"종이 울려 문을 열었다",
  "knows":[{"who":"n1","via":"직접","ref":"m1"}],"unaware":["손님"],"hide_from":["경비"],"allow_to":["n1"]},
 {"id":"m2","kind":"event","body":"문이 열렸다","at":"l2"},
 {"id":"l3","from":"m2","rel":"recalls","to":"m1","q":"종소리를 떠올렸다"},
 {"id":"m1","kind":"event","body":"종이 울렸다"}],
 "links":[{"id":"l1","from":"m2","rel":"recalls","to":"m1","q":"먼저 적은 연결"}]}`
	out, parseNotes := memory.Parse(raw)
	job, err := db.JobFor(turnSource(twoTurnSession(), 0))
	if err != nil {
		t.Fatal(err)
	}
	notes, err := db.SaveGeneration(job, Generation{Model: "test", Response: raw, Output: out, Notes: parseNotes})
	if err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf([]int64{job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if count(t, db, "memories") != 2 || count(t, db, "links") != 3 || len(it.Memories) != 2 || len(it.Links) != 3 {
		t.Fatalf("stored memories %+v links %+v notes %v", it.Memories, it.Links, notes)
	}
	// m2 precedes m1 in the response; persistent IDs follow that order,
	// while links resolve both forward references and a moved link's ID.
	m2, m1 := it.Memories[0].ID, it.Memories[1].ID
	if it.Memories[0].Body != "문이 열렸다" || it.Memories[1].Body != "종이 울렸다" || it.Memories[0].At != it.Links[1].ID {
		t.Fatalf("memory order/link reference: %+v", it.Memories)
	}
	for i, want := range []struct{ from, rel, to, q string }{
		{m2, "recalls", m1, "먼저 적은 연결"},
		{m1, "causes", m2, "종이 울려 문을 열었다"},
		{m2, "recalls", m1, "종소리를 떠올렸다"},
	} {
		l := it.Links[i]
		if !strings.HasPrefix(l.ID, "L") || l.From != want.from || l.Rel != want.rel || l.To != want.to || l.Q != want.q {
			t.Fatalf("link %d: %+v want %+v", i, l, want)
		}
		var temp, id string
		if err := db.sql.QueryRow(`SELECT temp_id, item_id FROM gen_items WHERE job_id=? AND arr='links' AND pos=?`, job.ID, i).Scan(&temp, &id); err != nil {
			t.Fatal(err)
		}
		if temp != []string{"l1", "l2", "l3"}[i] || id != l.ID {
			t.Fatalf("issued link %d: %s -> %s, stored %s", i, temp, id, l.ID)
		}
	}
	awareness := map[string]bool{}
	for _, a := range it.Awareness {
		if a.Target != it.Links[1].ID {
			t.Fatalf("awareness target: %+v", a)
		}
		awareness[a.Kind+":"+a.Who] = true
		if a.Kind == "knows" && (a.Ref != m1 || a.Via != "직접") {
			t.Fatalf("knowledge refs: %+v", a)
		}
	}
	if len(it.Awareness) != 4 || !awareness["knows:"+it.Names[0].ID] || !awareness["unaware:손님"] || !awareness["hide_from:경비"] || !awareness["allow_to:"+it.Names[0].ID] {
		t.Fatalf("awareness: %+v", it.Awareness)
	}
	for _, note := range notes {
		if strings.Contains(note, "unresolved") || strings.Contains(note, "ambiguous") {
			t.Fatalf("references not resolved: %v", notes)
		}
	}
}
