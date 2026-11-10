package memory

import (
	"reflect"
	"strings"
	"testing"
)

// An empty object inside a cut-off item (a basis written as {}) is part of
// that item, not a later response: the complete item before it and the day
// delta are kept, and the lost item is noted as other cut-off answers are.
func TestParseEmptyObjectInsideACutItemIsNotTheResponse(t *testing.T) {
	raw := `{"elapsed_days":2,"memories":[{"id":"m1","kind":"fact","body":"보존할 사실"},{"id":"m2","kind":"view","basis":{},"body":"잘린`
	out, notes := Parse(raw)
	if len(out.Memories) != 1 || out.Memories[0].ID != "m1" || out.Memories[0].Body != "보존할 사실" || out.ElapsedDays != 2 {
		t.Fatalf("out %+v notes %v", out, notes)
	}
	j := strings.Join(notes, " ")
	if !strings.Contains(j, "response JSON is incomplete") || !strings.Contains(j, "memories: an incomplete item was dropped") {
		t.Fatalf("notes %v", notes)
	}
	// a complete response after a draft cut off inside a string is still
	// the response
	draft := `{"memories":[{"id":"m1","body":"초안 final: {"memories":[{"id":"m1","kind":"fact","body":"최종"}]}`
	if out, notes := Parse(draft); len(out.Memories) != 1 || out.Memories[0].Body != "최종" {
		t.Fatalf("draft: out %+v notes %v", out, notes)
	}
}

// One element of the wrong shape in an item's list leaves out that element
// only; the well-formed elements are kept and the element is named.
func TestSalvageKeepsTheWellFormedElementsOfAList(t *testing.T) {
	out, notes := Parse(`{"memories":[{"id":"m1","kind":"event","body":"약속을 이행했다","effects":[{"about":"M10","status":"fulfilled"},{"about":"N1","slot":"돈","value":[100]}]}]}`)
	if len(out.Memories) != 1 || len(out.Memories[0].Effects) != 1 || out.Memories[0].Effects[0].About != "M10" || out.Memories[0].Effects[0].Status != "fulfilled" {
		t.Errorf("effects: out %+v notes %v", out, notes)
	}
	if !strings.Contains(strings.Join(notes, " "), "fields of the wrong shape were left out of an item: effects[1]") {
		t.Errorf("effects notes %v", notes)
	}
	out, notes = Parse(`{"memories":[{"id":"m1","kind":"fact","body":"금고의 비밀번호는 1234다","hide_from":["N2",17]}]}`)
	if len(out.Memories) != 1 || !reflect.DeepEqual([]string(out.Memories[0].HideFrom), []string{"N2"}) || out.Memories[0].Body != "금고의 비밀번호는 1234다" {
		t.Fatalf("hide_from: out %+v notes %v", out, notes)
	}
	if !strings.Contains(strings.Join(notes, " "), "fields of the wrong shape were left out of an item: hide_from[1]") {
		t.Fatalf("hide_from notes %v", notes)
	}
}

// A place written on a complete event reads as roles.place whether the
// response is whole or cut off after it.
func TestSalvageReadsAPlaceBesideRoles(t *testing.T) {
	whole, _ := Parse(`{"memories":[{"id":"m1","kind":"event","body":"도착했다","place":"N12"}]}`)
	cut, notes := Parse(`{"memories":[{"id":"m1","kind":"event","body":"도착했다","place":"N12"},{"id":"m2","body":"잘린`)
	if len(whole.Memories) != 1 || whole.Memories[0].Roles == nil || !reflect.DeepEqual(whole.Memories[0].Roles.Place, []string{"N12"}) {
		t.Fatalf("whole %+v", whole)
	}
	if len(cut.Memories) != 1 || !reflect.DeepEqual(cut.Memories[0], whole.Memories[0]) {
		t.Fatalf("cut %+v notes %v", cut, notes)
	}
	if !strings.Contains(strings.Join(notes, " "), "memories: 1 place(s) written beside roles were read as roles.place") {
		t.Fatalf("notes %v", notes)
	}
}

// Effects are numbered by position (m1.2); leaving out a malformed effect
// must not move a kept one to another number, so a reference to m1.2 never
// reaches a different effect.
func TestSalvageKeepsEffectNumbers(t *testing.T) {
	value := map[string]string{"위치": "서울", "직업": "상인"}
	bad := `{"about":"N1","slot":"돈","value":[100]}`
	for _, tc := range []struct {
		name    string
		list    string
		written []string // the slot written at each position; "" for the malformed one
		kept    int      // the well-formed effects before the malformed one
	}{
		{"first", bad + `,{"about":"N1","slot":"위치","value":"서울"},{"about":"N1","slot":"직업","value":"상인"}`, []string{"", "위치", "직업"}, 0},
		{"middle", `{"about":"N1","slot":"위치","value":"서울"},` + bad + `,{"about":"N1","slot":"직업","value":"상인"}`, []string{"위치", "", "직업"}, 1},
		{"last", `{"about":"N1","slot":"위치","value":"서울"},{"about":"N1","slot":"직업","value":"상인"},` + bad, []string{"위치", "직업", ""}, 2},
	} {
		out, notes := Parse(`{"memories":[{"id":"m1","kind":"event","body":"거래 후 이동했다","effects":[` + tc.list + `]},{"id":"m2","kind":"fact","about":"N1","slot":"위치","body":"부산으로 이동했다","was":"m1.2"}]}`)
		if len(out.Memories) != 2 || out.Memories[1].Was[0] != "m1.2" {
			t.Fatalf("%s: out %+v notes %v", tc.name, out, notes)
		}
		effects := out.Memories[0].Effects
		if len(effects) != tc.kept {
			t.Fatalf("%s: kept %d effects, want %d: %+v", tc.name, len(effects), tc.kept, effects)
		}
		for j, e := range effects {
			if e.Slot != tc.written[j] || e.Value != value[e.Slot] {
				t.Errorf("%s: effect %d is %+v, written there was slot %q (notes %v)", tc.name, j+1, e, tc.written[j], notes)
			}
		}
		if !strings.Contains(strings.Join(notes, " "), "fields of the wrong shape were left out of an item: effects") {
			t.Errorf("%s: notes %v", tc.name, notes)
		}
	}
}

// A null roles is an empty set: a place beside it is read as roles.place,
// whole or cut off, without a panic.
func TestPlaceBesideNullRoles(t *testing.T) {
	item := `{"id":"m1","kind":"event","body":"도착했다","roles":null,"place":"N12"}`
	for _, raw := range []string{`{"memories":[` + item + `]}`, `{"memories":[` + item + `,{"id":"m2","body":"잘린`} {
		out, notes := Parse(raw)
		if len(out.Memories) != 1 || out.Memories[0].Roles == nil || !reflect.DeepEqual(out.Memories[0].Roles.Place, []string{"N12"}) {
			t.Fatalf("%s: out %+v notes %v", raw, out, notes)
		}
	}
}

// A complete response written again after an unfinished draft whose JSON
// still reads to the end is the response (only an empty object inside the
// draft is not).
func TestRestartedResponseAfterAnOpenDraft(t *testing.T) {
	out, notes := Parse("{\"memories\":[\n{\"memories\":[{\"id\":\"m1\",\"kind\":\"fact\",\"body\":\"최종 사실\"}]}")
	if len(out.Memories) != 1 || out.Memories[0].Body != "최종 사실" {
		t.Fatalf("out %+v notes %v", out, notes)
	}
}

// An empty object written after an unfinished draft (not as a key's value)
// is the response, as before: nothing new this turn, the draft is not read.
// An empty object that is a key's value inside a cut-off item stays part of
// that item.
func TestEmptyFinalAfterAnOpenDraft(t *testing.T) {
	for _, final := range []string{"{}", "{ }"} {
		out, notes := Parse("{\"memories\":[{\"id\":\"draft\",\"kind\":\"fact\",\"body\":\"최종에서 버릴 초안\"},\n" + final)
		if len(out.Memories) != 0 {
			t.Errorf("%q: out %+v notes %v", final, out, notes)
		}
	}
	out, notes := Parse(`{"elapsed_days":2,"memories":[{"id":"m1","kind":"fact","body":"보존할 사실"},{"id":"m2","kind":"view","basis": { },"body":"잘린`)
	if len(out.Memories) != 1 || out.Memories[0].ID != "m1" || out.ElapsedDays != 2 {
		t.Errorf("basis with spaces: out %+v notes %v", out, notes)
	}
}
