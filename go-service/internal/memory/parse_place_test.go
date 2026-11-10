package memory

import (
	"reflect"
	"testing"
)

// 2026-10-30: 4 of 34 real turns wrote an event's place on the memory, not
// in roles; one wrote a basis source as a list. Both were lost.
func TestParsePlaceBesideRolesAndBasisFromList(t *testing.T) {
	raw := `{"entities":[],"memories":[
{"id":"m1","kind":"event","body":"a","place":"N111,N110","roles":{"actor":["N49"]}},
{"id":"m2","kind":"event","body":"b","place":["N126"]},
{"id":"m3","kind":"event","body":"c","place":"N5","roles":{"place":["N6"]}},
{"id":"m4","kind":"view","owner":"N49","about":"N83","aspect":"belief","body":"d","basis":{"via":"추론","from":["N83","N84"],"confidence":"보통"}}
]}`
	out, notes := Parse(raw)
	if len(out.Memories) != 4 {
		t.Fatalf("items lost: %d %v", len(out.Memories), notes)
	}
	want := [][]string{{"N111", "N110"}, {"N126"}, {"N6"}}
	for i, w := range want {
		r := out.Memories[i].Roles
		if r == nil || !reflect.DeepEqual(r.Place, w) {
			t.Fatalf("memory %d place = %+v, want %v (%v)", i+1, r, w, notes)
		}
	}
	if out.Memories[0].Roles.Actor[0] != "N49" {
		t.Fatal("other roles lost")
	}
	b := out.Memories[3].Basis
	if b == nil || b.Via != "추론" || b.From != "N83,N84" || b.Confidence != "보통" {
		t.Fatalf("basis = %+v (%v)", b, notes)
	}
	var one Basis
	if err := one.UnmarshalJSON([]byte(`{"via":"전언","from":"N1"}`)); err != nil || one.From != "N1" {
		t.Fatalf("single source: %+v %v", one, err)
	}
	if err := one.UnmarshalJSON([]byte(`"경험"`)); err != nil || one.Via != "경험" {
		t.Fatalf("string basis: %+v %v", one, err)
	}
}
