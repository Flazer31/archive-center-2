package store

import "testing"

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
