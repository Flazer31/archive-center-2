package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

func longReply(lines, width int) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		b.WriteString(strings.Repeat("가", width))
		b.WriteString("\n")
	}
	return b.String()
}

func TestReplyPartsSplitsOnlyLongReplies(t *testing.T) {
	short := Source{Responses: []SourceResponse{{Data: strings.Repeat("a", PartChars-20)}}}
	if got := ReplyParts(short); len(got) != 1 || got[0] != "REPLY: "+short.Responses[0].Data+"\n" {
		t.Fatalf("a reply within PartChars is one part, got %d", len(got))
	}
	// 45,000+ chars in two rows: three parts, none over PartChars, cut at
	// a line end, nothing lost or repeated
	src := Source{Responses: []SourceResponse{{Data: longReply(300, 99)}, {Data: longReply(150, 99)}}}
	whole := "REPLY: " + src.Responses[0].Data + "\nREPLY: " + src.Responses[1].Data + "\n"
	parts := ReplyParts(src)
	if len(parts) != 3 {
		t.Fatalf("parts %d, want 3 for %d chars", len(parts), len([]rune(whole)))
	}
	if strings.Join(parts, "") != whole {
		t.Fatal("the parts do not join back into the reply")
	}
	for i, p := range parts {
		if n := len([]rune(p)); n > PartChars || n < PartChars/2 {
			t.Fatalf("part %d has %d chars", i+1, n)
		}
		if i < len(parts)-1 && !strings.HasSuffix(p, "\n") {
			t.Fatalf("part %d is not cut at a line end", i+1)
		}
	}
	// one long line: cut where it must be
	one := Source{Responses: []SourceResponse{{Data: strings.Repeat("b", 3*PartChars)}}}
	got := ReplyParts(one)
	if len(got) != 4 || strings.Join(got, "") != "REPLY: "+one.Responses[0].Data+"\n" {
		t.Fatalf("a reply without line ends: %d parts", len(got))
	}
}

func TestPartTextShowsInputFirstAndEarlierPartsLater(t *testing.T) {
	src := Source{Members: []SourceMember{{Data: "질문"}}}
	parts := []string{"REPLY: 하나\n", "둘\n"}
	first := PartText(src, parts, 0, Output{})
	if !strings.Contains(first, "USER: 질문") || !strings.Contains(first, "part 1 of 2") || strings.Contains(first, "EARLIER PARTS") {
		t.Fatalf("first part: %q", first)
	}
	second := PartText(src, parts, 1, Output{Memories: []Memory{{ID: "m1001", Kind: "fact", Body: "앞 기록"}}})
	if strings.Contains(second, "USER: 질문") || !strings.Contains(second, `"m1001"`) || !strings.HasSuffix(second, "둘\n") {
		t.Fatalf("later part: %q", second)
	}
}

func TestRenumberKeepsOtherIds(t *testing.T) {
	out := Output{
		Entities: []Entity{{ID: "n1", Name: "카렌"}},
		Memories: []Memory{
			{ID: "m1", Kind: "event", Body: "n1", Roles: &Roles{Actor: Refs{"n1", "N12", "n1001"}}},
			{ID: "m2", Kind: "fact", About: "n1", At: "m1", Was: Refs{"m1.2", "M40"}},
		},
		Scenes: []Scene{{ID: "s1", Change: "open"}},
		Links:  []Link{{From: "m1", Rel: "causes", To: "m2"}},
	}
	got := Renumber(out, 2)
	if got.Entities[0].ID != "n2001" || got.Memories[0].ID != "m2001" || got.Memories[1].ID != "m2002" || got.Scenes[0].ID != "s2001" {
		t.Fatalf("defined ids: %+v", got)
	}
	if a := got.Memories[0].Roles.Actor; len(a) != 3 || a[0] != "n2001" || a[1] != "N12" || a[2] != "n1001" {
		t.Fatalf("roles: %v (a KNOWN id and an earlier part's id stay)", a)
	}
	if m := got.Memories[1]; m.About != "n2001" || m.At != "m2001" || m.Was[0] != "m2001.2" || m.Was[1] != "M40" {
		t.Fatalf("references: %+v", m)
	}
	if l := got.Links[0]; l.From != "m2001" || l.To != "m2002" {
		t.Fatalf("link: %+v", l)
	}
	if out.Entities[0].ID != "n1" {
		t.Fatal("the answer given was changed")
	}
}

func TestMergePartsAddsDays(t *testing.T) {
	m := MergeParts([]Output{{ElapsedDays: 1, Memories: []Memory{{ID: "m1001"}}}, {ElapsedDays: 2, Memories: []Memory{{ID: "m2001"}}}})
	if m.ElapsedDays != 3 || len(m.Memories) != 2 || m.Memories[1].ID != "m2001" {
		t.Fatalf("merged %+v", m)
	}
}

// No part over PartChars (a cut pulled back to a line end must not leave
// the last part longer), and the limit is the reply's own text, not its
// "REPLY: " lines.
func TestReplyPartsStayWithinPartChars(t *testing.T) {
	src := Source{Responses: []SourceResponse{{Data: strings.Repeat("a", 16992) + "\n" + strings.Repeat("b", 22999)}}}
	parts := ReplyParts(src)
	if strings.Join(parts, "") != "REPLY: "+src.Responses[0].Data+"\n" {
		t.Fatal("the parts do not join back into the reply")
	}
	for i, p := range parts {
		if n := len([]rune(p)); n > PartChars {
			t.Fatalf("part %d of %d has %d chars", i+1, len(parts), n)
		}
	}
	exact := Source{Responses: []SourceResponse{{Data: strings.Repeat("c", PartChars)}}}
	if got := ReplyParts(exact); len(got) != 1 {
		t.Fatalf("a reply of exactly PartChars chars is one part, got %d", len(got))
	}
}

// An identity relation's id is renumbered with its references; names,
// quotes and bodies are text and stay as written.
func TestRenumberRelationsAndKeepsText(t *testing.T) {
	out := Output{
		Entities: []Entity{{ID: "n1", Name: "n1", Q: "n1", SameAs: "N3", How: "alias", Relation: "r1"}},
		Memories: []Memory{{ID: "m1", Kind: "view", Owner: "n1", Body: "m1.txt n1", Q: "n1", Basis: &Basis{Ref: "r1", From: "n1"}}},
	}
	got := Renumber(out, 3)
	e, m := got.Entities[0], got.Memories[0]
	if e.ID != "n3001" || e.Relation != "r3001" || e.SameAs != "N3" || e.Name != "n1" || e.Q != "n1" {
		t.Fatalf("entity %+v", e)
	}
	if m.Owner != "n3001" || m.Basis.Ref != "r3001" || m.Basis.From != "n3001" || m.Body != "m1.txt n1" || m.Q != "n1" {
		t.Fatalf("memory %+v basis %+v", m, m.Basis)
	}
}

// A long reply's later part: the child and the people who learn the father
// are ids too, renumbered with the names.
func TestRenumberKeepsTheBirthRefs(t *testing.T) {
	var out Output
	if err := json.Unmarshal([]byte(`{"entities":[{"id":"n1","name":"리나","type":"person"},{"id":"n2","name":"하늘","type":"person"}],`+
		`"body_events":[{"kind":"paternity_learned","who":"n1","child":"n2","paternity":{"status":"confirmed","candidates":["n2"],"unaware":["n1"],"learns":["n1"]}}]}`), &out); err != nil {
		t.Fatal(err)
	}
	got := Renumber(out, 1)
	e := got.BodyEvents[0]
	n1, n2 := got.Entities[0].ID, got.Entities[1].ID
	if n1 == "n1" || e.Who != n1 || e.Child != n2 || e.Paternity.Learns[0] != n1 || e.Paternity.Unaware[0] != n1 || e.Paternity.Candidates[0] != n2 {
		t.Fatalf("renumbered %+v %+v", got.Entities, e)
	}
}
