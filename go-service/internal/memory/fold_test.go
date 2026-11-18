package memory

import (
	"strings"
	"testing"
)

// The lists of their own are read by Parse and folded into the items the
// store keeps (1.3). Names here are made up.
func TestFoldBoundaries(t *testing.T) {
	raw := `{"memories":[
	 {"id":"m1","kind":"event","body":"가람이 상인 행세를 하며 성문을 지났다."},
	 {"id":"m2","kind":"fact","mode":"정체","about":"N1","body":"가람은 북쪽 기사단 소속이다."}],
	 "boundaries":[
	  {"on":"m2","unaware":["N7"],"hide_from":"성문 경비대","q":"상인이라고 둘러댔다"},
	  {"on":"M40","knows":["N3"],"q":"다온은 이미 알고 있었다"},
	  {"what":"가람의 짐에는 밀서가 들어 있다.","whose":"N1","hide_from":["N7"],"q":"밀서를 숨긴 채"},
	  {"on":"M41"},
	  {}]}`
	out, notes := Parse(raw)
	if len(out.Boundaries) != 5 {
		t.Fatalf("parsed %d boundaries, notes %v", len(out.Boundaries), notes)
	}
	got, foldNotes := Fold(out, Known{})
	if len(got.Boundaries) != 0 {
		t.Fatalf("boundaries stay after folding")
	}
	m2 := got.Memories[1]
	if len(m2.Unaware) != 1 || m2.Unaware[0] != "N7" || len(m2.HideFrom) != 1 || m2.HideFrom[0] != "성문 경비대" {
		t.Fatalf("m2 awareness: %+v", m2.Awareness)
	}
	if len(got.Memories) != 4 {
		t.Fatalf("memories %d, want the two written, one update and one new fact", len(got.Memories))
	}
	up := got.Memories[2]
	if up.Ref != "M40" || up.Kind != "" || len(up.Knows) != 1 || up.Knows[0].Who != "N3" {
		t.Fatalf("update of a stored item: %+v", up)
	}
	nf := got.Memories[3]
	if nf.Kind != "fact" || nf.Mode != IdentityMode || nf.About != "N1" || nf.Body != "가람의 짐에는 밀서가 들어 있다." || len(nf.HideFrom) != 1 || nf.ID == "" {
		t.Fatalf("new fact of a boundary with no item: %+v", nf)
	}
	// the two entries that say nothing are noted, the rest is kept
	if len(foldNotes) != 2 {
		t.Fatalf("notes %v", foldNotes)
	}
}

func TestFoldClosed(t *testing.T) {
	out, _ := Parse(`{"memories":[{"id":"m1","kind":"view","owner":"N1","body":"x"},{"id":"m2","kind":"event","body":"다온이 약속한 약초를 건넸다.","effects":[{"about":"N2","slot":"약초","value":"3"}]}],
	 "closed":[{"ref":"M12","status":"fulfilled","how":"약초를 건넸다","q":"약초를 건넸다"},{"ref":"M13","status":"cancelled"},{"status":"missed"}]}`)
	got, notes := Fold(out, Known{})
	ev := got.Memories[1]
	if len(ev.Effects) != 3 || ev.Effects[0].Slot != "약초" || ev.Effects[1].About != "M12" || ev.Effects[1].Status != "fulfilled" || ev.Effects[2].About != "M13" || ev.Effects[2].Status != "cancelled" {
		t.Fatalf("effects: %+v", ev.Effects)
	}
	if len(notes) != 1 || len(got.Closed) != 0 {
		t.Fatalf("notes %v closed %v", notes, got.Closed)
	}
	// no event in the answer: one that says how it ended carries the status
	out, _ = Parse(`{"closed":[{"ref":"M12","status":"missed","how":"가람은 끝내 오지 않았다.","q":"오지 않았다"},{"ref":"M13","status":"cancelled"}]}`)
	got, notes = Fold(out, Known{})
	if len(got.Memories) != 1 || got.Memories[0].Kind != "event" || got.Memories[0].Body != "가람은 끝내 오지 않았다." || len(got.Memories[0].Effects) != 2 || len(notes) != 0 {
		t.Fatalf("carrier: %+v notes %v", got.Memories, notes)
	}
	// the stored status closes the obligation for later reads
	k := Known{Memories: []KnownMemory{
		{ID: "M12", Turn: 1, Memory: Memory{Kind: "fact", Mode: "의무", About: "N1", Body: "가람은 돌아와야 한다."}},
		{ID: "M20", Turn: 3, Memory: got.Memories[0]},
	}}
	for _, m := range CurrentFacts(k) {
		if m.ID == "M12" {
			t.Fatalf("M12 still current after its status effect")
		}
	}
}

func TestFoldStateReplacesEarlierState(t *testing.T) {
	k := Known{Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", Mode: "상태", Slot: StateSlot, About: "N1", Body: "가람은 성문 앞에 있다."}},
		{ID: "M2", Turn: 1, Memory: Memory{Kind: "fact", Mode: "상태", Slot: "위치", About: "N1", Body: "1.2까지의 상태"}},
		{ID: "M3", Turn: 1, Memory: Memory{Kind: "fact", Mode: IdentityMode, About: "N1", Body: "가람은 북쪽 기사단 소속이다."}},
		{ID: "M4", Turn: 1, Memory: Memory{Kind: "fact", Mode: "상태", Slot: StateSlot, About: "N2", Body: "다온은 여관에 있다."}},
	}}
	out, _ := Parse(`{"memories":[{"id":"m1","kind":"fact","mode":"상태","slot":"지금","about":"N1","body":"가람은 여관 2층에 있다."},
	 {"id":"m2","kind":"fact","mode":"상태","slot":"옷","about":"N2","body":"다온은 망토를 입었다."}]}`)
	got, _ := Fold(out, k)
	if len(got.Memories[0].Was) != 1 || got.Memories[0].Was[0] != "M1" {
		t.Fatalf("was %v, want only the earlier current state of the same subject", got.Memories[0].Was)
	}
	if len(got.Memories[1].Was) != 0 {
		t.Fatalf("a state of another slot named %v", got.Memories[1].Was)
	}
	// after it is stored, the earlier state is replaced and the identity stays
	k.Memories = append(k.Memories, KnownMemory{ID: "M9", Turn: 2, Memory: got.Memories[0]})
	sup := Superseded(k)
	if !sup["M1"] || sup["M2"] || sup["M3"] || sup["M4"] {
		t.Fatalf("superseded %v", sup)
	}
	// the next state replaces M9, not M1 again
	got, _ = Fold(out, k)
	if len(got.Memories[0].Was) != 1 || got.Memories[0].Was[0] != "M9" {
		t.Fatalf("second state names %v", got.Memories[0].Was)
	}
}

// A boundary on an event that also says the secret keeps the awareness on a
// fact of that sentence; the event is recorded at it (11-17). On a fact, or
// with no sentence, the item itself keeps the awareness as before.
func TestFoldBoundaryWithWhatOnAnEvent(t *testing.T) {
	out, _ := Parse(`{"memories":[
	 {"id":"m1","kind":"event","body":"가람이 다온에게 밀서를 건네며 웃었다."},
	 {"id":"m2","kind":"fact","mode":"정체","about":"N1","body":"가람은 북쪽 기사단 소속이다."},
	 {"id":"m3","kind":"event","body":"가람이 성문을 지났다."}],
	 "boundaries":[
	  {"on":"m1","what":"밀서는 북쪽 기사단의 명령서다.","whose":"N1","knows":["N2"],"unaware":["N7"],"hide_from":["N7"],"q":"밀서를"},
	  {"on":"m2","what":"가람은 기사단 소속을 숨긴다.","unaware":["N7"]},
	  {"on":"m3","unaware":["N7"]}]}`)
	got, notes := Fold(out, Known{})
	if len(notes) != 0 || len(got.Memories) != 4 {
		t.Fatalf("memories %d notes %v", len(got.Memories), notes)
	}
	ev, f := got.Memories[0], got.Memories[3]
	if ev.At != f.ID || f.ID == "" || len(ev.Unaware)+len(ev.HideFrom)+len(ev.Knows) != 0 {
		t.Fatalf("event: %+v fact: %+v", ev, f)
	}
	if f.Kind != "fact" || f.Mode != IdentityMode || f.About != "N1" || f.Body != "밀서는 북쪽 기사단의 명령서다." ||
		len(f.Unaware) != 1 || len(f.HideFrom) != 1 || len(f.Knows) != 1 || f.Knows[0].Who != "N2" {
		t.Fatalf("secret fact: %+v", f)
	}
	if len(got.Memories[1].Unaware) != 1 || got.Memories[1].At != "" {
		t.Fatalf("a fact keeps its own awareness: %+v", got.Memories[1])
	}
	if len(got.Memories[2].Unaware) != 1 || got.Memories[2].At != "" {
		t.Fatalf("an event with no sentence keeps its own awareness: %+v", got.Memories[2])
	}
}

// A state or place fact written without its mode reads as that mode, so a
// state left without one is still replaced by the next (11-17).
func TestFoldReadsALeftOutMode(t *testing.T) {
	k := Known{Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", Mode: "상태", Slot: StateSlot, About: "N1", Body: "가람은 성문 앞에 있다."}},
	}}
	out, _ := Parse(`{"memories":[{"id":"m1","kind":"fact","slot":"지금","about":"N1","body":"가람은 여관에 있다."},
	 {"id":"m2","kind":"fact","slot":"위치","about":"N5","to":"N6","body":"여관은 성문 옆에 있다."},
	 {"id":"m3","kind":"fact","mode":"위치","slot":"위치","about":"N7","body":"시장은 광장 남쪽이다."},
	 {"id":"m4","kind":"fact","slot":"돈","about":"N1","body":"20냥"}]}`)
	got, _ := Fold(out, k)
	if got.Memories[0].Mode != "상태" || len(got.Memories[0].Was) != 1 || got.Memories[0].Was[0] != "M1" {
		t.Fatalf("state: %+v", got.Memories[0])
	}
	if got.Memories[1].Mode != IdentityMode || got.Memories[2].Mode != IdentityMode {
		t.Fatalf("place: %q %q", got.Memories[1].Mode, got.Memories[2].Mode)
	}
	if got.Memories[3].Mode != "" {
		t.Fatalf("another slot got a mode: %q", got.Memories[3].Mode)
	}
}

// A cut-off answer keeps the complete entries of the new lists.
func TestParseSalvagesBoundariesAndClosed(t *testing.T) {
	out, _ := Parse(`{"memories":[{"id":"m1","kind":"event","body":"x"}],"boundaries":[{"on":"m1","unaware":["N2"]},{"on":"m1","unaw`)
	if len(out.Memories) != 1 || len(out.Boundaries) != 1 {
		t.Fatalf("memories %d boundaries %d", len(out.Memories), len(out.Boundaries))
	}
	out, _ = Parse(`{"closed":[{"ref":"M3","status":"fulfilled"}]}`)
	if len(out.Closed) != 1 {
		t.Fatalf("an answer of only closed was not read")
	}
}

// A generation call's KNOWN shows the open records together, the awareness
// lines under their heading and the people of the open scene.
func TestKnownSections(t *testing.T) {
	k := Known{
		Names: []KnownName{{ID: "N1", Name: "가람", Type: "person", Turn: 1}, {ID: "N2", Name: "다온", Type: "person", Turn: 1}, {ID: "N3", Name: "여관", Type: "place", Turn: 1}},
		Memories: []KnownMemory{
			{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", Mode: "의무", About: "N1", To: Who{"N2"}, Body: "가람은 다온에게 약초를 가져다줘야 한다."}},
			{ID: "M2", Turn: 1, Memory: Memory{Kind: "event", Body: "가람이 여관에서 다온을 만났다.", Roles: &Roles{Actor: []string{"N1"}, Target: []string{"N2"}, Place: []string{"N3"}}}},
			{ID: "M3", Turn: 1, Memory: Memory{Kind: "fact", Mode: IdentityMode, About: "N1", Body: "가람은 북쪽 기사단 소속이다."}},
		},
		Awareness: []KnownAwareness{{Target: "M3", Kind: "unaware", Turn: 1, Knows: Knows{Who: "N2"}}},
		Scenes:    []KnownScene{{ID: "S1", Change: "open", Turn: 1}},
	}
	sel, _ := SelectWithin(k, "가람이 다온에게 말했다.", nil, 0)
	shown := RenderKnown(sel, Superseded(k))
	open := strings.Index(shown, "OPEN (")
	if open < 0 || strings.Index(shown, "[M1]") < open || strings.Index(shown, "[M3]") > open {
		t.Fatalf("OPEN section:\n%s", shown)
	}
	if b := strings.Index(shown, "BOUNDARIES ("); b < 0 || strings.Index(shown, "{M3} unaware: N2") < b {
		t.Fatalf("BOUNDARIES section:\n%s", shown)
	}
	if !strings.Contains(shown, "PRESENT (people in the current scene so far): N1, N2\n") {
		t.Fatalf("PRESENT line:\n%s", shown)
	}
	// a delivery's own selection (not a generation call) has no sections
	plain := RenderKnown(Select(k, "가람이 다온에게 말했다.", nil), Superseded(k))
	if strings.Contains(plain, "OPEN (") || strings.Contains(plain, "PRESENT (") || strings.Contains(plain, "BOUNDARIES (") {
		t.Fatalf("sections outside a generation call:\n%s", plain)
	}
}

// A response closed after its memories and written on in a second object
// (11-17, S T8: 23 of 24 memories were lost) is read as one; a second
// object that writes the memories again still replaces the first.
func TestParseReadsAResponseWrittenOnInASecondObject(t *testing.T) {
	out, _ := Parse("{\"story_date\":\"2900/08/15\",\"memories\":[{\"id\":\"m1\",\"kind\":\"event\",\"body\":\"a\"},{\"id\":\"m2\",\"kind\":\"fact\",\"at\":\"m1\",\"body\":\"b\"}]}\n{\"scenes\":[{\"ref\":\"S8\",\"change\":\"continue\"}],\"closed\":[{\"ref\":\"M3\",\"status\":\"fulfilled\"}],\"links\":[{\"from\":\"m1\",\"rel\":\"causes\",\"to\":\"m2\"}]}")
	if len(out.Memories) != 2 || len(out.Scenes) != 1 || len(out.Closed) != 1 || len(out.Links) != 1 || out.StoryDate != "2900/08/15" {
		t.Fatalf("memories %d scenes %d closed %d links %d date %q", len(out.Memories), len(out.Scenes), len(out.Closed), len(out.Links), out.StoryDate)
	}
	out, _ = Parse("{\"memories\":[{\"id\":\"m1\",\"kind\":\"event\",\"body\":\"draft\"}]}\n{\"memories\":[{\"id\":\"m1\",\"kind\":\"event\",\"body\":\"answer\"}]}")
	if len(out.Memories) != 1 || out.Memories[0].Body != "answer" {
		t.Fatalf("a draft then the answer: %+v", out.Memories)
	}
}

// A name of the answer under "on" gets the awareness on its entity, so an
// alias is protected on its relation as the store keeps it; a boundary that
// names nobody protects nothing; the caller's answer is left as read (11-17).
func TestFoldBoundaryOnAName(t *testing.T) {
	out, _ := Parse(`{"entities":[{"id":"n1","name":"바렌","same_as":"N1","relation":"r1"},{"id":"n2","name":"세린","same_as":"N9"}],
	 "memories":[{"id":"m1","kind":"fact","mode":"정체","about":"N1","body":"에드윈은 바렌으로 위장했다"},{"id":"m2","kind":"fact","body":"바렌은 전사다"}],
	 "boundaries":[
	  {"on":"n1","unaware":["N5"]},
	  {"on":"n2","hide_from":["N6"]},
	  {"on":"N7"}]}`)
	got, notes := Fold(out, Known{})
	if len(got.Entities[0].Unaware) != 1 || got.Entities[0].Unaware[0] != "N5" {
		t.Fatalf("alias under on: %+v", got.Entities[0].Awareness)
	}
	if len(got.Entities[1].HideFrom) != 1 || got.Entities[1].HideFrom[0] != "N6" {
		t.Fatalf("alias under on: %+v", got.Entities[1].Awareness)
	}
	for _, m := range got.Memories {
		if m.Ref == "n1" || m.Ref == "n2" || m.Ref == "N7" {
			t.Fatalf("a name of the answer or a boundary naming nobody became an update: %+v", m)
		}
		if m.ID == "m2" && len(m.Unaware)+len(m.HideFrom)+len(m.Knows) != 0 {
			t.Fatalf("m2 got a protection: %+v", m.Awareness)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("a boundary naming nobody is noted once: %v", notes)
	}
	if len(out.Entities[0].Unaware)+len(out.Entities[1].HideFrom) != 0 {
		t.Fatalf("the caller's entities were changed")
	}
}
