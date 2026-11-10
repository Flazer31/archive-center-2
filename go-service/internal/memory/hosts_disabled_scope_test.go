package memory

import (
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
)

// Turned-off turns keep their place for matching a branch's copies
// (ASTRA-RECHECK-51 H3-1) and are dropped from the read scope only after.

func offPair(prefix, body string) []lifecycle.Row {
	return []lifecycle.Row{{ChatID: prefix + "u", Role: "user", Data: "input " + body}, {ChatID: prefix + "a", Role: "char", Data: "reply " + body}}
}

func offApply(s *lifecycle.Session, src lifecycle.Source, rows ...lifecycle.Row) {
	for i := range rows {
		rows[i].Index = i
	}
	s.Apply(lifecycle.Snapshot{SessionKey: s.Key, Source: src, TargetIndex: -1, Rows: rows})
}

func scopeWith(t *testing.T, c *lifecycle.Session, parents ...*lifecycle.Session) []ScopedTurn {
	t.Helper()
	// the pipeline records a branch's copies at the observation
	if c.BranchOf[0] != "" && c.CopiesOf != c.BranchOf {
		_, err := RecordCopies(c, loaderOf(append(parents, c)...))
		if err != nil {
			t.Fatal(err)
		}
	}
	return ScopeOf(c)
}

func marker(parent, point string) lifecycle.Row {
	return lifecycle.Row{ChatID: "mk", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::" + parent + "::name::" + point + "::}}"}
}

// The branch turns its first copy off (new ids): the other copies still
// read the parent's memory.
func TestOffCopyKeepsTheOtherCopiesInherited(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	offApply(p, lifecycle.SourceLoad, append(append(offPair("p1", "one"), offPair("p2", "two")...), offPair("p3", "three")...)...)
	c := lifecycle.NewSession("chat:C")
	rows := append(append(offPair("c1", "one"), offPair("c2", "two")...), offPair("c3", "three")...)
	rows[0].Disabled = "true"
	offApply(c, lifecycle.SourceLoad, append(rows, marker("P", "p3a"))...)
	got := scopeWith(t, c, p)
	if len(got) != 2 || !got[0].Inherited || !got[1].Inherited || got[0].Source.Turn != p.Turns[1].ID || got[1].Source.Turn != p.Turns[2].ID {
		t.Fatalf("scope %+v", got)
	}
}

// The parent and the branch turn the branch point's turn off: the turns
// before it are still the branch's past.
func TestOffBranchPointKeepsTheEarlierPast(t *testing.T) {
	p := lifecycle.NewSession("chat:P")
	pr := append(offPair("p1", "one"), offPair("p2", "two")...)
	pr[2].Disabled = "true"
	offApply(p, lifecycle.SourceLoad, pr...)
	c := lifecycle.NewSession("chat:C")
	cr := append(offPair("p1", "one"), offPair("p2", "two")...)
	cr[2].Disabled = "true"
	offApply(c, lifecycle.SourceLoad, append(cr, marker("P", "p2a"))...)
	got := scopeWith(t, c, p)
	if len(got) != 1 || !got[0].Inherited || got[0].Source.Turn != p.Turns[0].ID {
		t.Fatalf("scope %+v", got)
	}
}

// A reply without chatId seen unreadable and disabled stands for the known
// reply: that reply is off (ASTRA-RECHECK-51 H3-2), and the turn has
// nothing left to read.
func TestUnreadableKeylessDisabledReplyIsOff(t *testing.T) {
	s := lifecycle.NewSession("chat:A")
	offApply(s, lifecycle.SourceOutputEvent, lifecycle.Row{ChatID: "u", Role: "user", Data: "input"}, lifecycle.Row{Role: "char", Data: "original"})
	offApply(s, lifecycle.SourceLoad, lifecycle.Row{ChatID: "u", Role: "user", Data: "input"}, lifecycle.Row{Role: "char", Unreadable: true, Disabled: "true"})
	tr := s.ActiveTurns()[0]
	got := scopeWith(t, s)
	if tr.Off || !tr.Responses[0].Off || len(got) != 1 || len(got[0].Source.Responses) != 0 || got[0].Source.Readable() {
		t.Fatalf("turn off %v reply off %v scope %+v", tr.Off, tr.Responses[0].Off, got)
	}
}

// The user decision (10-23, option C): a disabled user row (a one-turn OOC)
// takes the whole turn out; a disabled reply alone takes only that reply
// out, and the turn's other replies are read.
func TestDisabledUserRowOrReplyOnly(t *testing.T) {
	s := lifecycle.NewSession("chat:A")
	offApply(s, lifecycle.SourceLoad,
		lifecycle.Row{ChatID: "u1", Role: "user", Data: "그룹 질문"}, lifecycle.Row{ChatID: "a1", Role: "char", Data: "첫 캐릭터 답"}, lifecycle.Row{ChatID: "b1", Role: "char", Data: "끈 답", Disabled: "true"},
		lifecycle.Row{ChatID: "u2", Role: "user", Data: "(OOC)", Disabled: "true"}, lifecycle.Row{ChatID: "a2", Role: "char", Data: "OOC 답"})
	got := scopeWith(t, s)
	if len(got) != 1 || len(got[0].Source.Responses) != 1 || got[0].Source.Responses[0].Data != "첫 캐릭터 답" {
		t.Fatalf("scope %+v", got)
	}
	// turning the reply on again reads it again
	offApply(s, lifecycle.SourceLoad,
		lifecycle.Row{ChatID: "u1", Role: "user", Data: "그룹 질문"}, lifecycle.Row{ChatID: "a1", Role: "char", Data: "첫 캐릭터 답"}, lifecycle.Row{ChatID: "b1", Role: "char", Data: "끈 답"},
		lifecycle.Row{ChatID: "u2", Role: "user", Data: "(OOC)", Disabled: "true"}, lifecycle.Row{ChatID: "a2", Role: "char", Data: "OOC 답"})
	if got := scopeWith(t, s); len(got) != 1 || len(got[0].Source.Responses) != 2 {
		t.Fatalf("on again %+v", got)
	}
}
