package memory

import (
	"encoding/json"
	"testing"
)

// A basis naming several sources is kept joined; each source is its own
// reference for the names read with it and for the search index, and a
// source that is plain text stays as written.
func TestBasisSourcesAreEachAReference(t *testing.T) {
	var m Memory
	if err := json.Unmarshal([]byte(`{"id":"m1","kind":"view","owner":"N3","body":"들은 소식","basis":{"via":"전언","from":["N1","N2"]}}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Basis.From != "N1,N2" {
		t.Fatalf("joined %q", m.Basis.From)
	}
	refs := map[string]bool{}
	for _, r := range refsOf(m) {
		refs[r] = true
	}
	if !refs["N1"] || !refs["N2"] || refs["N1,N2"] {
		t.Fatalf("names read with the record: %v", refsOf(m))
	}
	found := map[string]bool{}
	for _, e := range MemoryRefs(m) {
		if e.Field == "basis_from" {
			found[e.Value] = true
		}
	}
	if !found["N1"] || !found["N2"] || found["N1,N2"] {
		t.Fatalf("index entries: %+v", MemoryRefs(m))
	}
	name := func(s string) string {
		if s == "N1" {
			return "민재"
		}
		return s
	}
	if got := MapSource("N1, 마을 사람들", name); got != "민재, 마을 사람들" {
		t.Fatalf("mapped %q", got)
	}
}

// A later part of a long reply is renumbered: each source of a basis
// follows its entity's new id.
func TestRenumberRenamesEachBasisSource(t *testing.T) {
	var out Output
	if err := json.Unmarshal([]byte(`{"entities":[{"id":"n1","name":"민재"},{"id":"n2","name":"서진"},{"id":"n3","name":"하루"}],"memories":[{"id":"m1","kind":"view","owner":"n3","body":"들은 소식","basis":{"via":"전언","from":["n1","n2"]}}]}`), &out); err != nil {
		t.Fatal(err)
	}
	got := Renumber(out, 2)
	if from := got.Memories[0].Basis.From; from != "n2001,n2002" {
		t.Fatalf("renumbered sources %q, entities %+v", from, got.Entities)
	}
}
