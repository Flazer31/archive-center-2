package memory_test

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"encoding/json"
	"testing"
)

func r50(id, role, data, gen string) lifecycle.Row {
	return lifecycle.Row{ChatID: id, Role: role, Data: data, GenerationID: gen}
}
func pair50(prefix, q, a string) []lifecycle.Row {
	return []lifecycle.Row{r50(prefix+"u", "user", q, ""), r50(prefix+"a", "char", a, "")}
}
func mark50(parent, point, id string) lifecycle.Row {
	return lifecycle.Row{ChatID: id, Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::" + parent + "::name::" + point + "::}}"}
}
func apply50(s *lifecycle.Session, rows ...lifecycle.Row) {
	for i := range rows {
		rows[i].Index = i
	}
	s.Apply(lifecycle.Snapshot{SessionKey: s.Key, Source: lifecycle.SourceLoad, TargetIndex: -1, Rows: rows})
}
func session50(k string, rows ...lifecycle.Row) *lifecycle.Session {
	s := lifecycle.NewSession("chat:" + k)
	apply50(s, rows...)
	return s
}
func scope50(t *testing.T, s *lifecycle.Session, parents ...*lifecycle.Session) []memory.ScopedTurn {
	t.Helper()
	// the pipeline records a branch's copies at the observation (the
	// parents first: each was recorded when it was observed)
	load := func(k string) (*lifecycle.Session, bool, error) {
		for _, p := range append(parents, s) {
			if p.Key == k {
				return p, true, nil
			}
		}
		return nil, false, nil
	}
	for _, x := range append(append([]*lifecycle.Session{}, parents...), s) {
		if x.BranchOf[0] != "" && x.CopiesOf != x.BranchOf {
			_, e := memory.RecordCopies(x, load)
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	out := memory.ScopeOf(s)
	b, _ := json.Marshal(out)
	t.Logf("parent=%v scope=%s", s.BranchOf, b)
	return out
}

func TestA50NormalPrefixAndNewTurn(t *testing.T) {
	p := session50("P", append(pair50("p1", "q1", "a1"), pair50("p2", "q2", "a2")...)...)
	rows := append(pair50("c1", "q1", "a1"), pair50("c2", "q2", "a2")...)
	rows = append(rows, mark50("P", "p2a", "m"))
	rows = append(rows, pair50("c3", "q3", "a3")...)
	c := session50("C", rows...)
	v := scope50(t, c, p)
	if len(v) != 3 || !v[0].Inherited || !v[1].Inherited || v[2].Inherited {
		t.Fatal("normal prefix/new turn")
	}
}
func TestA50FirstMismatchLeavesSuffixOwn(t *testing.T) {
	p := session50("P", append(pair50("p1", "q1", "a1"), pair50("p2", "q2", "a2")...)...)
	rows := append(pair50("c1", "q1", "edited"), pair50("c2", "q2", "a2")...)
	c := session50("C", append(rows, mark50("P", "p2a", "m"))...)
	v := scope50(t, c, p)
	if len(v) != 2 || v[0].Inherited || v[1].Inherited {
		t.Fatal("must stop at first mismatch")
	}
}
func TestA50UniqueFrontDeletionIsOwn(t *testing.T) {
	p := session50("P", append(pair50("p1", "q1", "a1"), pair50("p2", "q2", "a2")...)...)
	c := session50("C", append(pair50("c2", "q2", "a2"), mark50("P", "p2a", "m"))...)
	v := scope50(t, c, p)
	if len(v) != 1 || v[0].Inherited {
		t.Fatal("unique deletion should not map to first parent")
	}
}
func TestA50RepeatFrontDeletionMustNotBorrowFirstOccurrence(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "first_observation", true: "previously_observed"}[known], func(t *testing.T) {
			p := session50("P", append(pair50("p1", "same q", "same a"), pair50("p2", "same q", "same a")...)...)
			c := session50("C")
			if known {
				rows := append(pair50("c1", "same q", "same a"), pair50("c2", "same q", "same a")...)
				apply50(c, append(rows, mark50("P", "p2a", "m"))...)
				scope50(t, c, p)
			}
			apply50(c, append(pair50("c2", "same q", "same a"), mark50("P", "p2a", "m"))...)
			v := scope50(t, c, p)
			wrong := memory.Own(p)[0].Source.Key()
			correct := memory.Own(p)[1].Source.Key()
			t.Logf("first_source=%s actual_copy_source=%s selected_source=%s", wrong, correct, v[0].Source.Key())
			if v[0].Inherited && v[0].Source.Key() == wrong {
				t.Fatal("copied second occurrence is linked to first occurrence")
			}
		})
	}
}
func TestA50OneSharedWholeTurnOnlyCostsReuse(t *testing.T) {
	p := session50("P", append(pair50("p1", "q1", "a1"), pair50("p2", "q2", "a2")...)...)
	rows := append(pair50("c1", "q1", "a1"), pair50("p2", "q2", "a2")...)
	c := session50("C", append(rows, mark50("P", "p2a", "m"))...)
	v := scope50(t, c, p)
	if v[0].Inherited || !v[1].Inherited || v[1].Source.Turn != 2 {
		t.Fatal("mixed whole-key mode")
	}
}
func TestA50OneSharedRowMustNotMapToOtherTurn(t *testing.T) {
	p := session50("P", append(pair50("p1", "same q", "same a"), pair50("p2", "same q", "same a")...)...)
	c := session50("C", r50("p2u", "user", "same q", ""), r50("new-a", "char", "same a", ""), mark50("P", "p2a", "m"))
	v := scope50(t, c, p)
	if v[0].Inherited && v[0].Source.Turn != 2 {
		t.Fatal("shared user row p2u linked to P turn 1 because whole key bundle differs")
	}
}
func TestA50ParentInputEditKeepsTheRecordedSource(t *testing.T) {
	p := session50("P", pair50("p1", "old q", "answer")...)
	c := session50("C", append(pair50("c1", "old q", "answer"), mark50("P", "p1a", "m"))...)
	before := scope50(t, c, p)
	apply50(p, pair50("p1", "edited q", "answer")...)
	after := scope50(t, c, p)
	// the copy keeps the parent's source it copied (recorded when the branch
	// was first seen): the parent's later edit does not rewrite it
	if !before[0].Inherited || !after[0].Inherited || after[0].Source.Key() != before[0].Source.Key() || after[0].Source.Members[0].Data != "old q" {
		t.Fatal("parent input edit handling")
	}
}
func TestA50MissingBranchPointLeavesChildOwn(t *testing.T) {
	p := session50("P", pair50("p1", "q", "a")...)
	c := session50("C", append(pair50("c1", "q", "a"), mark50("P", "p1a", "m"))...)
	apply50(p, r50("p1u", "user", "q", ""))
	v := scope50(t, c, p)
	if v[0].Inherited {
		t.Fatal("missing branch point")
	}
}
func TestA50LastMarkerThenAncestorOnlyMustNotReparent(t *testing.T) {
	a := session50("A", pair50("a1", "q1", "a1")...)
	bRows := append(pair50("b1", "q1", "a1"), mark50("A", "a1a", "ma"))
	bRows = append(bRows, pair50("b2", "q2", "b2")...)
	b := session50("B", bRows...)
	gRows := append(pair50("g1", "q1", "a1"), mark50("A", "a1a", "old"))
	gRows = append(gRows, pair50("g2", "q2", "b2")...)
	g := session50("G", append(append([]lifecycle.Row{}, gRows...), mark50("B", "b2a", "last"))...)
	before := scope50(t, g, a, b)
	apply50(g, gRows...)
	after := scope50(t, g, a, b)
	t.Logf("before_second=%s after_second=%s", before[1].Source.Session, after[1].Source.Session)
	if g.BranchOf[0] != "chat:B" {
		t.Fatal("known direct parent B overwritten by copied ancestor marker A")
	}
}
func TestA50NoMarkerRetainsKnownParentAndLateMarkerCorrects(t *testing.T) {
	rows := pair50("g1", "q", "a")
	g := session50("G", append(append([]lifecycle.Row{}, rows...), mark50("A", "a1a", "old"))...)
	apply50(g, append(append(append([]lifecycle.Row{}, rows...), mark50("A", "a1a", "old")), mark50("B", "b1a", "last"))...)
	if g.BranchOf[0] != "chat:B" {
		t.Fatal("late direct marker not applied")
	}
	apply50(g, rows...)
	if g.BranchOf[0] != "chat:B" {
		t.Fatal("all markers missing cleared parent")
	}
}
func TestA50UserForkLaterIdenticalAnswerIsChildOwn(t *testing.T) {
	p := session50("P", pair50("p1", "q", "same answer")...)
	c := session50("C", r50("c1u", "user", "q", ""), mark50("P", "p1u", "m"))
	initial := scope50(t, c, p)
	if initial[0].Inherited {
		t.Fatal("initial incomplete copy")
	}
	apply50(c, r50("c1u", "user", "q", ""), mark50("P", "p1u", "m"), r50("new-answer", "char", "same answer", "child-generation"))
	v := scope50(t, c, p)
	if v[0].Inherited {
		t.Fatal("answer generated after user-row fork linked to uncopied parent answer")
	}
}
func TestA50KnownDifferentGenerationDoesNotDisambiguateRepeatedTurn(t *testing.T) {
	rows := append(pair50("p1", "q", "a"), pair50("p2", "q", "a")...)
	rows[1].GenerationID = "gen1"
	rows[3].GenerationID = "gen2"
	p := session50("P", rows...)
	childRows := pair50("c2", "q", "a")
	childRows[1].GenerationID = "gen2"
	c := session50("C", append(childRows, mark50("P", "p2a", "m"))...)
	v := scope50(t, c, p)
	if v[0].Inherited && v[0].Source.Turn == 1 {
		t.Fatal("known gen2 still maps to gen1 occurrence through text fallback")
	}
}
func TestA50SameKeysPreserveChildOrder(t *testing.T) {
	p := session50("P", append(pair50("p1", "q1", "a1"), pair50("p2", "q2", "a2")...)...)
	rows := append(pair50("p1", "q1", "edited"), pair50("p2", "q2", "a2")...)
	c := session50("C", append(rows, mark50("P", "p2a", "m"))...)
	v := scope50(t, c, p)
	if v[0].Inherited || !v[1].Inherited || v[0].Source.Turn != 1 {
		t.Fatal("child order changed")
	}
}
func TestA50GrandchildPointOnInheritedLocalRow(t *testing.T) {
	a := session50("A", pair50("a1", "q1", "a1")...)
	bRows := append(pair50("b1", "q1", "a1"), mark50("A", "a1a", "ma"))
	b := session50("B", bRows...)
	g := session50("G", append(pair50("g1", "q1", "a1"), mark50("B", "b1a", "mb"))...)
	v := scope50(t, g, a, b)
	if len(v) != 1 || !v[0].Inherited || v[0].Source.Session != a.Key {
		t.Fatal("inherited local branch coordinate lost")
	}
}
func TestA50ParentOldResponseRevisionRetained(t *testing.T) {
	p := session50("P", pair50("p1", "q", "old answer")...)
	old := memory.Own(p)[0].Source.Key()
	c := session50("C", append(pair50("c1", "q", "old answer"), mark50("P", "p1a", "m"))...)
	apply50(p, pair50("p1", "q", "new answer")...)
	v := scope50(t, c, p)
	if !v[0].Inherited || v[0].Source.Key() != old {
		t.Fatal("old response revision lost")
	}
}
