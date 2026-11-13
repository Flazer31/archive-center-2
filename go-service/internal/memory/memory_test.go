package memory

import (
	"strings"
	"testing"
)

func TestParseTakesTheFinalObjectAfterThinking(t *testing.T) {
	raw := "<think>draft {\"memories\":[{\"id\":\"m9\"}]}</think>\nHere:\n```json\n{\"memories\":[{\"id\":\"m1\",\"kind\":\"fact\",\"body\":\"a\",\"was\":[\"M2\",\"M3\"]}]}\n```"
	out, notes := Parse(raw)
	if len(notes) != 0 || len(out.Memories) != 1 || out.Memories[0].ID != "m1" || len(out.Memories[0].Was) != 2 {
		t.Fatalf("out %+v notes %v", out, notes)
	}
	// a draft object followed by the final one: the final one wins
	out, _ = Parse(`{"memories":[{"id":"m1","body":"draft"}]} final: {"memories":[{"id":"m1","body":"final"}]}`)
	if out.Memories[0].Body != "final" {
		t.Fatalf("got %q", out.Memories[0].Body)
	}
}

// A cut-off response keeps the complete items and notes the loss; nothing
// is filled in.
func TestParseSalvagesCompleteItemsOnly(t *testing.T) {
	raw := `{"entities":[{"id":"n1","name":"한얼"}],"memories":[{"id":"m1","kind":"event","body":"갚았다"},{"id":"m2","kind":"fact","bo`
	out, notes := Parse(raw)
	if len(out.Entities) != 1 || len(out.Memories) != 1 || out.Memories[0].Body != "갚았다" {
		t.Fatalf("out %+v", out)
	}
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "incomplete") || !strings.Contains(joined, "salvaged 2") {
		t.Fatalf("notes %v", notes)
	}
	if _, notes := Parse("no json here"); len(notes) != 1 {
		t.Fatalf("notes %v", notes)
	}
}

// An item written after the response object closed is not taken for the
// response (seen in a saved answer: the main object was lost to a stray
// scene item); the main object is read and the leftover is noted.
func TestParseIgnoresItemAfterTheObject(t *testing.T) {
	raw := "{\"memories\":[{\"id\":\"m1\",\"kind\":\"fact\",\"body\":\"a\"}]}\n{\"id\":\"s1\",\"change\":\"continue\",\"q\":\"x\"}"
	out, notes := Parse(raw)
	if len(out.Memories) != 1 || out.Memories[0].Body != "a" || !strings.Contains(strings.Join(notes, " "), "not read") {
		t.Fatalf("out %+v notes %v", out, notes)
	}
}

func sampleKnown() Known {
	return Known{
		Names:     []KnownName{{ID: "N1", Name: "강한얼", Type: "person"}, {ID: "N2", Name: "민재"}, {ID: "N3", Name: "서진"}},
		Relations: []KnownRelation{{ID: "R1", Name: "N2", SameAs: "N1", How: "disguise"}},
		Memories: []KnownMemory{
			{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "갚음", Roles: &Roles{Actor: []string{"N1"}}, Effects: []Effect{{About: "N1", Slot: "돈", Before: "50", Value: "20"}}}},
			{ID: "M2", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Slot: "돈", Body: "20냥", At: "M1"}},
			{ID: "M3", Turn: 2, Memory: Memory{Kind: "fact", About: "N1", Slot: "돈", Body: "10냥", Was: Refs{"M2"}}},
			{ID: "M4", Turn: 2, Memory: Memory{Kind: "fact", About: "N3", Body: "서진 사실"}},
		},
		Awareness: []KnownAwareness{{Target: "R1", Kind: "unaware", Knows: Knows{Who: "N3"}}},
	}
}

// Names written in the turn and names linked to them by an identity
// relation are the starting points: their current facts are shown, an
// earlier value is not, an event outside the scene gives only the current
// value it set, and records about other names are not shown.
func TestSelectFollowsNamesAndRelations(t *testing.T) {
	sel := Select(sampleKnown(), "민재가 웃었다", nil)
	ids := map[string]bool{}
	for _, n := range sel.Names {
		ids[n.ID] = true
	}
	for _, m := range sel.Memories {
		ids[m.ID] = true
	}
	// N3 is named because the shown relation says N3 does not know it; its
	// own record M4 is not shown
	if !ids["N2"] || !ids["N1"] || !ids["N3"] || ids["M1"] || ids["M2"] || !ids["M3"] || ids["M4"] {
		t.Fatalf("selected %v", ids)
	}
	if len(sel.Effects) != 1 || sel.Effects[0].Ref != "M1.1" {
		t.Fatalf("effects %+v", sel.Effects)
	}
	if len(sel.Relations) != 1 || len(sel.Awareness) != 1 {
		t.Fatalf("relations %d awareness %d", len(sel.Relations), len(sel.Awareness))
	}
}

// Events and views of the open scene are shown; outside it they are shown
// only when the turn recalls them.
func TestSelectScene(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "한얼"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "장터 구경", Roles: &Roles{Actor: []string{"N1"}}}},
		{ID: "M2", Turn: 1, Memory: Memory{Kind: "view", Owner: "N1", Aspect: "emotion", Body: "들뜸"}},
		{ID: "M3", Turn: 1, Memory: Memory{Kind: "view", Owner: "N1", Aspect: "goal", Body: "과거 급제"}},
		{ID: "M4", Turn: 2, Memory: Memory{Kind: "event", Body: "주막에 들어감", Roles: &Roles{Actor: []string{"N1"}}}},
		{ID: "M5", Turn: 2, Memory: Memory{Kind: "view", Owner: "N1", Aspect: "emotion", Body: "피곤함"}},
	}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 1}, {ID: "S1", Change: "close", Turn: 2}, {ID: "S2", Change: "open", Turn: 2}}}
	sel := Select(k, "한얼이 앉았다", nil)
	if r := Select(k, "한얼은 과거 급제를 떠올렸다", nil); len(r.Memories) != 3 || r.Memories[0].ID != "M3" {
		t.Fatalf("recalled goal: %+v", r.Memories)
	}
	var got []string
	for _, m := range sel.Memories {
		got = append(got, m.ID)
	}
	if strings.Join(got, ",") != "M4,M5" || sel.Scene != "S2" {
		t.Fatalf("selected %v scene %q", got, sel.Scene)
	}
}

func TestRenderMarksEarlierValuesAndEffects(t *testing.T) {
	k := sampleKnown()
	// the turn recalls the earlier value and the event
	out := RenderKnown(Select(k, "강한얼이 20냥을 갚음", nil), Superseded(k))
	if !strings.Contains(out, "[M2] T1 fact about N1 slot \"돈\": 20냥 | at M1 | earlier value") || strings.Contains(out, "[M3] T2 fact about N1 slot \"돈\": 10냥 | was M2 | earlier") {
		t.Fatalf("render:\n%s", out)
	}
	if !strings.Contains(out, "[M1.1] effect N1 \"돈\": 50 -> 20") || !strings.Contains(out, "{R1} unaware: N3") {
		t.Fatalf("render:\n%s", out)
	}
}

func TestSourceKeyFollowsTextAndRevision(t *testing.T) {
	a := Source{Session: "s", Turn: 1, Members: []SourceMember{{Key: "c:u", Data: "x"}}, Responses: []SourceResponse{{Key: "c:a", Seq: 1, Data: "r"}}}
	b := a
	b.Members = []SourceMember{{Key: "c:u", Data: "edited"}}
	c := a
	c.Responses = []SourceResponse{{Key: "c:a", Seq: 2, Data: "r"}}
	if a.Key() == b.Key() || a.Key() == c.Key() || a.Key() != (Source{Session: "s", Turn: 1, Members: a.Members, Responses: a.Responses}).Key() {
		t.Fatal("source key does not follow the input text and revision")
	}
	if (Source{Responses: []SourceResponse{{Seq: 0}}}).Readable() {
		t.Fatal("an unread response is not readable")
	}
}

// Salvage keeps every complete item of an array, not only the first, and a
// field of the wrong shape costs that field, not the item (seen in a saved
// answer: "unaware" written as objects lost the event and every item after
// it).
func TestSalvageKeepsAllItemsAndLeavesOutBadFields(t *testing.T) {
	raw := `{"memories":[{"id":"m1","kind":"event","body":"첫째","unaware":[17]},{"id":"m2","kind":"fact","body":"둘째"},{"id":"m3","kind":"fact","body":"셋째"},{"id":"m4","kind":"fact","bo`
	out, notes := Parse(raw)
	if len(out.Memories) != 3 || out.Memories[0].Body != "첫째" || out.Memories[2].Body != "셋째" || len(out.Memories[0].Unaware) != 0 {
		t.Fatalf("out %+v notes %v", out.Memories, notes)
	}
	if j := strings.Join(notes, " "); !strings.Contains(j, "left out of an item: unaware") || !strings.Contains(j, "salvaged 3") {
		t.Fatalf("notes %v", notes)
	}
}

// A person list written as objects naming the person (seen in a saved glm
// answer: hide_from [{"who": ...}]) keeps the people instead of losing the
// protection; a value that names no one is still a shape error.
func TestWhoListsAcceptPersonObjects(t *testing.T) {
	out, notes := Parse(`{"memories":[{"id":"m1","kind":"fact","body":"태블릿을 숨긴다","hide_from":[{"who":"조선 사람들"}],"unaware":"주모","seen_by":[{"name":"N2"},"N3"]}]}`)
	m := out.Memories[0]
	if len(notes) != 0 || len(m.HideFrom) != 1 || m.HideFrom[0] != "조선 사람들" || m.Unaware[0] != "주모" || len(m.SeenBy) != 2 || m.SeenBy[0] != "N2" {
		t.Fatalf("out %+v notes %v", m, notes)
	}
	out, notes = Parse(`{"memories":[{"id":"m1","kind":"fact","body":"x","hide_from":[{"why":"secret"}]}]}`)
	if len(out.Memories[0].HideFrom) != 0 || !strings.Contains(strings.Join(notes, " "), "hide_from") {
		t.Fatalf("no person: %+v %v", out.Memories[0], notes)
	}
}

// Records with the same body but different conditions, setting, people or
// obligation result give different documents.
func TestDocTextTellsSameBodiesApart(t *testing.T) {
	names := map[string]string{"N1": "민재", "N2": "서진"}
	doc := func(m Memory) string { return DocText(KnownMemory{ID: "M9", Memory: m}, names) }
	pairs := [][2]Memory{
		{{Kind: "fact", Mode: "의무", Body: "쌀을 갚는다", Terms: "비가 오면 내일"}, {Kind: "fact", Mode: "의무", Body: "쌀을 갚는다", Terms: "맑으면 내달"}},
		{{Kind: "event", Body: "만났다", Setting: "흐린 날", Roles: &Roles{Actor: []string{"N1"}}}, {Kind: "event", Body: "만났다", Setting: "맑은 날", Roles: &Roles{Actor: []string{"N2"}}}},
		{{Kind: "event", Body: "약속을 정리했다", Effects: []Effect{{About: "M3", Status: "fulfilled"}}}, {Kind: "event", Body: "약속을 정리했다", Effects: []Effect{{About: "M3", Status: "refused"}}}},
	}
	for i, p := range pairs {
		if a, b := doc(p[0]), doc(p[1]); a == b {
			t.Errorf("pair %d gives one document %q", i, a)
		}
	}
	if d := doc(Memory{Kind: "event", Body: "만났다", Roles: &Roles{Actor: []string{"N1"}}, Effects: []Effect{{About: "M3", Status: "fulfilled"}}}); strings.Contains(d, "M3") || !strings.Contains(d, "actor 민재") {
		t.Fatalf("ids in the document: %q", d)
	}
}

// An item whose JSON is broken (a saved glm answer closed a string with a
// single quote) is skipped and the items after it are read; nothing is
// taken from the next array.
func TestSalvageSkipsABrokenItemAndReadsOn(t *testing.T) {
	raw := `{"memories":[{"id":"m1","kind":"fact","body":"첫째"},{"id":"m2","kind":"fact","body":"깨진 문장.','q":"인용","to":"N1"},{"id":"m3","kind":"fact","body":"셋째"}],"scenes":[{"id":"s1","change":"open"}],"links":[{"from":"m1","rel":"causes","to":"m3"},{"from":"m3","rel":"cau`
	out, notes := Parse(raw)
	var bodies []string
	for _, m := range out.Memories {
		bodies = append(bodies, m.Body)
	}
	if strings.Join(bodies, ",") != "첫째,셋째" || len(out.Scenes) != 1 || len(out.Links) != 1 {
		t.Fatalf("memories %v scenes %d links %d notes %v", bodies, len(out.Scenes), len(out.Links), notes)
	}
	if j := strings.Join(notes, " "); !strings.Contains(j, "memories: 1 item(s) with broken JSON were skipped") || !strings.Contains(j, "links: an incomplete item was dropped") {
		t.Fatalf("notes %v", notes)
	}
}

// A model may write an effect's amounts as JSON numbers; they are kept as
// written, and other fields of the effect are not lost. A value that is
// neither a string nor a number is still a shape error.
func TestEffectAmountsMayBeNumbers(t *testing.T) {
	out, notes := Parse(`{"memories":[{"id":"m1","kind":"event","body":"샀다","effects":[{"about":"N1","slot":"돈","before":65,"value":61,"delta":-4,"was":"M3.1"}]}]}`)
	if len(out.Memories) != 1 || len(out.Memories[0].Effects) != 1 {
		t.Fatalf("out %+v notes %v", out, notes)
	}
	e := out.Memories[0].Effects[0]
	if e.Before != "65" || e.Value != "61" || e.Delta != "-4" || e.Slot != "돈" || len(e.Was) != 1 || len(notes) != 0 {
		t.Fatalf("effect %+v notes %v", e, notes)
	}
	out, notes = Parse(`{"memories":[{"id":"m1","kind":"event","body":"샀다","effects":[{"about":"N1","value":true}]}]}`)
	if len(out.Memories) != 1 || len(out.Memories[0].Effects) != 0 || !strings.Contains(strings.Join(notes, " "), "effects") {
		t.Fatalf("a boolean amount should be a shape error: %+v %v", out, notes)
	}
}

// A link written without "id" begins with "from"; after a broken link the
// next complete link is still read (seen: a key with no value, "q"}).
func TestSalvageReadsTheLinkAfterABrokenOne(t *testing.T) {
	raw := `{"memories":[{"id":"m1","kind":"event","body":"샀다"}],
 "links":[
 {"from":"m2","rel":"causes","to":"m1","q"},
 {"from":"m4","rel":"causes","to":"m5","q":"옷을 바꾸자"}]}`
	out, notes := Parse(raw)
	if len(out.Memories) != 1 || len(out.Links) != 1 || out.Links[0].From != "m4" || out.Links[0].To != "m5" {
		t.Fatalf("out %+v notes %v", out, notes)
	}
	if !strings.Contains(strings.Join(notes, " "), "links: 1 item(s) with broken JSON were skipped") {
		t.Fatalf("notes %v", notes)
	}
}

// An effect may name the record it changes (a memory or an effect) instead
// of the person. Its current value is shown when that record's subject is
// in the turn, as when the effect names the person. Records that point to
// each other do not hang the reading.
func TestSelectFollowsTheRecordAnEffectChanges(t *testing.T) {
	k := Known{
		Names: []KnownName{{ID: "N1", Name: "한얼"}, {ID: "N2", Name: "주모"}},
		Memories: []KnownMemory{
			{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Slot: "소지금", Body: "15냥이 있다"}},
			{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Body: "빌렸다", Effects: []Effect{{About: "M1", Slot: "소지금", Before: "15냥", Value: "65냥", Was: Refs{"M1"}}}}},
			{ID: "M3", Turn: 3, Memory: Memory{Kind: "event", Body: "샀다", Effects: []Effect{{About: "M2.1", Slot: "소지금", Before: "65냥", Value: "61냥", Was: Refs{"M2.1"}}}}},
			{ID: "M4", Turn: 3, Memory: Memory{Kind: "fact", About: "M5", Body: "고리 하나"}},
			{ID: "M5", Turn: 3, Memory: Memory{Kind: "event", Body: "고리 둘", Effects: []Effect{{About: "M4", Value: "x"}}}},
		},
	}
	sel := Select(k, "한얼은 길을 걸었다", nil)
	var refs []string
	for _, e := range sel.Effects {
		refs = append(refs, e.Ref)
	}
	if strings.Join(refs, ",") != "M3.1" {
		t.Fatalf("current value effects %v, want M3.1 (M2.1 was replaced)", refs)
	}
	// the model reads whose value it is, and that person's name
	known := RenderKnown(sel, Superseded(k))
	if !strings.Contains(known, "[M3.1] T3 effect M2.1 (of N1)") || !strings.Contains(known, "[N1] 한얼") {
		t.Fatalf("rendered:\n%s", known)
	}
	if sel = Select(k, "주모가 웃었다", nil); len(sel.Effects) != 0 {
		t.Fatalf("another person's turn shows %+v", sel.Effects)
	}
	// a later record that replaces the whole event replaces its effects
	k.Memories = append(k.Memories, KnownMemory{ID: "M6", Turn: 4, Memory: Memory{Kind: "fact", About: "N1", Slot: "소지금", Body: "61냥이 있다", Was: Refs{"M3"}}})
	sel = Select(k, "한얼은 길을 걸었다", nil)
	if len(sel.Effects) != 0 || !Superseded(k)["M3.1"] {
		t.Fatalf("effects of a replaced event: %+v", sel.Effects)
	}
}

// Following an effect's record: a chain of effects inside one record is
// followed to the end, a view's subject is its owner, and records that point
// to each other end at the first one passed again.
func TestEffectSubjectChains(t *testing.T) {
	k := Known{
		Names: []KnownName{{ID: "N1", Name: "한얼"}, {ID: "N2", Name: "박도준"}},
		Memories: []KnownMemory{
			{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "세 번 바뀜", Effects: []Effect{
				{About: "N1", Slot: "돈", Value: "1냥"}, {About: "M1.1", Value: "2냥"}, {About: "M1.2", Value: "3냥"}}}},
			{ID: "M2", Turn: 1, Memory: Memory{Kind: "view", Owner: "N1", About: "N2", Body: "믿는다"}},
			{ID: "M3", Turn: 2, Memory: Memory{Kind: "event", Body: "생각이 바뀜", Effects: []Effect{{About: "M2", Value: "의심"}}}},
			{ID: "M4", Turn: 2, Memory: Memory{Kind: "fact", About: "M5", Body: "고리"}},
			{ID: "M5", Turn: 2, Memory: Memory{Kind: "fact", About: "M4", Body: "고리"}},
			{ID: "M6", Turn: 2, Memory: Memory{Kind: "event", Body: "고리 변화", Effects: []Effect{{About: "M4", Value: "x"}}}},
		},
	}
	got := map[string]string{}
	for _, e := range Select(k, "한얼은 걸었다", nil).Effects {
		got[e.Ref] = e.Subject
	}
	if got["M1.3"] != "N1" || got["M3.1"] != "N1" {
		t.Fatalf("subjects %v", got)
	}
	if _, ok := got["M6.1"]; ok {
		t.Fatalf("a loop of records reached a name: %v", got)
	}
	for _, e := range Select(k, "박도준이 웃었다", nil).Effects {
		if e.Ref == "M3.1" {
			t.Fatalf("a view's change shown for the person it is about, not its owner")
		}
	}
}

// A memory item written in the entities array says what it is by its kind;
// it is read as a memory (whole response and salvage), and real names stay
// names.
func TestMemoryItemsInEntitiesAreMemories(t *testing.T) {
	whole := `{"entities":[{"id":"n1","name":"박도준","type":"person"},{"id":"m1","kind":"event","body":"술을 맛보았다"},{"id":"m2","kind":"view","owner":"n1","body":"좋다"},{"ref":"M7","q":"한 병"}],"memories":[{"id":"m3","kind":"fact","about":"n1","body":"상인이다"}]}`
	out, notes := Parse(whole)
	if len(out.Entities) != 1 || out.Entities[0].Name != "박도준" || len(out.Memories) != 4 || out.Memories[0].ID != "m3" || out.Memories[1].Body != "술을 맛보았다" || out.Memories[2].Owner != "n1" || out.Memories[3].Ref != "M7" {
		t.Fatalf("out %+v notes %v", out, notes)
	}
	if !strings.Contains(strings.Join(notes, " "), "entities: 3 memory item(s) were read as memories") {
		t.Fatalf("notes %v", notes)
	}
	// salvage: the response is cut off
	cut := `{"entities":[{"id":"m1","kind":"event","body":"술을 맛보았다"},{"id":"n1","name":"박도준"}],"memories":[{"id":"m2","kind":"fact","body":"끊`
	out, notes = Parse(cut)
	if len(out.Entities) != 1 || len(out.Memories) != 1 || out.Memories[0].Body != "술을 맛보았다" {
		t.Fatalf("salvage out %+v notes %v", out, notes)
	}
}

// A basis written as a string keeps the way the person knows it; an object
// reads as before, and a number is still a shape error.
func TestBasisMayBeAString(t *testing.T) {
	out, notes := Parse(`{"memories":[{"id":"m1","kind":"view","owner":"N1","body":"좋다","basis":"경험"},{"id":"m2","kind":"view","owner":"N1","body":"싫다","basis":{"via":"전언","from":"N2"}}]}`)
	if len(out.Memories) != 2 || out.Memories[0].Basis == nil || out.Memories[0].Basis.Via != "경험" || out.Memories[1].Basis.From != "N2" || len(notes) != 0 {
		t.Fatalf("out %+v notes %v", out, notes)
	}
	out, notes = Parse(`{"memories":[{"id":"m1","kind":"view","owner":"N1","body":"좋다","basis":3}]}`)
	if len(out.Memories) != 1 || out.Memories[0].Basis != nil || !strings.Contains(strings.Join(notes, " "), "basis") {
		t.Fatalf("number basis: %+v %v", out, notes)
	}
}

// A response array key written without its closing quote ("memories:[) is
// still read when salvaging; the items after it are kept.
func TestSalvageReadsAKeyMissingItsClosingQuote(t *testing.T) {
	raw := `{"entities":[{"id":"n1","name":"장정"}],"memories:[{"id":"m1","kind":"event","body":"장정 열 명을 모았다"},{"id":"m2","kind":"fact","about":"n1","body":"품삯 닷 푼을 더 준다"}],"scenes":[{"id":"s1","change":"open"}]}`
	out, notes := Parse(raw)
	if len(out.Entities) != 1 || len(out.Memories) != 2 || out.Memories[1].Body != "품삯 닷 푼을 더 준다" || len(out.Scenes) != 1 {
		t.Fatalf("out %+v notes %v", out, notes)
	}
}

// The same words inside a string value are not an array key, also when a
// response is salvaged: the items stay where they are and a complete
// response is not replaced by a cut-off tail.
func TestArrayKeyWordsInsideAStringAreNotKeys(t *testing.T) {
	for _, name := range []string{"entities", "memories", "scenes", "links"} {
		raw := `{"memories":[{"id":"m1","kind":"fact","body":"본문에 \"` + name + `:[ 예시"},{"id":"m2","kind":"fact","body":"뒤의 정상 항목"}],"scenes":3}`
		out, notes := Parse(raw)
		if len(out.Memories) != 2 || out.Memories[1].Body != "뒤의 정상 항목" || len(out.Links) != 0 {
			t.Fatalf("%s: out %+v notes %v", name, out, notes)
		}
	}
	cut := `{"memories":[{"id":"m1","kind":"fact","body":"본문에 \"links:[ 예시"},{"id":"m2","kind":"fact","body":"둘"}],"scenes":[{"id":"s1","change":"op`
	if out, notes := Parse(cut); len(out.Memories) != 2 || len(out.Links) != 0 {
		t.Fatalf("cut: out %+v notes %v", out, notes)
	}
	tail := `{"memories":[{"id":"m1","kind":"fact","body":"앞의 완성 응답"}]} {"note":"본문에 \"memories:[ 예시`
	if out, notes := Parse(tail); len(out.Memories) != 1 || out.Memories[0].Body != "앞의 완성 응답" {
		t.Fatalf("tail: out %+v notes %v", out, notes)
	}
}
