package memory

import (
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
)

// The branch's copies are recorded once, when the branch is first seen with
// its parent (user decision 10-23, option B); later deletions, edits and
// rerolls do not move a copy to another parent turn (ASTRA-RECHECK-52).

func loaderOf(sessions ...*lifecycle.Session) Loader {
	return func(k string) (*lifecycle.Session, bool, error) {
		for _, s := range sessions {
			if s.Key == k {
				return s, true, nil
			}
		}
		return nil, false, nil
	}
}

func record(t *testing.T, s *lifecycle.Session, load Loader) {
	t.Helper()
	ok, err := RecordCopies(s, load)
	if err != nil || !ok {
		t.Fatalf("record %v %v", ok, err)
	}
}

// New ids (PocketRisu): the child deletes a copy after the copies were
// recorded; the other copies keep their parent turns. (The deleted copy is
// the middle one: the chat's first rows gone are a front cut and stay,
// user 11-08.)
func TestRecordedCopiesSurviveADeletedCopy(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	offApply(p, lifecycle.SourceLoad, append(append(offPair("p1", "one"), offPair("p2", "two")...), offPair("p3", "three")...)...)
	c := lifecycle.NewSession("chat:C")
	rows := append(append(offPair("c1", "one"), offPair("c2", "two")...), offPair("c3", "three")...)
	offApply(c, lifecycle.SourceBeforeRequest, append(append(append([]lifecycle.Row{}, rows...), marker("P", "p3a")), lifecycle.Row{ChatID: "c4u", Role: "user", Data: "new"})...)
	record(t, c, loaderOf(p, c))
	offApply(c, lifecycle.SourceBeforeRequest, append(append(append(append([]lifecycle.Row{}, rows[:2]...), rows[4:]...), marker("P", "p3a")), lifecycle.Row{ChatID: "c4u", Role: "user", Data: "new"})...)
	got := scopeWith(t, c, p)
	if len(got) != 3 || !got[0].Inherited || !got[1].Inherited || got[2].Inherited || got[0].Source.Turn != p.Turns[0].ID || got[1].Source.Turn != p.Turns[2].ID {
		t.Fatalf("scope %+v", got)
	}
}

// The parent deletes one of two identical turns after the branch was
// recorded: the child's copies do not move to the other occurrence.
func TestRecordedCopiesIgnoreTheParentsDeletion(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	pr := append(append(offPair("p0", "start"), offPair("p1", "same")...), offPair("p2", "same")...)
	offApply(p, lifecycle.SourceLoad, pr...)
	c := lifecycle.NewSession("chat:C")
	cr := append(append(offPair("c0", "start"), offPair("c1", "same")...), offPair("c2", "same")...)
	offApply(c, lifecycle.SourceLoad, append(append([]lifecycle.Row{}, cr...), marker("P", "p2a"))...)
	record(t, c, loaderOf(p, c))
	before := scopeWith(t, c, p)
	offApply(p, lifecycle.SourceLoad, append(append([]lifecycle.Row{}, pr[:2]...), pr[4:]...)...) // P1 deleted
	after := scopeWith(t, c, p)
	for i := range before {
		if before[i].Source.Key() != after[i].Source.Key() || before[i].Inherited != after[i].Inherited {
			t.Fatalf("copy %d moved: %+v -> %+v", i, before[i], after[i])
		}
	}
	if !after[0].Inherited || after[0].Source.Session != "chat:P" {
		t.Fatalf("the unique copy is not the parent's: %+v", after[0])
	}
}

// A fork from a parent user row: the copy of that turn is the branch's own,
// and stays its own after the earlier copy leaves the front of the chat
// (kept as a front cut, user 11-08) and the branch writes an answer the
// parent's earlier turn also has.
func TestRecordedPartialForkStaysOwn(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	offApply(p, lifecycle.SourceLoad, append(offPair("p1", "x"), offPair("p2", "x")...)...)
	c := lifecycle.NewSession("chat:C")
	offApply(c, lifecycle.SourceLoad, append(offPair("c1", "x"), lifecycle.Row{ChatID: "c2u", Role: "user", Data: "input x"}, marker("P", "p2u"))...)
	record(t, c, loaderOf(p, c))
	offApply(c, lifecycle.SourceLoad, lifecycle.Row{ChatID: "c2u", Role: "user", Data: "input x"}, marker("P", "p2u"), lifecycle.Row{ChatID: "new", Role: "char", Data: "reply x"})
	got := scopeWith(t, c, p)
	if len(got) != 2 || !got[0].Inherited || got[1].Inherited {
		t.Fatalf("scope %+v", got)
	}
}

// A grandchild reads the revision its direct parent holds: after the first
// chat rerolls and the grandchild edits its copy to the first chat's new
// text, the copy is the grandchild's own.
func TestGrandchildReadsTheDirectParentsRevision(t *testing.T) {
	a := lifecycle.NewSession("chat:A")
	offApply(a, lifecycle.SourceLoad, lifecycle.Row{ChatID: "a1u", Role: "user", Data: "q"}, lifecycle.Row{ChatID: "a1a", Role: "char", Data: "old", GenerationID: "g1"})
	b := lifecycle.NewSession("chat:B")
	offApply(b, lifecycle.SourceLoad, lifecycle.Row{ChatID: "b1u", Role: "user", Data: "q"}, lifecycle.Row{ChatID: "b1a", Role: "char", Data: "old", GenerationID: "g1"}, marker("A", "a1a"))
	record(t, b, loaderOf(a, b))
	offApply(a, lifecycle.SourceLoad, lifecycle.Row{ChatID: "a1u", Role: "user", Data: "q"}, lifecycle.Row{ChatID: "a1a", Role: "char", Data: "new", GenerationID: "g2"})
	g := lifecycle.NewSession("chat:G")
	gm := lifecycle.Row{ChatID: "mkg", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::B::name::b1a::}}"}
	offApply(g, lifecycle.SourceLoad, lifecycle.Row{ChatID: "g1u", Role: "user", Data: "q"}, lifecycle.Row{ChatID: "g1a", Role: "char", Data: "old", GenerationID: "g1"}, marker("A", "a1a"), gm)
	record(t, g, loaderOf(a, b, g))
	if got := scopeWith(t, g, a, b); len(got) != 1 || !got[0].Inherited || got[0].Source.Responses[0].Data != "old" {
		t.Fatalf("grandchild copy %+v", got)
	}
	offApply(g, lifecycle.SourceLoad, lifecycle.Row{ChatID: "g1u", Role: "user", Data: "q"}, lifecycle.Row{ChatID: "g1a", Role: "char", Data: "new", GenerationID: "g2"}, marker("A", "a1a"), gm)
	if got := scopeWith(t, g, a, b); len(got) != 1 || got[0].Inherited {
		t.Fatalf("edited grandchild copy read an ancestor revision: %+v", got)
	}
}

// The copies are not recorded while the parent's scope does not show the
// branch point yet, nor while a copy's text is not read; they are recorded
// at the observation that has both (ASTRA-RECHECK-54 3.2).
func TestCopiesWaitForTheBranchPointAndTheText(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	offApply(p, lifecycle.SourceLoad, offPair("p1", "one")...)
	c := lifecycle.NewSession("chat:C")
	cr := append(append(offPair("c1", "one"), offPair("c2", "two")...), marker("P", "p2a"))
	offApply(c, lifecycle.SourceLoad, cr...)
	if ok, err := RecordCopies(c, loaderOf(p, c)); ok || err != nil {
		t.Fatalf("recorded before the parent showed the branch point: %v %v", ok, err)
	}
	offApply(p, lifecycle.SourceLoad, append(offPair("p1", "one"), offPair("p2", "two")...)...)
	record(t, c, loaderOf(p, c))
	if got := scopeWith(t, c, p); len(got) != 2 || !got[0].Inherited || !got[1].Inherited {
		t.Fatalf("scope %+v", got)
	}
	// a branch whose copied reply is first seen without its text
	d := lifecycle.NewSession("chat:D")
	unread := append([]lifecycle.Row{}, cr...)
	unread[3] = lifecycle.Row{ChatID: "c2a", Role: "char", Unreadable: true}
	offApply(d, lifecycle.SourceLoad, unread...)
	if ok, _ := RecordCopies(d, loaderOf(p, d)); ok {
		t.Fatal("recorded with a copy's text not read")
	}
	offApply(d, lifecycle.SourceLoad, cr...)
	record(t, d, loaderOf(p, d))
	if got := scopeWith(t, d, p); len(got) != 2 || !got[1].Inherited {
		t.Fatalf("scope after the text came %+v", got)
	}
}

// A fork from a parent user row, first seen after the branch deleted its
// earlier copy and answered: the turn's answer comes after the marker, so
// the turn is the branch's own, not the parent's earlier same-text turn
// (ASTRA-RECHECK-52 2, before any record).
func TestCopiesAreOnlyTheRowsBeforeTheMarker(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	offApply(p, lifecycle.SourceLoad, append(offPair("p1", "x"), offPair("p2", "x")...)...)
	c := lifecycle.NewSession("chat:C")
	offApply(c, lifecycle.SourceBeforeRequest, lifecycle.Row{ChatID: "c2u", Role: "user", Data: "input x"}, marker("P", "p2u"), lifecycle.Row{ChatID: "new", Role: "char", Data: "reply x"}, lifecycle.Row{ChatID: "c3u", Role: "user", Data: "next"})
	record(t, c, loaderOf(p, c))
	if len(c.Copies) != 0 {
		t.Fatalf("copies %+v", c.Copies)
	}
	if got := scopeWith(t, c, p); len(got) != 2 || got[0].Inherited {
		t.Fatalf("scope %+v", got)
	}
}
