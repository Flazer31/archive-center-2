package store

import (
	"strconv"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// deliverChat is a chat of four answered turns and an input not answered
// yet. Turn 1 holds a long fact about 리나 and a secret kept from 도윤,
// turn 2 a short fact about her, turn 4 another. Each record has a stored
// 3-dimension vector.
func deliverChat(t *testing.T) (*DB, *lifecycle.Session, map[string]string) {
	t.Helper()
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	var rows []lifecycle.Row
	for i, txt := range []string{"리나를 만났다", "길을 걸었다", "비가 왔다", "리나가 웃었다"} {
		n := string(rune('1' + i))
		rows = append(rows, lrow("u"+n, "user", "", txt), lrow("a"+n, "char", "g"+n, txt+" (답)"))
		s.Apply(lsnap(lifecycle.SourceOutputEvent, append([]lifecycle.Row{}, rows...)...))
	}
	s.Apply(lsnap(lifecycle.SourceOutputEvent, append(append([]lifecycle.Row{}, rows...), lrow("u5", "user", "", "리나에게 묻는다"))...))
	_, it1, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나","type":"person"},{"id":"n2","name":"도윤","type":"person"}],
	 "memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","mode":"상태","body":"리나는 마을 약국에서 일하는 약사이고, 아침마다 약초를 말리며 손님의 증상을 오래 듣고 처방을 고른다"},
	  {"id":"m3","kind":"fact","about":"n1","slot":"비밀","mode":"상태","body":"리나는 왕녀다","hide_from":["n2"]}]}`)
	lina := it1.Names[0].ID
	_, it2, _ := a24Save(t, db, turnSource(s, 1), `{"memories":[{"id":"m1","kind":"fact","about":"`+lina+`","slot":"취미","mode":"성향","body":"리나는 낚시를 좋아한다"}]}`)
	_, it4, _ := a24Save(t, db, turnSource(s, 3), `{"memories":[{"id":"m1","kind":"fact","about":"`+lina+`","slot":"기분","mode":"상태","body":"리나는 기분이 좋다"}]}`)
	ids := map[string]string{"long": it1.Memories[0].ID, "short": it2.Memories[0].ID, "secret": it1.Memories[1].ID, "recent": it4.Memories[0].ID}
	vec := map[string][]float32{"long": {1, 0, 0}, "short": {0, 1, 0}, "secret": {0, 0, 1}, "recent": {1, 0, 0}}
	hashes := map[string]string{}
	var hs []string
	var vs [][]float32
	for name, id := range ids {
		h := TextHash("doc " + name)
		hashes[id] = h
		hs = append(hs, h)
		vs = append(vs, vec[name])
	}
	if err := db.SetDocHashes(hashes); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbeddings("m", hs, vs); err != nil {
		t.Fatal(err)
	}
	return db, s, ids
}

func printsOf(src memory.Source) []TextPrint {
	var out []TextPrint
	for _, m := range src.Members {
		out = append(out, textPrint(m.Data))
	}
	for _, r := range src.Responses {
		out = append(out, textPrint(r.Data))
	}
	return out
}

func has(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// The request's latest turns are found by their rows in the payload and
// their memory is not delivered; with none found the default window holds
// every turn of this short chat, so nothing is delivered.
func TestDeliverWindowFromPayload(t *testing.T) {
	db, s, ids := deliverChat(t)
	scope := memory.ScopeOf(s)
	// Isolate the restored window rule from semantic pruning.
	req := DeliveryRequest{Input: "리나에게 묻는다", Payload: printsOf(turnSource(s, 3)), Model: "m", Chars: 2000, Secret: 4000}
	d, err := db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 1 || d.WindowFrom != "payload" {
		t.Fatalf("window %d from %s, want 1 from payload", d.Window, d.WindowFrom)
	}
	if has(d.IDs, ids["recent"]) {
		t.Fatal("a record of a turn the request holds was delivered")
	}
	for _, name := range []string{"long", "short", "secret"} {
		if !has(d.IDs, ids[name]) {
			t.Fatalf("%s record of an earlier turn not delivered: %v", name, d.IDs)
		}
	}
	if d.Ranked != "recency" || !strings.Contains(d.Block, "리나는 낚시를 좋아한다") {
		t.Fatalf("ranked %s, block %q", d.Ranked, d.Block)
	}
	req.Payload = nil
	d, err = db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	if d.WindowFrom != "default" || d.Window != 4 || len(d.IDs) != 0 || d.Block != "" {
		t.Fatalf("no rows in the payload: window %d from %s, delivered %v", d.Window, d.WindowFrom, d.IDs)
	}
}

// Records are taken in the order of similarity rank plus recency rank
// while they fit the ceiling; one that does not fit is skipped and the next
// is tried. The query decides whether the older long record or the newer
// short one comes first; without a query the newer one does.
func TestDeliverOrderAndCeiling(t *testing.T) {
	db, s, ids := deliverChat(t)
	scope := memory.ScopeOf(s)
	k, err := db.CurrentKnown(s)
	if err != nil {
		t.Fatal(err)
	}
	sup := memory.Superseded(k)
	names := map[string]string{}
	for _, n := range k.Names {
		names[n.ID] = n.Name
	}
	size := map[string]int{}
	for _, m := range k.Memories {
		size[m.ID] = recordSize(memory.RenderKnown(memory.Selection{Memories: []memory.KnownMemory{m}}, sup), deliveryLine(m, names, "", nil))
	}
	if size[ids["long"]] <= size[ids["short"]] {
		t.Fatalf("fixture: long %d chars, short %d", size[ids["long"]], size[ids["short"]])
	}
	payload := printsOf(turnSource(s, 3))
	deliver := func(q []float32, chars int) Delivery {
		d, err := db.Deliver(scope, DeliveryRequest{Input: "리나에게 묻는다", Payload: payload, Query: q, Model: "m", Chars: chars, Secret: 0})
		if err != nil {
			t.Fatal(err)
		}
		if d.Chars > chars {
			t.Fatalf("delivered %d chars over the ceiling %d", d.Chars, chars)
		}
		return d
	}
	if d := deliver([]float32{1, 0, 0}, 2000); len(d.IDs) != 1 || d.IDs[0] != ids["long"] {
		t.Fatalf("query close to the long record: %v", d.IDs)
	}
	if d := deliver([]float32{0, 1, 0}, 2000); len(d.IDs) != 1 || d.IDs[0] != ids["short"] {
		t.Fatalf("query close to the short record: %v", d.IDs)
	}
	// The short row fits baseline selection, but points away from this
	// centered query. Removing it must not refill the freed space.
	if d := deliver([]float32{1, 0, 0}, size[ids["short"]]); len(d.IDs) != 0 || d.Block != "" {
		t.Fatalf("ceiling below the first record: %v", d.IDs)
	}
	if d := deliver([]float32{1, 0, 0}, size[ids["long"]]); len(d.IDs) != 1 || d.IDs[0] != ids["long"] {
		t.Fatalf("ceiling of the first record: %v", d.IDs)
	}
	if d := deliver(nil, 2000); d.Ranked != "recency" || len(d.IDs) != 2 || d.IDs[0] != ids["short"] {
		t.Fatalf("no query vector: ranked %s, delivered %v", d.Ranked, d.IDs)
	}
	if d := deliver([]float32{1, 0, 0}, 0); len(d.IDs) != 0 || d.Block != "" {
		t.Fatalf("ceiling 0 delivered %v", d.IDs)
	}
}

// A record someone must not know goes under the secret ceiling, separate
// from the memory ceiling, with its awareness line.
func TestDeliverSecretCeiling(t *testing.T) {
	db, s, ids := deliverChat(t)
	scope := memory.ScopeOf(s)
	payload := printsOf(turnSource(s, 3))
	d, err := db.Deliver(scope, DeliveryRequest{Input: "리나에게 묻는다", Payload: payload, Query: []float32{0, 0, 1}, Model: "m", Chars: 0, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.IDs) != 1 || d.IDs[0] != ids["secret"] || d.Chars != 0 || d.SecretChars == 0 {
		t.Fatalf("memory ceiling 0, secret ceiling 4000: %+v", d)
	}
	if !strings.Contains(d.Block, "hide from: 도윤") || strings.Contains(d.Block, "about N") {
		t.Fatalf("secret delivered without who must not know: %q", d.Block)
	}
	// Without a query the original public rows still use only their lane.
	d, err = db.Deliver(scope, DeliveryRequest{Input: "리나에게 묻는다", Payload: payload, Model: "m", Chars: 2000, Secret: 0})
	if err != nil {
		t.Fatal(err)
	}
	if has(d.IDs, ids["secret"]) || len(d.IDs) != 2 {
		t.Fatalf("secret ceiling 0: %v", d.IDs)
	}
}

// The memory ceilings default to 2,000 and 4,000 characters and keep what
// is set.
func TestMemoryBudgetsSetting(t *testing.T) {
	db := testDB(t)
	if c, s, err := db.MemoryBudgets(); err != nil || c != DefaultMemoryChars || s != DefaultSecretChars {
		t.Fatalf("defaults %d %d %v", c, s, err)
	}
	if err := db.SetMemoryBudgets(1500, 3000); err != nil {
		t.Fatal(err)
	}
	if c, s, err := db.MemoryBudgets(); err != nil || c != 1500 || s != 3000 {
		t.Fatalf("set %d %d %v", c, s, err)
	}
	if err := db.SetMemoryBudgets(-1, 3000); err == nil {
		t.Fatal("negative ceiling accepted")
	}
}

// chatOf applies turns of (input, reply) rows and an input not answered yet.
func chatOf(t *testing.T, pairs [][2]string, input string) *lifecycle.Session {
	t.Helper()
	s := lifecycle.NewSession("chat:A")
	var rows []lifecycle.Row
	for i, p := range pairs {
		n := strconv.Itoa(i + 1)
		rows = append(rows, lrow("u"+n, "user", "", p[0]), lrow("a"+n, "char", "g"+n, p[1]))
		s.Apply(lsnap(lifecycle.SourceOutputEvent, append([]lifecycle.Row{}, rows...)...))
	}
	s.Apply(lsnap(lifecycle.SourceOutputEvent, append(append([]lifecycle.Row{}, rows...), lrow("cur", "user", "", input))...))
	return s
}

// The same short input in every turn stands for one turn of the request:
// a request holding only the last pair holds one turn, and the first
// turn's record is delivered.
func TestDeliverRepeatedInputWindow(t *testing.T) {
	db := testDB(t)
	var pairs [][2]string
	for i := 1; i <= 6; i++ {
		pairs = append(pairs, [2]string{"네", "답" + strconv.Itoa(i)})
	}
	s := chatOf(t, pairs, "리나는?")
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"리나는 약사다"}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "리나는?", Payload: []TextPrint{textPrint("네"), textPrint("답6"), textPrint("리나는?")}, Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 1 || d.WindowFrom != "payload" || !has(d.IDs, it.Memories[0].ID) {
		t.Fatalf("window %d from %s, delivered %v", d.Window, d.WindowFrom, d.IDs)
	}
}

// A branch reads its parent's copied turns with the parent's turn numbers;
// the window and recency follow the read scope's order, not the numbers.
func TestDeliverBranchPlaces(t *testing.T) {
	db := testDB(t)
	parent := memory.Source{Session: "chat:P", Turn: 100, Members: []memory.SourceMember{{Key: "pu", Data: "리나를 만났다"}}, Responses: []memory.SourceResponse{{Key: "pa", Data: "만남"}}}
	_, it, _ := a24Save(t, db, parent, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"리나는 약사다"}]}`)
	child := memory.Source{Session: "chat:A", Turn: 2, Members: []memory.SourceMember{{Key: "cu", Data: "걸었다"}}, Responses: []memory.SourceResponse{{Key: "ca", Data: "길"}}}
	scope := []memory.ScopedTurn{{Source: parent, Inherited: true, Confirmed: true}, {Source: child, Confirmed: true}, {Source: memory.Source{Session: "chat:A", Turn: 3, Members: []memory.SourceMember{{Key: "cur", Data: "리나는?"}}}}}
	d, err := db.Deliver(scope, DeliveryRequest{Input: "리나는?", Payload: []TextPrint{textPrint("걸었다"), textPrint("길"), textPrint("리나는?")}, Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 1 || !has(d.IDs, it.Memories[0].ID) {
		t.Fatalf("window %d, delivered %v", d.Window, d.IDs)
	}
}

// A view keeps how the person holds it: basis, confidence and whether it
// differs from the facts.
func TestDeliverViewKeepsBasis(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 소문을 들었다", "들음"}, {"걸었다", "길"}}, "서진이 배신자라고?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m0","kind":"event","body":"도윤이 서진의 배신을 말했다"},{"id":"m1","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진이 배신자라고 믿는다","basis":{"via":"소문","from":"n3","ref":"m0","confidence":"낮음"},"affect":{"intensity":"강함","persistence":"남음"},"expressed":"no","differs":"yes"}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진이 배신자라고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	view := lineWith(d.Block, "배신자라고 믿는다")
	for _, want := range []string{"소문", "confidence 낮음", "differs from the facts: yes", "from 도윤", "feeling 강함 남음", "expressed no", "based on B1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view line lacks %q: %q", want, d.Block)
		}
	}
	if strings.Count(d.Block, `[text B1="도윤이 서진의 배신을 말했다"]`) != 1 || strings.Contains(view, `based on B1 "`) || strings.Count(d.Block, "도윤이 서진의 배신을 말했다") != 1 {
		t.Fatalf("exact basis definition not on the preceding source: %q", d.Block)
	}
}

// The delivered lines never pass the ceiling, also when names are longer
// than the ids they replace.
func TestDeliverLinesWithinCeiling(t *testing.T) {
	db := testDB(t)
	long := strings.Repeat("가", 210)
	s := chatOf(t, [][2]string{{long + " 왔다", "옴"}, {"걸었다", "길"}}, long+"는?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"`+long+`"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"약사다"}]}`)
	scope := memory.ScopeOf(s)
	for _, ceiling := range []int{100, 2000} {
		d, err := db.Deliver(scope, DeliveryRequest{Input: long + "는?", Payload: printsOf(turnSource(s, 1)), Chars: ceiling, Secret: 4000})
		if err != nil {
			t.Fatal(err)
		}
		if got := len([]rune(d.Block[strings.Index(d.Block, "\n")+1:])); d.Block != "" && got > ceiling {
			t.Fatalf("ceiling %d: delivered %d chars", ceiling, got)
		}
		if ceiling == 2000 && len(d.IDs) != 1 {
			t.Fatalf("ceiling 2000 delivered %v", d.IDs)
		}
	}
}

// Two whole identical pairs in the request are two turns: the record of the
// earlier of them is not delivered again.
func TestDeliverRepeatedWholePairs(t *testing.T) {
	db := testDB(t)
	var pairs [][2]string
	for i := 1; i <= 6; i++ {
		pairs = append(pairs, [2]string{"네", "알겠어"})
	}
	s := chatOf(t, pairs, "리나는?")
	_, it, _ := a24Save(t, db, turnSource(s, 4), `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"리나는 약사다"}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "리나는?", Payload: []TextPrint{textPrint("네"), textPrint("알겠어"), textPrint("네"), textPrint("알겠어"), textPrint("리나는?")}, Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 2 || has(d.IDs, it.Memories[0].ID) {
		t.Fatalf("window %d, delivered %v", d.Window, d.IDs)
	}
}

// In a branch, an obligation the branch's own later turn fulfilled is not
// delivered as still open, although the parent's copied turn that kept it
// active has the higher turn number.
func TestDeliverBranchStateOrder(t *testing.T) {
	db := testDB(t)
	parent := memory.Source{Session: "chat:P", Turn: 100, Members: []memory.SourceMember{{Key: "pu", Data: "리나가 빚을 졌다"}}, Responses: []memory.SourceResponse{{Key: "pa", Data: "빚"}}}
	_, it, _ := a24Save(t, db, parent, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"채무","mode":"의무","body":"리나는 채무를 청산한다","status":"open"}]}`)
	duty := it.Memories[0].ID
	// the parent's next turn keeps the obligation active
	parent2 := memory.Source{Session: "chat:P", Turn: 101, Members: []memory.SourceMember{{Key: "pu2", Data: "리나가 다짐했다"}}, Responses: []memory.SourceResponse{{Key: "pa2", Data: "다짐"}}}
	a24Save(t, db, parent2, `{"memories":[{"id":"m1","kind":"event","body":"리나가 갚겠다고 다짐했다","effects":[{"about":"`+duty+`","status":"active"}]}]}`)
	child := memory.Source{Session: "chat:A", Turn: 2, Members: []memory.SourceMember{{Key: "cu", Data: "리나가 빚을 갚았다"}}, Responses: []memory.SourceResponse{{Key: "ca", Data: "갚음"}}}
	a24Save(t, db, child, `{"memories":[{"id":"m1","kind":"event","body":"리나가 빚을 갚았다","effects":[{"about":"`+duty+`","status":"fulfilled"}]}]}`)
	last := memory.Source{Session: "chat:A", Turn: 3, Members: []memory.SourceMember{{Key: "lu", Data: "걸었다"}}, Responses: []memory.SourceResponse{{Key: "la", Data: "길"}}}
	scope := []memory.ScopedTurn{{Source: parent, Inherited: true, Confirmed: true}, {Source: parent2, Inherited: true, Confirmed: true}, {Source: child, Confirmed: true}, {Source: last, Confirmed: true}, {Source: memory.Source{Session: "chat:A", Turn: 4, Members: []memory.SourceMember{{Key: "cur", Data: "리나가 웃었다"}}}}}
	d, err := db.Deliver(scope, DeliveryRequest{Input: "리나가 웃었다", Payload: []TextPrint{textPrint("걸었다"), textPrint("길"), textPrint("리나가 웃었다")}, Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 1 {
		t.Fatalf("window %d", d.Window)
	}
	if has(d.IDs, duty) {
		t.Fatalf("fulfilled obligation delivered as open: %q", d.Block)
	}
}

// When the host changed the texts so no row is found, the request's own
// user messages before the latest one tell how many earlier turns it holds.
func TestDeliverWindowCountedFromRoles(t *testing.T) {
	db, s, ids := deliverChat(t)
	other := func(role, txt string) TextPrint { p := textPrint(txt); p.Role = role; return p }
	payload := []TextPrint{other("system", "card"), other("user", "변형된 3"), other("assistant", "변형된 답3"), other("user", "변형된 4"), other("assistant", "변형된 답4"), other("user", "리나에게 묻는다")}
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "리나에게 묻는다", Payload: payload, Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 2 || d.WindowFrom != "count" || has(d.IDs, ids["recent"]) || !has(d.IDs, ids["long"]) {
		t.Fatalf("window %d from %s, delivered %v", d.Window, d.WindowFrom, d.IDs)
	}
}

// A view whose basis is a secret record carries that record's text under
// the secret ceiling, with who must not be told.
func TestDeliverBasisSecret(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 소문을 들었다", "들음"}, {"걸었다", "길"}}, "서진을 믿지 않는다고?")
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m1","kind":"event","body":"도윤은 왕의 숨겨진 후계자다","hide_from":["n3"]},{"id":"m2","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진을 믿지 않는다","basis":{"via":"소문","ref":"m1"}}]}`)
	view := it.Memories[1].ID
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진을 믿지 않는다고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 0})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Block, "숨겨진 후계자") || d.SecretChars != 0 {
		t.Fatalf("secret text in the memory lane: %q", d.Block)
	}
	if d.Candidates == 0 {
		t.Fatal("fixture: the view is not a candidate")
	}
	d, err = db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진을 믿지 않는다고?", Payload: printsOf(turnSource(s, 1)), Chars: 0, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if !has(d.IDs, view) || !strings.Contains(d.Block, "숨겨진 후계자") || !strings.Contains(d.Block, "hide from: 도윤") {
		t.Fatalf("secret basis under the secret ceiling: %v %q", d.IDs, d.Block)
	}
}

// A view based on a value an event set shows that value.
func TestDeliverBasisEffect(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 돈을 받았다", "받음"}, {"걸었다", "길"}}, "서진이 리나 돈을 노린다고?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"}],"memories":[{"id":"m0","kind":"event","body":"리나가 돈을 받았다","effects":[{"about":"n1","slot":"돈","before":"0냥","value":"10냥"}]},{"id":"m1","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진이 리나 돈을 노린다고 믿는다","basis":{"via":"추론","ref":"m0.1"}}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진이 리나 돈을 노린다고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Block, "based on B1 \"리나가 돈을 받았다 [돈: 0냥 -> 10냥]\"") {
		t.Fatalf("effect basis lost: %q", d.Block)
	}
}

// The current input repeating the last turn's user text is not taken for
// that earlier turn being in the request.
func TestDeliverCurrentInputNotEarlierTurn(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나는?", "알겠어"}, {"리나는?", "알겠어"}}, "리나는?")
	_, it, _ := a24Save(t, db, turnSource(s, 1), `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"리나는 약사다"}]}`)
	cur := textPrint("리나는?")
	cur.Role = "user"
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "리나는?", Payload: []TextPrint{cur}, Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Window != 0 || !has(d.IDs, it.Memories[0].ID) {
		t.Fatalf("window %d from %s, delivered %v", d.Window, d.WindowFrom, d.IDs)
	}
}

// Two views on the same basis keep both views; the basis text is shown
// once and the second view points to its mark.
func TestDeliverBasisShownOnce(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 소문을 들었다", "들음"}, {"걸었다", "길"}}, "서진이 배신자라고?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m0","kind":"event","body":"도윤이 서진의 배신을 말했다"},{"id":"m1","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진이 배신자라고 믿는다","basis":{"via":"소문","ref":"m0"}},{"id":"m2","kind":"view","owner":"n3","about":"n2","aspect":"emotion","body":"서진이 배신자라 분노한다","basis":{"via":"추론","ref":"m0"}}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진이 배신자라고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(d.Block, `[text B1="도윤이 서진의 배신을 말했다"]`) != 1 || strings.Count(d.Block, "based on B1") != 2 || strings.Count(d.Block, "도윤이 서진의 배신을 말했다") != 1 || strings.Contains(d.Block, `based on B1 "`) {
		t.Fatalf("basis text not shown once: %q", d.Block)
	}
	for _, want := range []string{"서진이 배신자라고 믿는다", "서진이 배신자라 분노한다"} {
		if !strings.Contains(d.Block, want) {
			t.Fatalf("view %q dropped: %q", want, d.Block)
		}
	}
}

// lineWith is the delivered line holding text.
func lineWith(block, text string) string {
	for _, l := range strings.Split(block, "\n") {
		if strings.Contains(l, text) {
			return l
		}
	}
	return ""
}

// Protection set on an event's value (M1.1) goes with every line that
// shows that value: the event's own line and a view based on it.
func TestDeliverEffectSecret(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 돈을 받았다", "받음"}, {"걸었다", "길"}}, "서진이 리나 돈을 노린다고?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"}],"memories":[{"id":"m0","kind":"event","body":"리나가 돈을 받았다","effects":[{"about":"n1","slot":"비밀 금액","before":"0냥","value":"9876냥"}]},{"ref":"m0.1","hide_from":["n2"]},{"id":"m1","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진이 리나 돈을 노린다고 믿는다","basis":{"via":"추론","ref":"m0.1"}}]}`)
	req := DeliveryRequest{Input: "서진이 리나 돈을 노린다고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 0}
	d, err := db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Candidates == 0 {
		t.Fatal("fixture: no candidates")
	}
	if strings.Contains(d.Block, "9876") {
		t.Fatalf("a protected value in the memory lane: %q", d.Block)
	}
	req.Chars, req.Secret = 0, 4000
	if d, err = db.Deliver(memory.ScopeOf(s), req); err != nil {
		t.Fatal(err)
	}
	view := lineWith(d.Block, "노린다고 믿는다")
	if view == "" || !strings.Contains(view, "9876") || !strings.Contains(view, "hide from: 서진") {
		t.Fatalf("view line under the secret ceiling: %q", d.Block)
	}
}

// The view line itself carries who must not be told about its basis.
func TestDeliverViewLineAwareness(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 소문을 들었다", "들음"}, {"걸었다", "길"}}, "서진을 믿지 않는다고?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m1","kind":"event","body":"도윤은 왕의 숨겨진 후계자다","hide_from":["n3"]},{"id":"m2","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진을 믿지 않는다","basis":{"via":"소문","ref":"m1"}}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진을 믿지 않는다고?", Payload: printsOf(turnSource(s, 1)), Chars: 0, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if view := lineWith(d.Block, "서진을 믿지 않는다"); !strings.Contains(view, "hide from: 도윤") {
		t.Fatalf("view line without its basis's protection: %q", d.Block)
	}
}

// A basis mark is set only by a line that is delivered: when the first
// view on a basis does not fit, the next delivered view defines the mark.
// Final display may move that definition onto an earlier selected source.
func TestDeliverMarkAfterSkip(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 소문을 들었다", "들음"}, {"걸었다", "길"}}, "서진이 배신자라고?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m0","kind":"event","body":"도윤이 서진의 배신을 말했다"},{"id":"m1","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진이 배신자라고 믿는다","basis":{"via":"소문","ref":"m0"},"hide_from":["n2"]},{"id":"m2","kind":"view","owner":"n3","about":"n2","aspect":"emotion","body":"서진이 배신자라 분노한다","basis":{"via":"추론","ref":"m0"}}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진이 배신자라고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 0})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Block, "배신자라고 믿는다") {
		t.Fatalf("fixture: the secret view was delivered: %q", d.Block)
	}
	if view := lineWith(d.Block, "배신자라 분노한다"); !strings.Contains(view, "based on B1") || strings.Contains(view, `based on B1 "`) || strings.Count(d.Block, `[text B1="도윤이 서진의 배신을 말했다"]`) != 1 || strings.Count(d.Block, "based on B1") != 1 || strings.Count(d.Block, "도윤이 서진의 배신을 말했다") != 1 {
		t.Fatalf("basis text missing after the first view was skipped: %q", d.Block)
	}
}

// The memory text opens with its title and the fingerprint of the lines
// after it.
func TestDeliverBlockFingerprint(t *testing.T) {
	db, s, _ := deliverChat(t)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "리나에게 묻는다", Payload: printsOf(turnSource(s, 3)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	nl := strings.Index(d.Block, "\n")
	first, body := d.Block[:nl], d.Block[nl+1:]
	if first != BlockTitle+textPrint(body).H+"]" {
		t.Fatalf("title line %q for body %q", first, body)
	}
}
