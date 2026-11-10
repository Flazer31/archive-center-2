package memory

import (
	"archivecenter2/goservice/internal/lifecycle"
	"strings"
	"testing"
)

func TestA27ScalarDoesNotBecomeEntity(t *testing.T) {
	out, notes := Parse(`{"entities":[{"id":"n1","name":"민재"},17],"memories":[{"id":"m1","body":"사실"}]}`)
	t.Logf("out=%+v notes=%v", out, notes)
	if len(out.Entities) != 1 || out.Entities[0].Name != "민재" || len(out.Memories) != 1 {
		t.Fatal("scalar became an entity instead of preserving actual object items")
	}
}

func TestA27OptionalFieldsAcrossArrays(t *testing.T) {
	out, notes := Parse(`{"entities":[{"id":"n1","q":"가면을 썼다"}],"memories":[{"id":"m1","kind":"event","effects":[{"about":"N1","value":"10냥"}]}],"scenes":[{"ref":"S1","q":"같은 장소"}],"links":[{"from":"M1","q":"근거"}],"extra":`)
	t.Logf("out=%+v notes=%v", out, notes)
	if len(out.Entities) != 1 || out.Entities[0].Q != "가면을 썼다" || len(out.Memories) != 1 || len(out.Memories[0].Effects) != 1 || len(out.Scenes) != 1 || out.Scenes[0].Ref != "S1" || len(out.Links) != 1 || out.Links[0].From != "M1" {
		t.Fatal("optional fields erased actual object content")
	}
}

func TestA27RecalledClosedObligation(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", Mode: "의무", About: "N1", Body: "흑요석", Terms: "보름 전에 돌려준다", Status: "active"}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Body: "채무 이행", Effects: []Effect{{About: "M1", Status: "fulfilled"}}}},
	}}
	r := RenderKnown(Select(k, "민재가 흑요석을 떠올렸다", nil), Superseded(k))
	t.Log(r)
	if !strings.Contains(r, "보름 전에 돌려준다") || !strings.Contains(r, "status fulfilled") {
		t.Fatal("recall lost original terms or completed status")
	}
	if a26Has(Select(k, "민재가 잔다", nil), "M1") {
		t.Fatal("closed obligation still selected as open")
	}
}

func TestA27IncompleteItemAfterFinal(t *testing.T) {
	raw := `{"memories":[{"id":"m1","kind":"fact","body":"보존할 사실"}]} {"id":"s1","q":"memories","change":`
	out, notes := Parse(raw)
	t.Logf("out=%+v notes=%v", out, notes)
	if len(out.Memories) != 1 || out.Memories[0].ID != "m1" {
		t.Fatal("unfinished standalone item erased final response")
	}
}

func TestA27EmptyFinalDoesNotReadDraft(t *testing.T) {
	out, notes := Parse(`{"memories":[{"id":"draft","body":"초안"}]} final: {}`)
	t.Logf("out=%+v notes=%v", out, notes)
	if len(out.Memories) != 0 {
		t.Fatal("empty final read earlier draft")
	}
}

func TestA27SalvageUsesArrayKeyNotStringValue(t *testing.T) {
	out, notes := Parse(`{"extra":"memories","entities":[{"id":"n1","name":"민재"}],"memories":[{"id":"m1","kind":"fact","body":"실제 기억"}],"links":[`)
	t.Logf("out=%+v notes=%v", out, notes)
	if len(out.Memories) != 1 || out.Memories[0].ID != "m1" || out.Memories[0].Body != "실제 기억" {
		t.Fatal("a quoted field name stole the actual array")
	}
}

func TestA27EffectProtectionNames(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}, {ID: "N2", Name: "서진"}}, Memories: []KnownMemory{{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "몰래 받았다", Effects: []Effect{{About: "N1", Slot: "돈", Value: "100냥"}}}}}, Awareness: []KnownAwareness{{Target: "M1", Kind: "hide_from", Knows: Knows{Who: "N2"}}}}
	r := RenderKnown(Select(k, "민재가 웃었다", nil), Superseded(k))
	t.Log(r)
	if !strings.Contains(r, "hide_from: N2") || !strings.Contains(r, "[N2] 서진") {
		t.Fatal("protection target name is not delivered")
	}
}

func TestA27ReopenedObligationStatusHistory(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", Mode: "의무", About: "N1", Body: "빚을 갚는다", Status: "active"}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Body: "채무 이행", Effects: []Effect{{About: "M1", Status: "fulfilled"}}}},
		{ID: "M3", Turn: 3, Memory: Memory{Kind: "event", Body: "납부 취소 후 재개", Effects: []Effect{{About: "M1", Status: "resumed"}}}},
	}}
	r := RenderKnown(Select(k, "민재가 잔다", nil), Superseded(k))
	t.Log(r)
	if !a26Has(Select(k, "민재가 잔다", nil), "M1") || !strings.Contains(r, "status resumed") {
		t.Fatal("reopened obligation lost")
	}
}

func TestA27LateOriginalConfirmationAndRetry(t *testing.T) {
	snap := func(src lifecycle.Source, gen, data string, unread bool) lifecycle.Snapshot {
		return lifecycle.Snapshot{SessionKey: "s", Source: src, TargetIndex: 1, Rows: []lifecycle.Row{{Index: 0, ChatID: "u", Role: "user", Data: "입력"}, {Index: 1, ChatID: "a", Role: "char", GenerationID: gen, Data: data, Unreadable: unread}}}
	}
	next := func(gen string, unread bool) lifecycle.Snapshot { // the next input confirms the turn
		sn := snap(lifecycle.SourceBeforeRequest, gen, "", unread)
		sn.Rows = append(sn.Rows, lifecycle.Row{Index: 2, ChatID: "n", Role: "user", Data: "다음"})
		return sn
	}
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "not_completed", true: "completed"}[confirmed], func(t *testing.T) {
			s := lifecycle.NewSession("s")
			s.Apply(snap(lifecycle.SourceOutputEvent, "g1", "같은 본문", false))
			original := SourceOf(s.Key, s.ActiveTurns()[0])
			s.Apply(snap(lifecycle.SourceBeforeRequest, "g2", "", true))
			s.Apply(snap(lifecycle.SourceOutputEvent, "g2", "", true))
			if confirmed {
				s.Apply(next("g2", true))
			}
			s.Apply(snap(lifecycle.SourceLoad, "", "같은 본문", false))
			turn := s.ActiveTurns()[0]
			row := turn.Responses[0]
			current := SourceOf(s.Key, turn)
			if len(s.ActiveTurns()) != 1 || !current.Readable() || current.Responses[0].Gen != "g2" || current.Key() == original.Key() || turn.Confirmed != confirmed || row.Revisions[len(row.Revisions)-1].Confirmed != confirmed {
				t.Fatalf("catchup=%+v turn=%+v revisions=%+v", current, turn, row.Revisions)
			}
			n := len(row.Revisions)
			s.Apply(snap(lifecycle.SourceBeforeRequest, "", "같은 본문", false))
			s.Apply(snap(lifecycle.SourceLoad, "", "같은 본문", false))
			if len(row.Revisions) != n || SourceOf(s.Key, turn).Key() != current.Key() || turn.Confirmed != confirmed {
				t.Fatal("retry changed revision/job/completion")
			}
			s.Apply(snap(lifecycle.SourceLoad, "", "편집 본문", false))
			rev := row.Revisions[len(row.Revisions)-1]
			if len(row.Revisions) != n+1 || rev.GenerationID != "g2" || rev.Confirmed || turn.Confirmed != confirmed {
				t.Fatalf("edit changed contract: %+v", row.Revisions)
			}
			t.Logf("confirmed=%v original_seq=%d current_seq=%d revisions=%+v", confirmed, original.Responses[0].Seq, current.Responses[0].Seq, row.Revisions)
		})
	}
}
