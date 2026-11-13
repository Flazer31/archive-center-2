package store

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// A basis naming several sources ("from": ["n1","n2"], kept joined) has each
// source turned into its stored name id and shown by name: in storage, in
// the memory list, in the detail labels and on the delivered line (the
// joined text was one reference, left as "n1,n2").
func TestBasisWithSeveralSourcesKeepsEachName(t *testing.T) {
	db := testDB(t)
	tr := detailFixtureTurn(3, "전해 들은 소식")
	s := lifecycle.Restore("chat:FROM", []*lifecycle.Turn{tr}, nil)
	detailSaveSession(t, db, s)
	src := memory.SourceOf(s.Key, tr)
	_, it := detailSave(t, db, src, `{"entities":[{"id":"n1","name":"민재","type":"person"},{"id":"n2","name":"서진","type":"person"},{"id":"n3","name":"하루","type":"person"}],"memories":[{"id":"m1","kind":"view","owner":"n3","body":"두 사람에게 들은 소식을 믿는다","basis":{"via":"전언","from":["n1","n2"]}}]}`)
	ids := map[string]string{}
	for _, n := range it.Names {
		ids[n.Name] = n.ID
	}
	m := it.Memories[0]
	want := ids["민재"] + "," + ids["서진"]
	if m.Memory.Basis == nil || m.Memory.Basis.From != want || strings.Contains(m.Memory.Basis.From, "n1") {
		t.Fatalf("stored sources %+v, want %q", m.Memory.Basis, want)
	}
	name := func(id string) string {
		for _, n := range it.Names {
			if n.ID == id {
				return n.Name
			}
		}
		return id
	}
	if got := listedMemory(m.Memory, name).Basis.From; got != "민재,서진" {
		t.Fatalf("listed sources %q", got)
	}
	v := detailGet(t, db, s.Key, tr.ID, m.ID)
	if v.Labels[ids["민재"]] != "민재" || v.Labels[ids["서진"]] != "서진" {
		t.Fatalf("detail labels %v", v.Labels)
	}
	line := deliveryLine(memory.KnownMemory{Memory: m.Memory, ID: m.ID}, map[string]string{ids["민재"]: "민재", ids["서진"]: "서진", ids["하루"]: "하루"}, "", nil)
	if !strings.Contains(line, "from 민재, 서진") {
		t.Fatalf("delivered line %q", line)
	}
}

// Merging a name that a memory has only as one of several sources finds
// that memory and moves the source; a source written with a space after the
// comma keeps it.
func TestNameMergeMovesOneOfSeveralSources(t *testing.T) {
	db := testDB(t)
	tr := detailFixtureTurn(4, "전해 들은 소식")
	s := lifecycle.Restore("chat:MERGE", []*lifecycle.Turn{tr}, nil)
	detailSaveSession(t, db, s)
	_, it := detailSave(t, db, memory.SourceOf(s.Key, tr), `{"entities":[{"id":"n1","name":"민재","type":"person"},{"id":"n2","name":"서진","type":"person"},{"id":"n3","name":"하루","type":"person"},{"id":"n4","name":"서진이","type":"person"}],"memories":[{"id":"m1","kind":"view","owner":"n3","body":"두 사람에게 들은 소식을 믿는다","basis":{"via":"전언","from":["n1","n2"]}},{"id":"m2","kind":"view","owner":"n3","body":"또 들었다","basis":{"via":"전언","from":"n1, n2"}}]}`)
	ids := map[string]string{}
	for _, n := range it.Names {
		ids[n.Name] = n.ID
	}
	if got := it.Memories[1].Memory.Basis.From; got != ids["민재"]+", "+ids["서진"] {
		t.Fatalf("stored spaced sources %q", got)
	}
	p, err := db.PreviewMergeNames(s.Key, ids["서진이"], ids["서진"])
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Counts["memories"]; c.Count == nil || *c.Count != 2 {
		t.Fatalf("merge preview counts memories %+v, want 2", c)
	}
	if _, err := db.MergeNames(s.Key, ids["서진이"], ids["서진"]); err != nil {
		t.Fatal(err)
	}
	after, err := db.ItemsOf([]int64{it.Memories[0].Job})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{it.Memories[0].ID: ids["민재"] + "," + ids["서진이"], it.Memories[1].ID: ids["민재"] + ", " + ids["서진이"]}
	for _, m := range after.Memories {
		if w, ok := want[m.ID]; ok && m.Memory.Basis.From != w {
			t.Fatalf("%s sources after the merge %q, want %q", m.ID, m.Memory.Basis.From, w)
		}
	}
}
