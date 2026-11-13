package memory

import (
	"archivecenter2/goservice/internal/lifecycle"
	"fmt"
	"strings"
	"testing"
)

func TestA25ParseFinalShapeSalvage(t *testing.T) {
	raw := `{"memories":[{"id":"m1","kind":"fact","body":"초안"}]} final: {"memories":[{"id":"m2","kind":"fact","body":"최종 정상"},{"id":"m3","body":17}]}`
	out, notes := Parse(raw)
	t.Logf("out=%+v notes=%v", out, notes)
	// the wrong-shaped body of m3 is left out and noted; m3 itself stays
	// (a bad field costs the field, not the item)
	if len(out.Memories) != 2 || out.Memories[0].Body != "최종 정상" || out.Memories[1].ID != "m3" || out.Memories[1].Body != "" ||
		!strings.Contains(strings.Join(notes, " "), "left out of an item: body") {
		t.Fatal("final complete item replaced by draft")
	}
	for _, m := range out.Memories {
		if m.Body == "초안" {
			t.Fatal("draft item read")
		}
	}
}

func TestA25ParseLiteralThinkInQuote(t *testing.T) {
	raw := `{"memories":[{"id":"m1","kind":"fact","body":"태그 <think>열쇠</think>를 썼다","q":"<think>열쇠</think>"}]}`
	out, notes := Parse(raw)
	t.Logf("out=%+v notes=%v", out, notes)
	if len(out.Memories) != 1 || out.Memories[0].Q != "<think>열쇠</think>" {
		t.Fatal("literal content rewritten before parsing")
	}
}

func TestA25SelectReferenceTargets(t *testing.T) {
	for _, tc := range []struct {
		name string
		root Memory
	}{
		{"effect_about", Memory{Kind: "event", Roles: &Roles{Actor: []string{"N1"}}, Effects: []Effect{{About: "M1", Status: "fulfilled"}}}},
		{"basis_ref", Memory{Kind: "view", Owner: "N1", Basis: &Basis{Ref: "M1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Memories: []KnownMemory{
				{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "근거", Effects: []Effect{{Value: "10"}}}},
				{ID: "M2", Turn: 2, Memory: tc.root},
			}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 2}}}
			sel := Select(k, "민재", nil)
			t.Logf("selected=%+v", sel.Memories)
			for _, m := range sel.Memories {
				if m.ID == "M1" {
					return
				}
			}
			t.Fatal("referenced record missing")
		})
	}
}

func TestA25SelectAtChainOrder(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "원근거"}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "fact", Body: "중간", At: "M1"}},
		{ID: "M3", Turn: 3, Memory: Memory{Kind: "view", Owner: "N1", Body: "생각", At: "M2"}},
	}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 3}}}
	a := Select(k, "민재", nil)
	k.Memories[0], k.Memories[2] = k.Memories[2], k.Memories[0]
	b := Select(k, "민재", nil)
	t.Logf("chronological=%+v reversed=%+v", a.Memories, b.Memories)
	if len(a.Memories) != 3 || len(a.Memories) != len(b.Memories) {
		t.Fatal("at closure depends on insertion order")
	}
}

func TestA25RenderSemanticDistinctions(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b Memory
	}{
		{"past_current", Memory{Kind: "event", Body: "돈 10냥", Time: "이틀 전"}, Memory{Kind: "event", Body: "돈 10냥", Time: "지금"}},
		{"view_source", Memory{Kind: "view", Body: "믿는다", Basis: &Basis{Via: "전언", From: "N2", Confidence: "아마"}, Expressed: "속으로만", Differs: "yes"}, Memory{Kind: "view", Body: "믿는다", Basis: &Basis{Via: "경험", From: "N3", Confidence: "확실"}, Expressed: "고백", Differs: "no"}},
		{"scope_to", Memory{Kind: "fact", Body: "도와준다", To: Who{"N2"}, Scope: "여행 중"}, Memory{Kind: "fact", Body: "도와준다", To: Who{"N3"}, Scope: "집에서"}},
		{"attempt_success", Memory{Kind: "event", Body: "문 열기", Outcome: "attempt"}, Memory{Kind: "event", Body: "문 열기", Outcome: "success"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			render := func(m Memory) string {
				return RenderKnown(Selection{Memories: []KnownMemory{{ID: "M1", Turn: 1, Memory: m}}}, nil)
			}
			a, b := render(tc.a), render(tc.b)
			t.Logf("a=%q b=%q", a, b)
			if a == b {
				t.Fatal("different recorded meanings become identical model input")
			}
		})
	}
}

func TestA25GenerationCatchupControls(t *testing.T) {
	snap := func(src lifecycle.Source, gen, data string, unread bool) lifecycle.Snapshot {
		return lifecycle.Snapshot{SessionKey: "s", Source: src, TargetIndex: 1, Rows: []lifecycle.Row{{Index: 0, ChatID: "u", Role: "user", Data: "입력"}, {Index: 1, ChatID: "a", Role: "char", GenerationID: gen, Data: data, Unreadable: unread}}}
	}
	next := func(gen string, unread bool) lifecycle.Snapshot { // the next input confirms the turn
		sn := snap(lifecycle.SourceBeforeRequest, gen, "", unread)
		sn.Rows = append(sn.Rows, lifecycle.Row{Index: 2, ChatID: "n", Role: "user", Data: "다음"})
		return sn
	}
	s := lifecycle.NewSession("s")
	s.Apply(snap(lifecycle.SourceOutputEvent, "g1", "동일 본문", false))
	s.Apply(snap(lifecycle.SourceBeforeRequest, "g2", "", true))
	s.Apply(snap(lifecycle.SourceOutputEvent, "g2", "", true))
	s.Apply(next("g2", true))
	s.Apply(snap(lifecycle.SourceLoad, "g2", "동일 본문", false))
	turn := s.ActiveTurns()[0]
	seq := turn.Responses[0].Revisions[len(turn.Responses[0].Revisions)-1].Seq
	s.Apply(snap(lifecycle.SourceLoad, "g2", "동일 본문", false))
	// only the newest text is kept: the caught-up text is the row's one
	// text, confirmed; a retry of it makes no new text (same seq)
	if len(s.ActiveTurns()) != 1 || len(turn.Responses[0].Revisions) != 1 || !turn.Confirmed || !turn.Responses[0].Revisions[0].Confirmed || seq != turn.Responses[0].Revisions[0].Seq {
		t.Fatalf("catch-up/retry %+v", turn)
	}
	s.Apply(snap(lifecycle.SourceLoad, "g2", "편집 본문", false))
	if revs := turn.Responses[0].Revisions; len(revs) != 1 || revs[0].Data != "편집 본문" || revs[0].Seq <= seq || !turn.Confirmed || revs[0].Confirmed {
		t.Fatal("same-generation edit changed completion contract")
	}
}

func TestA25UnknownFieldsDiagnosed(t *testing.T) {
	out, notes := Parse(`{"memories":[{"id":"m1","kind":"fact","body":"본문","unexpected_detail":"실제 값"}]}`)
	if len(out.Memories) != 1 || out.Memories[0].Body != "본문" {
		t.Fatal("body lost")
	}
	if !strings.Contains(strings.Join(notes, " "), "unexpected_detail") {
		t.Fatalf("unknown field silent: notes=%v", notes)
	}
}

// An earlier value (the event whose effect a later effect replaced) is not
// brought by the later record; it is shown when the turn recalls it.
func TestEarlierValueShownOnlyWhenRecalled(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "거지 시절 빈털터리", Effects: []Effect{{About: "N1", Slot: "돈", Value: "0냥"}}}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Body: "품삯을 받음", Effects: []Effect{{About: "N1", Slot: "돈", Before: "0냥", Value: "10냥", Was: Refs{"M1.1"}}}}},
	}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 2}}}
	shown := func(sel Selection, id string) bool {
		for _, m := range sel.Memories {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	sel := Select(k, "민재가 웃었다", nil)
	if shown(sel, "M1") || !shown(sel, "M2") || len(sel.Effects) != 0 {
		t.Fatalf("without recall: %+v effects %+v", sel.Memories, sel.Effects)
	}
	if out := RenderKnown(sel, Superseded(k)); !strings.Contains(out, `[M2.1] effect N1 "돈": 0냥 -> 10냥 | was M1.1`) || strings.Contains(out, "[M1]") {
		t.Fatalf("render:\n%s", out)
	}
	if sel := Select(k, "민재는 거지 시절을 떠올렸다", nil); !shown(sel, "M1") {
		t.Fatalf("recalled earlier value missing: %+v", sel.Memories)
	}
}

// Restored 11-09 with SelectWithin (from 11-01 b062cf3/2fb4c94).
func TestSelectFactDoesNotBringItsSourceEvent(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "민재가 장터에 도착했다"}},
		{ID: "M2", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Slot: "나이", Body: "민재는 열한 살이다", At: "M1"}},
	}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 5}}}
	if all := Select(k, "민재", nil); len(all.Memories) != 2 {
		t.Fatalf("Select changed: shown %+v", all.Memories)
	}
	// under its ceiling a generation call reads what Select shows (11-09)
	if under, n := SelectWithin(k, "민재", nil, 0); len(under.Memories) != 2 || n != 0 {
		t.Fatalf("no ceiling: shown %+v, left %d", under.Memories, n)
	}
	size := len([]rune(RenderKnown(Select(k, "민재", nil), Superseded(k))))
	if under, n := SelectWithin(k, "민재", nil, size); len(under.Memories) != 2 || n != 0 {
		t.Fatalf("at its size: shown %+v, left %d", under.Memories, n)
	}
	// over it, the source event goes first
	sel, n := SelectWithin(k, "민재", nil, size-1)
	if len(sel.Memories) != 1 || sel.Memories[0].ID != "M2" || n != 1 {
		t.Fatalf("shown %+v (left %d), want the fact alone", sel.Memories, n)
	}
	if out := RenderKnown(sel, Superseded(k)); !strings.Contains(out, "| at M1") {
		t.Fatalf("the fact's line lost its source id:\n%s", out)
	}
}

func TestSelectWithinLeavesOutByOrder(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}, {ID: "N2", Name: "서윤"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "민재가 엽전 서른 냥을 받았다", Effects: []Effect{{About: "N1", Slot: "돈", Value: "30냥"}}}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "fact", About: "N1", Slot: "나이", Body: "민재는 열한 살이다"}},
		{ID: "M3", Turn: 2, Memory: Memory{Kind: "event", Body: "청동열쇠 분실"}},
		{ID: "M4", Turn: 3, Memory: Memory{Kind: "fact", Mode: "의무", About: "N1", Body: "민재는 서윤에게 책을 돌려주기로 했다"}},
		{ID: "M5", Turn: 5, Memory: Memory{Kind: "view", Owner: "N2", Body: "서윤은 민재가 부자라고 생각한다", Basis: &Basis{Ref: "M1"}}},
	}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 5}}}
	text := "민재가 청동열쇠를 찾았다"
	shown := func(sel Selection) string {
		var ids []string
		for _, m := range sel.Memories {
			ids = append(ids, m.ID)
		}
		return strings.Join(ids, ",")
	}
	sup := Superseded(k)
	full, _ := SelectWithin(k, text, nil, 0)
	if got := shown(full); got != "M1,M2,M3,M4,M5" {
		t.Fatalf("without a ceiling shown %s", got)
	}
	// from any ceiling down to 1, what is left out is a prefix of this
	// order (linked, recalled, named fact, scene record), and the open
	// obligation M4 always stays; at the full size nothing goes
	order := []string{"M1", "M3", "M2", "M5"}
	size := len([]rune(RenderKnown(full, sup)))
	if sel, n := SelectWithin(k, text, nil, size); n != 0 || shown(sel) != "M1,M2,M3,M4,M5" {
		t.Fatalf("at the full size left out %d", n)
	}
	seen := map[int]bool{}
	for max := size - 1; max >= 1; max-- {
		sel, n := SelectWithin(k, text, nil, max)
		gone := map[string]bool{}
		for _, id := range order[:n] {
			gone[id] = true
		}
		var want []string
		for _, id := range []string{"M1", "M2", "M3", "M4", "M5"} {
			if !gone[id] {
				want = append(want, id)
			}
		}
		if n < 1 || n > len(order) || shown(sel) != strings.Join(want, ",") {
			t.Fatalf("max %d: shown %s left out %d, want %s", max, shown(sel), n, strings.Join(want, ","))
		}
		if !strings.Contains(RenderKnown(sel, sup), "[M1.1] T1 effect N1 \"돈\"") {
			t.Fatalf("max %d: the left-out event's current value is not shown:\n%s", max, RenderKnown(sel, sup))
		}
		seen[n] = true
	}
	for n := 1; n <= len(order); n++ {
		if !seen[n] {
			t.Fatalf("no ceiling left out exactly %d records: %v", n, seen)
		}
	}
}

// A record kept from someone (hide_from, unaware) is never left out, so
// its protection stays with it however low the ceiling (11-01 scene
// records became removable, protected ones did not).
func TestSelectWithinKeepsProtectedRecord(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}, {ID: "N2", Name: "서윤"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "금고 암호는 1234다"}},
		{ID: "M2", Turn: 5, Memory: Memory{Kind: "view", Owner: "N1", Body: "민재는 금고 암호 1234를 기억한다", Basis: &Basis{Ref: "M1"}}},
		{ID: "M3", Turn: 6, Memory: Memory{Kind: "event", Body: "민재가 문을 열었다"}},
	}, Awareness: []KnownAwareness{{Target: "M1", Kind: "hide_from", Knows: Knows{Who: "N2"}}, {Target: "M3", Kind: "unaware", Knows: Knows{Who: "N2"}}},
		Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 5}}}
	sup := Superseded(k)
	sel, n := SelectWithin(k, "민재", nil, 1)
	out := RenderKnown(sel, sup)
	var ids []string
	for _, m := range sel.Memories {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "M1,M3" || n != 1 {
		t.Fatalf("shown %v left out %d, want the protected M1 and M3 kept and the view left out:\n%s", ids, n, out)
	}
	if !strings.Contains(out, "{M1} hide_from: N2") || !strings.Contains(out, "{M3} unaware: N2") || !strings.Contains(out, "[N2] 서윤") {
		t.Fatalf("protection lost:\n%s", out)
	}
}

// An open obligation stays under the ceiling whatever brought it: here it
// is only recalled by a word of the turn.
func TestSelectWithinKeepsRecalledOpenObligation(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N2", Name: "서윤"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N2", Mode: "의무", Status: "active", Body: "보라등을 돌려준다"}},
	}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 5}}}
	full, _ := SelectWithin(k, "보라등", nil, 0)
	if len(full.Memories) != 1 {
		t.Fatalf("the obligation is not recalled: %+v", full.Memories)
	}
	sel, n := SelectWithin(k, "보라등", nil, 1)
	if len(sel.Memories) != 1 || n != 0 {
		t.Fatalf("the open obligation was left out: %+v (%d)", sel.Memories, n)
	}
}

// Under the ceiling after one record goes, no second one is left out: the
// first took its awareness and name with it.
func TestSelectWithinStopsWhenUnder(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재", Turn: 1}, {ID: "N2", Name: "서윤", Turn: 1}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Slot: "a", Body: "오래된 사실"}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "fact", About: "N1", Slot: "b", Body: "유지할 사실"}},
	}, Awareness: []KnownAwareness{{Target: "M1", Kind: "knows", Knows: Knows{Who: "N2", Part: strings.Repeat("가", 200)}}},
		Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 5}}}
	sup := Superseded(k)
	full, _ := SelectWithin(k, "민재", nil, 0)
	size := len([]rune(RenderKnown(full, sup)))
	sel, n := SelectWithin(k, "민재", nil, size-50)
	if n != 1 || len(sel.Memories) != 1 || sel.Memories[0].ID != "M2" {
		t.Fatalf("left out %d, shown %+v", n, sel.Memories)
	}
}

// A shown record keeps the protection of an earlier value it replaced,
// though that value is not shown (11-01, a 20,000 ceiling on a 183-turn
// chat lost "{M1484} unaware" of a replaced HP value).
func TestSelectWithinKeepsProtectionOfReplacedValue(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}, {ID: "N2", Name: "서윤"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Slot: "HP", Body: "HP 62"}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "fact", About: "N1", Slot: "HP", Body: "HP 54", Was: Refs{"M1"}}},
	}, Awareness: []KnownAwareness{{Target: "M1", Kind: "unaware", Knows: Knows{Who: "N2"}}},
		Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 5}}}
	sel, _ := SelectWithin(k, "민재", nil, 0)
	out := RenderKnown(sel, Superseded(k))
	if !strings.Contains(out, "[M2]") || strings.Contains(out, "[M1]") || !strings.Contains(out, "{M1} unaware: N2") {
		t.Fatalf("want M2 shown, M1 not, and M1's protection kept:\n%s", out)
	}
}

// An event left out under the ceiling still shows its value on a line of
// its own; that line keeps the protection of the earlier value it
// replaced.
func TestSelectWithinKeepsProtectionOfValueLine(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}, {ID: "N2", Name: "서윤"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "기존 체력 기록", Effects: []Effect{{About: "N1", Slot: "HP", Value: "62"}}}},
		{ID: "M2", Turn: 5, Memory: Memory{Kind: "event", Body: "체력이 줄었다", Effects: []Effect{{About: "N1", Slot: "HP", Before: "62", Value: "54", Was: Refs{"M1.1"}}}}},
	}, Awareness: []KnownAwareness{{Target: "M1.1", Kind: "unaware", Knows: Knows{Who: "N2"}}},
		Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 5}}}
	sup := Superseded(k)
	full, _ := SelectWithin(k, "민재", nil, 0)
	size := len([]rune(RenderKnown(full, sup)))
	sel, n := SelectWithin(k, "민재", nil, size-1)
	out := RenderKnown(sel, sup)
	if n != 1 || len(sel.Memories) != 0 || !strings.Contains(out, "[M2.1] T5 effect") {
		t.Fatalf("want M2 left out with its value on its own line (%d left out):\n%s", n, out)
	}
	if !strings.Contains(out, "{M1.1} unaware: N2") || !strings.Contains(out, "[N2] 서윤") {
		t.Fatalf("the replaced value's protection was lost:\n%s", out)
	}
}

// Leaving out more is not always shorter (a left-out event's values each
// gain a turn on their own lines): when leaving out fewer fits, that is
// taken.
func TestSelectWithinFitsWhenMoreIsLonger(t *testing.T) {
	var effects []Effect
	for i := 1; i <= 10; i++ {
		effects = append(effects, Effect{About: "N1", Slot: fmt.Sprintf("항목%d", i), Value: fmt.Sprintf("값%d", i)})
	}
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Body: strings.Repeat("가", 100)}},
		{ID: "M2", Turn: 5, Memory: Memory{Kind: "event", Body: "문 열기", Effects: effects}},
	}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 5}}}
	sup := Superseded(k)
	one, _ := SelectWithin(k, "민재", nil, 0)
	// the size with M1 alone left out, measured through the ceiling itself
	var withoutM1 int
	for max := len([]rune(RenderKnown(one, sup))) - 1; max > 0; max-- {
		if sel, n := SelectWithin(k, "민재", nil, max); n == 1 {
			withoutM1 = len([]rune(RenderKnown(sel, sup)))
			break
		}
	}
	sel, n := SelectWithin(k, "민재", nil, withoutM1)
	if size := len([]rune(RenderKnown(sel, sup))); size > withoutM1 || n != 1 {
		t.Fatalf("ceiling %d: got %d chars with %d left out", withoutM1, size, n)
	}
}

// An event whose values on lines of their own are as long as the event
// shown whole is not left out (leaving it out made the range longer), so
// one record out fits where it fits.
func TestSelectWithinLeavesOutOnlyWhatShortens(t *testing.T) {
	k := Known{Names: []KnownName{{ID: "N1", Name: "민재"}}, Scenes: []KnownScene{{ID: "S1", Change: "open", Turn: 1}},
		Memories: []KnownMemory{{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Body: strings.Repeat("가", 20)}}}}
	for i := 2; i <= 4; i++ {
		var es []Effect
		for j := 0; j < 10; j++ {
			es = append(es, Effect{About: "N1", Slot: fmt.Sprintf("%d항목%d", i, j), Value: "값"})
		}
		k.Memories = append(k.Memories, KnownMemory{ID: fmt.Sprintf("M%d", i), Turn: i, Memory: Memory{Kind: "event", Body: "값 변경", Effects: es}})
	}
	sup := Superseded(k)
	control := k
	control.Memories = append([]KnownMemory(nil), k.Memories[1:]...)
	withoutM1 := len([]rune(RenderKnown(Select(control, "민재", nil), Superseded(control))))
	if all := len([]rune(RenderKnown(Select(k, "민재", nil), sup))); all <= withoutM1 {
		t.Fatalf("fixture: all %d, without M1 %d", all, withoutM1)
	}
	sel, left := SelectWithin(k, "민재", nil, withoutM1)
	size := len([]rune(RenderKnown(sel, sup)))
	if size > withoutM1 || left != 1 {
		t.Fatalf("size %d over %d, left %d; want M1 alone left out", size, withoutM1, left)
	}
	for _, m := range sel.Memories {
		if m.ID == "M1" {
			t.Fatal("M1 shown")
		}
	}
}
