package memory

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseLinksInMemories(t *testing.T) {
	const items = `"memories":[
 {"id":"l1","from":"m1","rel":"causes","to":"m2","q":"종이 울려 문을 열었다",
  "knows":[{"who":"n1","via":"직접","from":"n2","extent":"부분","part":"종소리","ref":"m1","q":"종이 울렸다"}],
  "unaware":["n3"],"hide_from":["n4"],"allow_to":["n5"]},
 {"id":"m1","kind":"event","body":"종이 울렸다"},
 {"id":"l2","from":"m2","rel":"recalls","to":"m1","q":"종소리를 떠올렸다"}]`
	want := []Link{
		{ID: "l1", From: "m1", Rel: "causes", To: "m2", Q: "종이 울려 문을 열었다", Awareness: Awareness{
			Knows:   []Knows{{Who: "n1", Via: "직접", From: "n2", Extent: "부분", Part: "종소리", Ref: "m1", Q: "종이 울렸다"}},
			Unaware: Who{"n3"}, HideFrom: Who{"n4"}, AllowTo: Who{"n5"},
		}},
		{ID: "l2", From: "m2", Rel: "recalls", To: "m1", Q: "종소리를 떠올렸다"},
	}
	for _, tc := range []struct{ name, raw string }{
		{"complete", `{` + items + `}`},
		{"cut", `{` + items + `,"scenes":[{"id":"s1","change":`},
		{"shape_error", `{"entities":[{"id":"n1","name":17}],` + items + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, notes := Parse(tc.raw)
			if !reflect.DeepEqual(out.Links, want) || !reflect.DeepEqual(out.Memories, []Memory{{ID: "m1", Kind: "event", Body: "종이 울렸다"}}) {
				t.Fatalf("out %+v notes %v", out, notes)
			}
			if !strings.Contains(strings.Join(notes, " "), "a link written in memories was read as a link") {
				t.Fatalf("missing diagnostic: %v", notes)
			}
		})
	}
}

func TestParseLinkFieldsKeepMemory(t *testing.T) {
	const items = `"memories":[
 {"id":"m1","kind":"fact","from":"m2","rel":"causes","to":"m3"},
 {"id":"m2","body":"문이 열렸다","from":"m1","rel":"causes"},
 {"id":"m3","kind":"","from":"m1","rel":"causes"},
 {"id":"m4","body":"","from":"m1","rel":"causes"},
 {"id":"m5","from":"m1"},
 {"id":"m6","rel":"causes"}]`
	want := []Memory{{ID: "m1", Kind: "fact", To: Who{"m3"}}, {ID: "m2", Body: "문이 열렸다"}, {ID: "m3"}, {ID: "m4"}, {ID: "m5"}, {ID: "m6"}}
	for _, tail := range []string{`}`, `,"scenes":[{"id":`} {
		out, notes := Parse(`{` + items + tail)
		if len(out.Links) != 0 || !reflect.DeepEqual(out.Memories, want) {
			t.Fatalf("out %+v notes %v", out, notes)
		}
	}
}

func TestParseLinksInMemoriesOrder(t *testing.T) {
	const items = `"memories":[{"id":"l2","from":"m1","rel":"causes","to":"m2"},{"id":"l3","from":"l2","rel":"recalls"}],"links":[{"id":"l1","from":"m2","rel":"recalls","to":"m1"}]`
	for _, tail := range []string{`}`, `,"scenes":[{"id":`} {
		out, notes := Parse(`{` + items + tail)
		var ids []string
		for _, l := range out.Links {
			ids = append(ids, l.ID)
		}
		if len(out.Memories) != 0 || strings.Join(ids, ",") != "l1,l2,l3" || out.Links[2].To != "" {
			t.Fatalf("out %+v notes %v", out, notes)
		}
	}
}
