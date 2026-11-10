package memory

import (
	"reflect"
	"strings"
	"testing"
)

// A fact for several people (a shared goal's members) keeps every id in
// "to"; the one-id form still reads as before.
func TestToSeveral(t *testing.T) {
	out, notes := Parse(`{"memories":[
 {"id":"m1","kind":"fact","about":"N1","mode":"공동 목표","body":"함께 성을 지킨다","to":["N1","N2"]},
 {"id":"m2","kind":"fact","about":"N1","mode":"의무","body":"매일 연락","to":"N3"}]}`)
	if len(notes) != 0 {
		t.Fatalf("notes %v", notes)
	}
	if len(out.Memories) != 2 || !reflect.DeepEqual(out.Memories[0].To, Who{"N1", "N2"}) || !reflect.DeepEqual(out.Memories[1].To, Who{"N3"}) {
		t.Fatalf("to read as %+v", out.Memories)
	}
	var to []string
	for _, e := range MemoryRefs(out.Memories[0]) {
		if e.Field == "to" {
			to = append(to, e.Value)
		}
	}
	if !reflect.DeepEqual(to, []string{"N1", "N2"}) {
		t.Fatalf("index to %v", to)
	}
	known := RenderKnown(Selection{Memories: []KnownMemory{{ID: "M1", Turn: 1, Memory: out.Memories[0]}}}, nil)
	if !strings.Contains(known, " to N1,N2") {
		t.Fatalf("KNOWN line %q", known)
	}
	if got := refsOf(out.Memories[0]); !contains(got, "N2") {
		t.Fatalf("refsOf %v", got)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
