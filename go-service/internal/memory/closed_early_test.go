package memory

import (
	"strings"
	"testing"
)

// 1.4 P4: the model closed the response object too early and wrote its
// remaining keys after a comma. The rest is read; a text that would lose or
// replace what the first object holds, or items after a closed array, is not.
func TestParseReadsRestOfEarlyClosedObject(t *testing.T) {
	has := func(notes []string, s string) bool {
		for _, n := range notes {
			if strings.Contains(n, s) {
				return true
			}
		}
		return false
	}
	// object closed, then more keys
	out, notes := Parse(`{"memories":[{"id":"m1","kind":"event","body":"가람이 왔다"},{"id":"m2","kind":"event","body":"다온이 떠났다"}]},"scenes":[{"id":"s1","change":"open"}],"links":[{"from":"m1","rel":"causes","to":"m2"}]}`)
	if len(out.Memories) != 2 || len(out.Scenes) != 1 || len(out.Links) != 1 || !has(notes, "read as its rest") || has(notes, "was not read") {
		t.Fatalf("keys after the object: %+v notes %v", out, notes)
	}
	// with a code fence around it
	out, notes = Parse("```json\n" + `{"memories":[{"id":"m1","kind":"event","body":"가람이 왔다"}]},"scenes":[{"ref":"S1","change":"continue"}]}` + "\n```")
	if len(out.Memories) != 1 || len(out.Scenes) != 1 || !has(notes, "read as its rest") {
		t.Fatalf("fenced: %+v notes %v", out, notes)
	}
	// items written after a closed array: the array they belong to is not
	// known, so they are not read (they would land in the last array)
	out, notes = Parse(`{"memories":[{"id":"m1","kind":"event","body":"a"}]},{"ref":"S2","change":"close"}]}`)
	if len(out.Memories) != 1 || len(out.Scenes) != 0 || has(notes, "read as its rest") || !has(notes, "was not read") {
		t.Fatalf("items after an array: %+v notes %v", out, notes)
	}
	// a text that writes an array again with fewer items would replace it: not read
	out, notes = Parse(`{"memories":[{"id":"m1","kind":"event","body":"a"},{"id":"m2","kind":"event","body":"b"}]},"memories":[{"id":"m3","kind":"event","body":"c"}]}`)
	if len(out.Memories) != 2 || out.Memories[1].ID != "m2" || !has(notes, "was not read") {
		t.Fatalf("replacing text was read: %+v notes %v", out, notes)
	}
	// the same key again with as many items, or a plain value again: not read
	out, notes = Parse(`{"memories":[{"id":"m1","kind":"event","body":"a"}]},"memories":[{"id":"m2","kind":"event","body":"b"}]}`)
	if len(out.Memories) != 1 || out.Memories[0].ID != "m1" || has(notes, "read as its rest") {
		t.Fatalf("same-size array replaced: %+v notes %v", out, notes)
	}
	out, notes = Parse(`{"elapsed_days":2,"memories":[{"id":"m1","kind":"event","body":"a"}]},"elapsed_days":9}`)
	if out.ElapsedDays != 2 || has(notes, "read as its rest") {
		t.Fatalf("plain value replaced: %+v notes %v", out, notes)
	}
	// prose after the object stays unread as before
	out, notes = Parse(`{"memories":[{"id":"m1","kind":"event","body":"a"}]} 끝. {x}`)
	if len(out.Memories) != 1 || has(notes, "read as its rest") || !has(notes, "was not read") {
		t.Fatalf("prose: %+v notes %v", out, notes)
	}
}
