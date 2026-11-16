package store

import (
	"strings"
	"testing"
	"unicode/utf8"

	"archivecenter2/goservice/internal/memory"
)

// Through production Deliver (1.2 general lane, deliver_general.go). The
// stored records are invented; no input vector, so each kind keeps the
// weak recency order and ties keep the stored order.

const generalRaw = `{"entities":[{"id":"n1","name":"리나","type":"person"},{"id":"n2","name":"마크","type":"person"},{"id":"n3","name":"도윤","type":"person"}],"memories":[
 {"id":"m1","kind":"event","body":"리나가 장터에서 소금 상인과 오래 흥정하다가 값을 반으로 깎고 소금 두 자루를 샀다","roles":{"actor":["n1"]}},
 {"id":"m2","kind":"event","body":"리나가 장터 우물가에서 물을 긷던 아이들에게 길을 묻고 동쪽 다리를 건넜다","roles":{"actor":["n1"]}},
 {"id":"m3","kind":"fact","about":"n1","slot":"일","mode":"상태","body":"리나는 장터에서 약방을 한다"},
 {"id":"m4","kind":"fact","about":"n2","slot":"일","mode":"상태","body":"마크는 장터 밖 북쪽 성채에서 문지기를 한다"},
 {"id":"m5","kind":"fact","about":"n1","slot":"출신","mode":"상태","body":"리나는 사실 북쪽 왕가의 막내다","hide_from":["n3"]}]}`

func deliverGeneral(t *testing.T, chars int) (Delivery, map[string]string) {
	t.Helper()
	db := testDB(t)
	input := "리나가 장터에 돌아왔다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"오늘", "리나와 도윤이 약방 앞을 쓸었다"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), generalRaw)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 2)), Chars: chars, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 1 {
		t.Fatalf("fixture: window %d", d.Window)
	}
	ids := map[string]string{}
	for i, name := range []string{"event1", "event2", "fact", "mark", "secret"} {
		ids[name] = it.Memories[i].ID
	}
	return d, ids
}

// A current fact about the person the input names comes before the events
// stored before it. The shown turn names 리나 too, so 1.1's entering-first
// does not move her fact; the single rank took the events first on a tie.
func TestDeliverGeneralFactBeforeEvents(t *testing.T) {
	d, ids := deliverGeneral(t, 2000)
	var general []string
	for _, id := range d.IDs {
		if !d.SecretIDs[id] {
			general = append(general, id)
		}
	}
	if len(general) < 3 || general[0] != ids["fact"] {
		t.Fatalf("general lane %v, want 리나's fact %s first", general, ids["fact"])
	}
}

// With no anchor at all (the input names no one, the shown turn has no
// records), Select's candidates are still the general lane's records: the
// event the input's words recall is delivered.
func TestDeliverGeneralSelectWithoutAnchors(t *testing.T) {
	db := testDB(t)
	input := "그래서 소금 흥정은 어떻게 됐지"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"memories":[{"id":"m1","kind":"event","body":"소금 흥정은 값을 반으로 깎고 끝났다"}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 2)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.IDs) != 1 || d.IDs[0] != it.Memories[0].ID {
		t.Fatalf("recalled event %s not delivered: %v", it.Memories[0].ID, d.IDs)
	}
}

// The secret lane keeps the single rank: the hidden fact is delivered as a
// secret, never in the general lane.
func TestDeliverGeneralSecretLaneKept(t *testing.T) {
	d, ids := deliverGeneral(t, 2000)
	if !d.SecretIDs[ids["secret"]] {
		t.Fatalf("secret %s not in the secret lane: %v %v", ids["secret"], d.IDs, d.SecretIDs)
	}
}

// The general lane counts the line sent (and its break), the secret lane
// the full record when that is longer.
func TestDeliverGeneralCountsLineSent(t *testing.T) {
	d, ids := deliverGeneral(t, 2000)
	line := map[string]string{}
	for i, id := range d.IDs {
		line[id] = d.Lines[i]
	}
	for _, r := range d.Trace {
		if r.Result != "taken" || r.Secret {
			continue
		}
		if want := utf8.RuneCountInString(line[r.ID]) + 1; r.Size != want {
			t.Fatalf("%s counted %d, line sent %d", r.ID, r.Size, want)
		}
	}
	if d.Chars == 0 {
		t.Fatalf("fixture: no general record delivered: %v", ids)
	}
}

// An independent fact recorded at a secret event keeps the event's
// protection on its own line (Sol review 1: it was shown in the general
// lane without it). It is not a secret itself: it is counted in the general
// lane, so it comes with or without secret room, always shown protected.
func TestDeliverIndependentFactKeepsEventProtection(t *testing.T) {
	db := testDB(t)
	input := "리나의 출신을 떠올렸다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나","type":"person"},{"id":"n2","name":"도윤","type":"person"}],"memories":[
	 {"id":"m1","kind":"event","body":"리나가 왕가의 문장을 몰래 꺼내 보았다","roles":{"actor":["n1"]},"hide_from":["n2"]},
	 {"id":"m2","kind":"fact","about":"n1","slot":"출신","mode":"상태","at":"m1","body":"리나는 왕가의 막내다"}]}`)
	fact := it.Memories[1].ID
	for _, secretRoom := range []int{4000, 0} {
		d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: secretRoom})
		if err != nil {
			t.Fatal(err)
		}
		for i, id := range d.IDs {
			if id == fact && (!d.SecretIDs[id] || !strings.Contains(d.Lines[i], "hide from: 도윤")) {
				t.Fatalf("secret room %d: fact %s shown without its event's protection: %q", secretRoom, id, d.Lines[i])
			}
		}
		if !d.SecretIDs[fact] {
			t.Fatalf("secret room %d: protected fact %s not delivered: %v", secretRoom, fact, d.IDs)
		}
		if d.SecretChars > secretRoom {
			t.Fatalf("secret room %d: counted %d in the secret lane", secretRoom, d.SecretChars)
		}
	}
}

// With nothing for Select to find (the input names no one and recalls no
// words), the people of the shown turn's records still anchor the general
// lane (Sol review 6: Deliver returned before building it).
func TestDeliverRosterWithoutSelect(t *testing.T) {
	db := testDB(t)
	input := "그래서?"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, old, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[
	 {"id":"m1","kind":"fact","about":"n1","slot":"일","mode":"상태","body":"리나는 약방을 한다"}]}`)
	a24Save(t, db, turnSource(s, 2), `{"memories":[{"id":"m1","kind":"event","body":"그가 문을 닫았다","roles":{"actor":["`+old.Names[0].ID+`"]}}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 2)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if !has(d.IDs, old.Memories[0].ID) {
		t.Fatalf("리나's fact %s not delivered from the shown turn's people: %v (candidates %d)", old.Memories[0].ID, d.IDs, d.Candidates)
	}
}

// Two partial knowings of different parts by the same person stay two
// (Sol review 2: the duplicate key left out the part).
func TestDistinctAwarenessKeepsParts(t *testing.T) {
	a := func(part string) memory.KnownAwareness {
		return memory.KnownAwareness{Target: "M1", Kind: "knows", Knows: memory.Knows{Who: "N1", Via: "전언", Extent: "부분", Part: part}}
	}
	got := distinctAwareness([]memory.KnownAwareness{a("장소"), a("날짜"), a("장소")})
	if len(got) != 2 || got[0].Part != "장소" || got[1].Part != "날짜" {
		t.Fatalf("parts: %+v", got)
	}
}

// A view shown with protection it only inherits is counted in the general
// lane together with the chars its basis reference adds to the source line
// (Sol review 2): at a ceiling one char short of both, the line falls back
// to quoting, which does not fit either, and the lane stays under it.
func TestDeliverInheritedBasisCountsSourceChars(t *testing.T) {
	db := testDB(t)
	input := "리나가 약방을 떠올렸다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나","type":"person"},{"id":"n2","name":"도윤","type":"person"}],"memories":[
	 {"id":"m1","kind":"event","body":"리나가 몰래 왕가의 문장을 보았다","roles":{"actor":["n1"]},"hide_from":["n2"]},
	 {"id":"m2","kind":"fact","about":"n1","slot":"일","mode":"상태","body":"리나는 장터에서 약방을 한다"},
	 {"id":"m3","kind":"view","owner":"n1","about":"n1","aspect":"emotion","at":"m1","body":"리나는 약방이 자랑스럽다","basis":{"ref":"m2"}}]}`)
	fact, view := it.Memories[1].ID, it.Memories[2].ID
	req := DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000}
	wide, err := db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	size := map[string]int{}
	for _, r := range wide.Trace {
		size[r.ID] = r.Size
	}
	if !has(wide.IDs, fact) || !has(wide.IDs, view) || !wide.SecretIDs[view] {
		t.Fatalf("fixture: the view should be shown protected: %v %v", wide.IDs, wide.SecretIDs)
	}
	added := len([]rune(`[text B1=""]`))
	if wide.Chars != size[fact]+size[view]+added {
		t.Fatalf("fixture: the view should refer to the fact's line: chars %d, sizes %d+%d+%d", wide.Chars, size[fact], size[view], added)
	}
	req.Chars = size[fact] + size[view] + added - 1
	d, err := db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Chars > req.Chars {
		t.Fatalf("general lane %d over its ceiling %d: %v", d.Chars, req.Chars, d.IDs)
	}
}

// The general order's single read gives the similarities similarities
// gives and the unit-vector sum sceneContrast centers on; scoring against
// that sum gives sceneContrast's scores (1.2 latency: the general records
// are read once for both, not three times).
func TestSimilaritiesWithSumMatchesSeparateReads(t *testing.T) {
	f := &contrastFixture{vectors: map[int64][]float32{1: {3, 4}, 2: {1, 0}, 3: {0, 2}, 4: {-1, 1}, 5: {0, 0}}}
	db := fixtureDB(t, f)
	req := DeliveryRequest{Model: "synthetic", Query: []float32{2, 1}}
	ids := []string{"M1", "M2", "M3", "M4", "M5", "M6"}
	sim, sum, count, err := db.similaritiesWithSum(req.Model, req.Query, ids)
	if err != nil {
		t.Fatal(err)
	}
	want, err := db.similarities(req.Model, req.Query, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(sim) != len(want) {
		t.Fatalf("similarities %v, want %v", sim, want)
	}
	for id, v := range want {
		if sim[id] != v {
			t.Fatalf("%s similarity %v, want %v", id, sim[id], v)
		}
	}
	k := contrastKnown(6)
	scores, err := db.sceneContrast(k, req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.contrastAgainst(req, sum, count, ids)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 || len(got) != len(scores) || len(scores) != 4 {
		t.Fatalf("count %d, scores %v, want %v", count, got, scores)
	}
	for id, v := range scores {
		if d := got[id] - v; d > 1e-12 || d < -1e-12 {
			t.Fatalf("%s contrast %v, want %v", id, got[id], v)
		}
	}
}

// Each trace row says why its record is in the lane it is counted in (#5
// selection reason, 1.2): 리나's fact is taken first for the input's name,
// her events are about the person the input calls, the hidden fact is the
// secret lane's, and a record only Select found says so.
func TestDeliverTraceWhy(t *testing.T) {
	d, ids := deliverGeneral(t, 2000)
	why := map[string]string{}
	for _, r := range d.Trace {
		why[r.ID] = r.Why
	}
	want := map[string]string{ids["fact"]: WhyInputFact, ids["event1"]: WhyCalled, ids["event2"]: WhyCalled, ids["secret"]: WhySecret}
	for id, w := range want {
		if why[id] != w {
			t.Fatalf("%s why %q, want %q (all %v)", id, why[id], w, why)
		}
	}
	db := testDB(t)
	input := "그래서 소금 흥정은 어떻게 됐지"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"memories":[{"id":"m1","kind":"event","body":"소금 흥정은 값을 반으로 깎고 끝났다"}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 2)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Trace) != 1 || d.Trace[0].ID != it.Memories[0].ID || d.Trace[0].Why != WhySelected {
		t.Fatalf("recalled event's trace: %+v", d.Trace)
	}
}

// The stored alias the scene uses is delivered (shown first), as a short
// line in the room the records leave in the general lane (1.2 #6: a stored alias was never
// delivered). An alias kept from someone is a secret line
// with that awareness, counted in the secret lane and never in the general
// text; a name the scene does not use brings no alias line.
func TestDeliverAliasLines(t *testing.T) {
	deliver := func(input, entities string, chars int) Delivery {
		db := testDB(t)
		s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"오늘", "마지막 답"}}, input)
		a24Save(t, db, turnSource(s, 0), `{"entities":[`+entities+`],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"일","mode":"상태","body":"에드윈은 성의 주인이다"},{"id":"m2","kind":"fact","about":"n4","slot":"일","mode":"상태","body":"리나는 약방을 한다"}]}`)
		d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 1)), Chars: chars, Secret: 4000})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	open := `{"id":"n1","name":"에드윈","type":"person"},{"id":"n4","name":"리나","type":"person"},{"id":"n2","name":"가면 기사","type":"person","same_as":"n1","how":"alias"}`
	d := deliver("가면 기사는 길드로 갔다", open, 2000)
	want := "- (T1) name/별칭 가면 기사 = 에드윈"
	if len(d.Names) != 1 || d.Names[0] != want || !strings.Contains(d.PDFBlock, want) {
		t.Fatalf("alias line: names %q, block %q", d.Names, d.PDFBlock)
	}
	lines := 0
	for _, l := range d.Lines {
		lines += utf8.RuneCountInString(l) + 1
	}
	if d.Chars != lines+utf8.RuneCountInString(want)+1 || len(d.IDs) != len(d.Lines) {
		t.Fatalf("chars %d, lines %d + alias %d", d.Chars, lines, utf8.RuneCountInString(want)+1)
	}
	// the alias takes only the room the records leave: at a ceiling one
	// char short of both, the record stays and the alias goes
	if d = deliver("가면 기사는 길드로 갔다", open, d.Chars-1); d.Chars > lines || len(d.Names) != 0 || len(d.IDs) == 0 {
		t.Fatalf("alias pushed a record: chars %d (records %d), names %q, ids %v", d.Chars, lines, d.Names, d.IDs)
	}
	if d = deliver("리나가 왔다", open, 2000); len(d.Names) != 0 || len(d.IDs) == 0 {
		t.Fatalf("alias of an uncalled person delivered: %q (ids %v)", d.Names, d.IDs)
	}
	kept := `{"id":"n1","name":"에드윈","type":"person"},{"id":"n4","name":"리나","type":"person"},{"id":"n3","name":"바렌","type":"person"},{"id":"n2","name":"가면 기사","type":"person","same_as":"n1","how":"alias","unaware":["n3"]}`
	d = deliver("가면 기사는 길드로 갔다", kept, 2000)
	if len(d.Names) != 0 || len(d.SecretNames) != 1 || !strings.Contains(d.SecretNames[0], "unaware: 바렌") ||
		strings.Contains(d.PDFBlock, "가면 기사 = 에드윈") || !strings.Contains(d.RetainedBlock, d.SecretNames[0]) || d.SecretChars < utf8.RuneCountInString(d.SecretNames[0])+1 {
		t.Fatalf("kept alias: names %q, secret %q, pdf %q, retained %q, secret chars %d", d.Names, d.SecretNames, d.PDFBlock, d.RetainedBlock, d.SecretChars)
	}
}
