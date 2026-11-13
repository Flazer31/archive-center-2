package memory

import (
	"archivecenter2/goservice/internal/lifecycle"
	"testing"
)

// Actual first observation can follow a deletion in the parent.
func TestA55DeleteBeforeRecord(t *testing.T) {
	for _, kept := range []bool{false, true} {
		name := "new"
		if kept {
			name = "kept"
		}
		t.Run(name, func(t *testing.T) {
			p := lifecycle.NewSession("chat:P")
			pr := append(offPair("p1", "same"), offPair("p2", "same")...)
			offApply(p, lifecycle.SourceLoad, pr...)
			original := SourceOf(p.Key, p.Turns[0]).Key()
			offApply(p, lifecycle.SourceLoad, pr[2:]...)
			c := lifecycle.NewSession("chat:C")
			cr := append(offPair("c1", "same"), offPair("c2", "same")...)
			if kept {
				cr = append([]lifecycle.Row{}, pr...)
			}
			offApply(c, lifecycle.SourceBeforeRequest, append(cr, marker("P", "p2a"), lifecycle.Row{ChatID: "next", Role: "user", Data: "next"})...)
			record(t, c, loaderOf(p, c))
			got := scopeWith(t, c, p)
			t.Logf("A55: kept=%v original=%s selected=%s session=%s turn=%d copies=%d", kept, original, got[0].Source.Key(), got[0].Source.Session, got[0].Source.Turn, len(c.Copies))
			if got[0].Inherited && got[0].Source.Key() != original {
				t.Fatal("first child occurrence borrowed the parent's second occurrence before recording")
			}
		})
	}
}

// Both sides have one active reply, but the replies are different row occurrences.
func TestA55OffDifferentRows(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	pr := []lifecycle.Row{{ChatID: "u", Role: "user", Data: "q"}, {ChatID: "a", Role: "char", Data: "same"}, {ChatID: "b", Role: "char", Data: "same"}}
	offApply(p, lifecycle.SourceLoad, pr...)
	pr[1].Disabled = "true"
	offApply(p, lifecycle.SourceLoad, pr...)
	c := lifecycle.NewSession("chat:C")
	cr := append([]lifecycle.Row{}, pr...)
	cr[1].Disabled = ""
	cr[2].Disabled = "true"
	offApply(c, lifecycle.SourceBeforeRequest, append(cr, marker("P", "b"), lifecycle.Row{ChatID: "next", Role: "user", Data: "next"})...)
	record(t, c, loaderOf(p, c))
	got := scopeWith(t, c, p)
	t.Logf("A55: off-different child reads %+v", got[0].Source)
	if got[0].Inherited && got[0].Source.Responses[0].Key != "c:a" {
		t.Fatal("enabled child reply a borrowed disabled-in-child reply b")
	}
}

func TestA55OldRevisionAndGrandchildBeforeRecord(t *testing.T) {
	for _, kept := range []bool{false, true} {
		name := "new"
		if kept {
			name = "kept"
		}
		t.Run(name, func(t *testing.T) {
			p := lifecycle.NewSession("chat:P")
			old := []lifecycle.Row{{ChatID: "u", Role: "user", Data: "q"}, {ChatID: "a", Role: "char", GenerationID: "g1", Data: "old"}}
			offApply(p, lifecycle.SourceLoad, old...)
			oldkey := SourceOf(p.Key, p.Turns[0]).Key()
			newer := append([]lifecycle.Row{}, old...)
			newer[1].GenerationID = "g2"
			newer[1].Data = "new"
			offApply(p, lifecycle.SourceLoad, newer...)
			c := lifecycle.NewSession("chat:C")
			cr := append([]lifecycle.Row{}, old...)
			if !kept {
				cr[0].ChatID = "cu"
				cr[1].ChatID = "ca"
			}
			offApply(c, lifecycle.SourceLoad, append(cr, marker("P", "a"))...)
			record(t, c, loaderOf(p, c))
			// the parent kept only its newest text: the child, holding the
			// old one, reads its own turn and never the parent's newer reply
			if got := scopeWith(t, c, p); got[0].Inherited || got[0].Source.Key() == oldkey || got[0].Source.Responses[0].Data != "old" {
				t.Fatalf("child holding the parent's overwritten text: %+v", got[0])
			}
			g := lifecycle.NewSession("chat:G")
			gr := append([]lifecycle.Row{}, cr...)
			if !kept {
				gr[0].ChatID = "gu"
				gr[1].ChatID = "ga"
			}
			gr[1].Data = "new"
			gr[1].GenerationID = "g2"
			gm := lifecycle.Row{ChatID: "gm", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::C::name::" + cr[1].ChatID + "::}}"}
			offApply(g, lifecycle.SourceLoad, append(gr, marker("P", "a"), gm)...)
			record(t, g, loaderOf(p, c, g))
			if got := scopeWith(t, g, p, c); got[0].Inherited {
				t.Fatal("grandchild borrowed an ancestor revision absent from its direct parent")
			}
		})
	}
}

// A record deferred for unread text must not let an unrelated post-marker turn borrow a parent.
func TestA55DeferredRecordStillUsesChildBoundary(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	pr := append(offPair("p1", "x"), offPair("p2", "x")...)
	offApply(p, lifecycle.SourceLoad, pr...)
	c := lifecycle.NewSession("chat:C")
	cr := []lifecycle.Row{{ChatID: "c2u", Role: "user", Data: "input x"}, marker("P", "p2u"), {ChatID: "new", Role: "char", Data: "reply x"}, {ChatID: "lateru", Role: "user", Data: "later"}}
	offApply(c, lifecycle.SourceBeforeRequest, cr...)
	if ok, _ := RecordCopies(c, loaderOf(c)); ok {
		t.Fatal("parent was not yet observed")
	}
	// The next observation lacks the temporarily detached marker, as reroll can do.
	offApply(c, lifecycle.SourceLoad, cr[0], cr[2], cr[3])
	if ok, _ := RecordCopies(c, loaderOf(p, c)); ok {
		t.Fatal("expected recording to defer")
	}
	got := scopeWith(t, c, p)
	t.Logf("A55: deferred scope=%s turn=%d inherited=%v", got[0].Source.Session, got[0].Source.Turn, got[0].Inherited)
	if got[0].Inherited {
		t.Fatal("deferred recording still dynamically borrowed earlier identical parent turn")
	}
}
