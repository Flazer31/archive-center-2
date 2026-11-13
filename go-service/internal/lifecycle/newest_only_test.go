package lifecycle

import "testing"

// Only the newest reply value is kept: the replaced reply stays an empty marker, so it keeps its
// turn and its text number.

// A reply rerolled away, then the input cut from the front, then a swipe
// back to the old reply: it returns to its own turn, not the greeting.
func TestReplacedReplyReturnsToItsTurnAfterFrontCut(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "first")))
	id := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a2", "g2", "second")))
	s.Apply(snap(SourceLoad, c("a2", "g2", "second")))
	s.Apply(snap(SourceLoad, c("a1", "g1", "first")))
	owner := s.respTurn["c:a1"]
	if owner == nil || owner.Greeting || owner.ID != id {
		t.Fatalf("the old reply came back on turn %+v, want turn %d", owner, id)
	}
	if r := s.respRow["c:a1"]; r.State != Active || r.Revisions[len(r.Revisions)-1].Data != "first" {
		t.Fatalf("the old reply after the swipe back: %+v", r)
	}
}

// A reply replaced by a new reply whose text is not read yet, then a
// restart: the next text number is not one handed out before.
func TestTextNumbersGoOnAfterRestart(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "first")))
	before := s.respRow["c:a1"].Revisions[0].Seq
	s.Apply(snap(SourceBeforeRequest, u("u1", "a")))
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), Row{ChatID: "a2", Role: "char", GenerationID: "g2", Unreadable: true}))
	r := Restore("chat-1", s.Turns, nil)
	r.Apply(snap(SourceLoad, u("u1", "a"), c("a2", "g2", "first")))
	got := r.respRow["c:a2"]
	if got == nil || len(got.Revisions) != 1 || got.Revisions[0].Seq <= before {
		t.Fatalf("text number after restart %+v, the replaced reply had %d", got, before)
	}
}

// Going back to an earlier reply is classified as before 1.1: the reply it
// leaves is deleted and, as any deleted reply, keeps its text (reviews
// 11-11: treating it as a reroll changed the identity rules and broke front
// cuts and multi-part replies; reverted). Plain and partial deletes keep
// their texts, also after the next input.
func TestDeletedRepliesKeepTheirTexts(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "A")))
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a2", "g2", "B")))
	s.Apply(snap(SourceLoad, u("u1", "a"), c("a1", "g1", "A")))
	goOn(s, u("u1", "a"), c("a1", "g1", "A"))
	if b := s.respRow["c:a2"]; b.State != Deleted || b.Revisions[len(b.Revisions)-1].Data != "B" {
		t.Fatalf("the reply left by going back: %+v", b)
	}
	if a := s.respRow["c:a1"]; a.State != Active || a.Revisions[len(a.Revisions)-1].Data != "A" {
		t.Fatalf("the chosen reply: %+v", a)
	}
	d := NewSession("chat-2")
	d.Apply(snap(SourceOutputEvent, u("u1", "a"), c("p1", "g1", "part A"), c("p2", "g2", "part B")))
	d.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("p1", "g1", "part A")))
	goOn(d, u("u1", "a"), c("p1", "g1", "part A"))
	if b := d.respRow["c:p2"]; b.State != Deleted || b.Revisions[0].Data != "part B" {
		t.Fatalf("a part only deleted must keep its text: %+v", b)
	}
	p := NewSession("chat-3")
	p.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "A")))
	p.Apply(snap(SourceBeforeRequest, u("u1", "a"), u("u2", "b")))
	if x := p.respRow["c:a1"]; x.State != Deleted || x.Revisions[0].Data != "A" {
		t.Fatalf("a plain delete must keep its text: %+v", x)
	}
}

// A part cut from the front that a later snapshot counts as replaced (an
// older misreading, also in f5486f7: a new part arrives after the cut) keeps
// its text; only the newest-value rule must not lose kept content.
func TestCutPartKeepsTextWhenMiscountedReplaced(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("p1", "g0", "P"), c("a1", "g1", "A"), u("u2", "b"), c("c1", "h1", "C")))
	s.Apply(snap(SourceLoad, c("a1", "g1", "A"), u("u2", "b"), c("c1", "h1", "C")))
	s.Apply(snap(SourceLoad, c("a1", "g1", "A"), c("d1", "g2", "D"), u("u2", "b"), c("c1", "h1", "C")))
	p := s.respRow["c:p1"]
	if p == nil || len(p.Revisions) == 0 || p.Revisions[len(p.Revisions)-1].Data != "P" {
		t.Fatalf("the cut part lost its text: %+v", p)
	}
	if _, ok := s.TakePruned().Cleared["c:p1"]; ok {
		t.Fatal("the cut part is listed to be emptied in storage")
	}
}

// Only the reply replaced just now is emptied. Replaced rows that still hold
// a text (history from before 1.1, or a cut part an older misreading counts
// as replaced) cannot be told apart and are left as they are, on the turn
// rerolled or edited and on other turns (cleaning stored history is a
// separate decision).
func TestOnlyTheReplyReplacedNowIsEmptied(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "one"), u("u2", "b"), c("b1", "h1", "two")))
	t1, t2 := s.ActiveTurns()[0], s.ActiveTurns()[1]
	t1.Responses = append([]*ResponseRow{{Key: "c:old1", State: Replaced, Revisions: []Revision{{Seq: 90, Data: "legacy one"}}}}, t1.Responses...)
	t2.Responses = append([]*ResponseRow{{Key: "c:old2", State: Replaced, Revisions: []Revision{{Seq: 91, Data: "legacy two"}}}}, t2.Responses...)
	r := Restore("chat-1", s.Turns, nil)
	// turn 1 rerolled, turn 2 edited in place
	r.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a3", "g3", "three"), u("u2", "b"), c("b1", "h1", "two edited")))
	if a := r.respRow["c:a1"]; a.State != Replaced || a.Revisions[0].Data != "" {
		t.Fatalf("the reply replaced now kept its text: %+v", a)
	}
	if old := r.respRow["c:old1"]; old.Revisions[0].Data != "legacy one" {
		t.Fatalf("history on the rerolled turn was emptied: %+v", old)
	}
	if old := r.respRow["c:old2"]; old.Revisions[0].Data != "legacy two" {
		t.Fatalf("history on the edited turn was emptied: %+v", old)
	}
	p := r.TakePruned()
	if _, ok := p.Cleared["c:old1"]; ok || p.Cleared["c:a1"] == 0 || p.Trimmed["c:b1"] == 0 {
		t.Fatalf("pruned %+v", p)
	}
}

// The same misreading when the front cut and the new part come in one
// snapshot: the part cut keeps its text.
func TestCutPartKeepsTextWhenCutAndNewPartTogether(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("p1", "g0", "P"), c("a1", "g1", "A"), u("u2", "b"), c("c1", "h1", "C")))
	s.Apply(snap(SourceLoad, c("a1", "g1", "A"), c("d1", "g2", "D"), u("u2", "b"), c("c1", "h1", "C")))
	p := s.respRow["c:p1"]
	if p == nil || len(p.Revisions) == 0 || p.Revisions[len(p.Revisions)-1].Data != "P" {
		t.Fatalf("the cut part lost its text: %+v", p)
	}
	if _, ok := s.TakePruned().Cleared["c:p1"]; ok {
		t.Fatal("the cut part is listed to be emptied in storage")
	}
}
