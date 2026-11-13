package store

import (
	"reflect"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// The current facts of a name the input gives that the window does not hold
// in every turn go first, in rank order; everything else keeps its rank
// order (1.1 B1). With no window turn there is nothing to compare with, and
// nothing moves.
func TestEnteringFirst(t *testing.T) {
	k := memory.Known{
		Names: []memory.KnownName{{ID: "N1", Name: "리나"}, {ID: "N2", Name: "도윤"}, {ID: "N3", Name: "약사 리나"}, {ID: "N4", Name: "칠흑의 검"}},
		// N3 is recorded as the same person as N1
		Relations: []memory.KnownRelation{{Name: "N3", SameAs: "N1"}},
	}
	mem := func(id, kind, about string, to ...string) candidate {
		m := &memory.KnownMemory{ID: id}
		m.Kind, m.About, m.To = kind, about, to
		return candidate{id: id, mem: m}
	}
	order := []candidate{
		mem("M1", "fact", "N2"),       // someone else
		mem("M2", "event", "N1"),      // an event, not a current fact
		mem("M3", "fact", "N1"),       // about the entering name
		mem("M4", "fact", "N2", "N3"), // to the entering name (by its other name)
		mem("M5", "fact", "N1"),       // a completing fact: waits for its event
		mem("M6", "fact", "N4"),       // another name, not in the input
	}
	parent := map[string]string{"M5": "M2"}
	turn := func(text string) memory.ScopedTurn {
		return memory.ScopedTurn{Source: memory.Source{Members: []memory.SourceMember{{Data: text}}, Responses: []memory.SourceResponse{{Data: "답"}}}}
	}
	ids := func(cs []candidate) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.id)
		}
		return out
	}
	rankOrder := []string{"M1", "M2", "M3", "M4", "M5", "M6"}
	for _, tc := range []struct {
		name   string
		input  string
		window []memory.ScopedTurn
		want   []string
	}{
		{"coming back", "리나가 돌아왔다", []memory.ScopedTurn{turn("리나와 걸었다"), turn("비가 왔다")}, []string{"M3", "M4", "M1", "M2", "M5", "M6"}},
		{"by its other name", "약사 리나에게 묻는다", []memory.ScopedTurn{turn("비가 왔다")}, []string{"M3", "M4", "M1", "M2", "M5", "M6"}},
		{"not in the input", "비가 그쳤다", []memory.ScopedTurn{turn("비가 왔다")}, rankOrder},
		// another name the input gives moves its own facts only
		{"the other name", "도윤이 웃었다", []memory.ScopedTurn{turn("비가 왔다")}, []string{"M1", "M4", "M2", "M3", "M5", "M6"}},
		{"held in every window turn", "리나가 웃었다", []memory.ScopedTurn{turn("리나와 걸었다"), turn("리나가 말했다")}, rankOrder},
		{"no window", "리나가 돌아왔다", nil, rankOrder},
		// the other name of the same person in the window holds her
		{"held by her other name", "리나가 웃었다", []memory.ScopedTurn{turn("약사 리나가 말했다")}, rankOrder},
		// a name inside another word is not a mention
		{"inside a word", "어린 리나는 없다", []memory.ScopedTurn{turn("비가 왔다")}, []string{"M3", "M4", "M1", "M2", "M5", "M6"}},
		{"inside a word only", "칠흑의검객이 왔다 어리나", []memory.ScopedTurn{turn("비가 왔다")}, rankOrder},
	} {
		got := ids(enteringFirst(append([]candidate(nil), order...), k, tc.window, tc.input, parent))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Mentions begin a word: particles may follow, letters may not come before.
func TestMentions(t *testing.T) {
	for _, tc := range []struct {
		text, name string
		want       bool
	}{
		{"린이 돌아왔다", "린", true},
		{"어린 아이가 왔다", "린", false},
		{"어린 아이와 린", "린", true},
		{"\"린\"이라고 불렀다", "린", true},
		{"칠흑의 검이 왔다", "칠흑의 검", true},
		{"", "린", false},
		{"Ann arrived", "Ann", true},
		{"Anna arrived", "Ann", false},
		{"Ann은 웃었다", "Ann", true},
		{"(Ann)", "Ann", true},
		{"Anna and Ann", "Ann", true},
		// a Hangul name may take a particle: a longer word that begins
		// with it still counts (the limit of not knowing particles)
		{"린넨을 샀다", "린", true},
		// no spaces to read next to Han, Hiragana or Katakana
		{"花子は笑った", "花子", true},
		{"そして、花子が来た", "花子", true},
		{"我看见小明", "小明", true},
		{"Annは笑った", "Ann", true},
		// an overlapping later occurrence is still found
		{"어린 린 린", "린 린", true},
	} {
		if got := mentions(tc.text, tc.name); got != tc.want {
			t.Errorf("mentions(%q, %q) = %v, want %v", tc.text, tc.name, got, tc.want)
		}
	}
}

// A cycle of same-as records ends (path compression keeps one root).
func TestEnteringFirstSameAsCycle(t *testing.T) {
	k := memory.Known{
		Names:     []memory.KnownName{{ID: "N1", Name: "리나"}, {ID: "N2", Name: "약사"}, {ID: "N3", Name: "왕녀"}},
		Relations: []memory.KnownRelation{{Name: "N1", SameAs: "N2"}, {Name: "N2", SameAs: "N3"}, {Name: "N3", SameAs: "N1"}},
	}
	m := &memory.KnownMemory{ID: "M1"}
	m.Kind, m.About = "fact", "N3"
	o := &memory.KnownMemory{ID: "M2"}
	o.Kind, o.About = "fact", "N9"
	got := enteringFirst([]candidate{{id: "M2", mem: o}, {id: "M1", mem: m}}, k, []memory.ScopedTurn{{Source: memory.Source{Members: []memory.SourceMember{{Data: "비"}}}}}, "리나가 왔다", nil)
	if got[0].id != "M1" {
		t.Fatalf("order %v", []string{got[0].id, got[1].id})
	}
}
