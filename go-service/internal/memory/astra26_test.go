package memory

import (
	"archivecenter2/goservice/internal/lifecycle"
	"strings"
	"testing"
)

func a26Has(s Selection, id string) bool {
	for _, m := range s.Memories {
		if m.ID == id {
			return true
		}
	}
	return false
}

func TestA26RecallQuoteChangesEligibility(t *testing.T) {
	k := Known{Memories: []KnownMemory{{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "흑요석"}}}}
	if !a26Has(Select(k, "흑요석을 떠올렸다", nil), "M1") {
		t.Fatal("control failed")
	}
	k.Memories[0].Q = "흑요석"
	got := Select(k, "흑요석을 떠올렸다", nil)
	t.Logf("same fact with its supporting quote: %+v", got)
	if !a26Has(got, "M1") {
		t.Fatal("supporting quote alone suppressed exact recall")
	}
}

func TestA26EffectCarriesProtection(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}, {ID: "N2", Name: "서진"}}, Memories: []KnownMemory{{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "몰래 받았다", Effects: []Effect{{About: "N1", Slot: "돈", Value: "100냥"}}}}}, Awareness: []KnownAwareness{{Target: "M1", Kind: "hide_from", Knows: Knows{Who: "N2"}}}}
	sel := Select(k, "민재가 웃었다", nil)
	rendered := RenderKnown(sel, Superseded(k))
	t.Log(rendered)
	if len(sel.Effects) != 1 {
		t.Fatal("effect not selected")
	}
	if !strings.Contains(rendered, "hide_from") {
		t.Fatal("selected effect lost its event protection")
	}
}

func TestA26ExplicitSceneOwnership(t *testing.T) {
	k := Known{Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 1}, {ID: "S1", Change: "close", Turn: 2}, {ID: "S2", Change: "open", Turn: 2}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 3, Memory: Memory{Kind: "event", Body: "옛날 회상", Scene: "S1"}},
		{ID: "M2", Turn: 3, Memory: Memory{Kind: "event", Body: "현재 대화", Scene: "S2"}},
	}}
	sel := Select(k, "무관한 입력", nil)
	t.Logf("%+v", sel)
	if a26Has(sel, "M1") || !a26Has(sel, "M2") {
		t.Error("stored turn replaced explicit scene membership")
	}
	a := RenderKnown(Selection{Memories: []KnownMemory{k.Memories[0]}}, nil)
	k.Memories[0].Scene = "S2"
	b := RenderKnown(Selection{Memories: []KnownMemory{k.Memories[0]}}, nil)
	if a == b {
		t.Fatal("render lost scene distinction")
	}
}

func TestA26LateOriginalWithoutRepeatedGenerationID(t *testing.T) {
	snap := func(src lifecycle.Source, gen, data string, unread bool) lifecycle.Snapshot {
		return lifecycle.Snapshot{SessionKey: "s", Source: src, TargetIndex: 1, Rows: []lifecycle.Row{{Index: 0, ChatID: "u", Role: "user", Data: "입력"}, {Index: 1, ChatID: "a", Role: "char", GenerationID: gen, Data: data, Unreadable: unread}}}
	}
	s := lifecycle.NewSession("s")
	s.Apply(snap(lifecycle.SourceOutputEvent, "g1", "같은 본문", false))
	before := SourceOf(s.Key, s.ActiveTurns()[0])
	s.Apply(snap(lifecycle.SourceBeforeRequest, "g2", "", true))
	s.Apply(snap(lifecycle.SourceOutputEvent, "g2", "", true))
	s.Apply(snap(lifecycle.SourceLoad, "", "같은 본문", false))
	after := SourceOf(s.Key, s.ActiveTurns()[0])
	t.Logf("before=%+v after=%+v confirmed=%v", before, after, s.ActiveTurns()[0].Confirmed)
	if !after.Readable() || after.Responses[0].Seq == before.Responses[0].Seq {
		t.Fatal("known generation catchup requires optional ID repeated")
	}
}

func TestA26CompletedObligationOutsideScene(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Scenes: []KnownScene{{ID: "S2", Change: "open", Turn: 3}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Mode: "의무", Body: "빚을 갚는다", Status: "active"}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Body: "채무 이행", Effects: []Effect{{About: "M1", Status: "fulfilled"}}}},
	}}
	r := RenderKnown(Select(k, "민재가 잔다", nil), Superseded(k))
	t.Log(r)
	if strings.Contains(r, "status active") && !strings.Contains(r, "fulfilled") {
		t.Fatal("completed obligation presented as active without completion")
	}
}

func TestA26UnknownFirstFieldPreservesKnownItems(t *testing.T) {
	out, notes := Parse(`{"extra":"note","memories":[{"id":"m1","kind":"fact","body":"보존할 사실"}]}`)
	t.Logf("out=%+v notes=%v", out, notes)
	if len(out.Memories) != 1 {
		t.Fatal("unknown first field discarded known memory")
	}
}

func TestA26IncompleteDraftDoesNotHideFinal(t *testing.T) {
	out, notes := Parse("draft {\"memories\":[\nfinal: {\"memories\":[{\"id\":\"m1\",\"kind\":\"fact\",\"body\":\"최종\"}]}")
	t.Logf("out=%+v notes=%v", out, notes)
	if len(out.Memories) != 1 || out.Memories[0].Body != "최종" {
		t.Fatal("incomplete draft hid complete final object")
	}
}

func TestA26SalvageOptionalBody(t *testing.T) {
	complete := `{"memories":[{"id":"m1","kind":"event","effects":[{"about":"N1","value":"10냥"}]}]}`
	a, _ := Parse(complete)
	b, notes := Parse(strings.TrimSuffix(complete, "]}") + `,{"id":"m2","body":`)
	t.Logf("complete=%+v truncated=%+v notes=%v", a, b, notes)
	if len(a.Memories) != 1 || len(b.Memories) != 1 {
		t.Fatal("complete meaningful item dropped because optional body missing")
	}
}

func TestA26ViewAboutReference(t *testing.T) {
	k := Known{Scenes: []KnownScene{{ID: "S2", Change: "open", Turn: 2}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "배신"}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "view", Owner: "N1", About: "M1", Body: "분노"}},
	}}
	sel := Select(k, "현재 이야기", nil)
	t.Logf("%+v", sel)
	if !a26Has(sel, "M1") {
		t.Fatal("selected view points to absent about event")
	}
}

func TestA26CloseKeepsDifferentOpenScene(t *testing.T) {
	k := Known{Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 1}, {ID: "S2", Change: "open", Turn: 2}, {ID: "S1", Change: "close", Turn: 2}}}
	now := Select(k, "", nil).Scene
	if now != "S2" {
		t.Fatal("closing S1 incorrectly closed S2")
	}
}
