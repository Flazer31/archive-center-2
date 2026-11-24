package store

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Preprocessing modes (E01~E03, 11-07 plan step 2) hand Deliver the records
// they chose. With every delivered record chosen the delivery is the same
// text; a record not chosen is left out and shown in the trace; nothing
// chosen delivers no record; a pinned record is taken regardless; a chosen
// secret still goes by the secret ceiling; with no ceiling every candidate is
// listed (the wide list), and choosing all of it gives that list back.
func TestDeliverOnlyChosen(t *testing.T) {
	db, s, ids := deliverChat(t)
	scope := memory.ScopeOf(s)
	payload := printsOf(turnSource(s, 3))
	deliver := func(q []float32, chars, secret int, only map[string]bool) Delivery {
		t.Helper()
		d, err := db.Deliver(scope, DeliveryRequest{Input: "리나에게 묻는다", Payload: payload, Query: q, Model: "m", Chars: chars, Secret: secret, Only: only})
		if err != nil {
			t.Fatal(err)
		}
		if d.Chars > chars || d.SecretChars > secret {
			t.Fatalf("over a ceiling: %d/%d %d/%d", d.Chars, chars, d.SecretChars, secret)
		}
		return d
	}
	set := func(xs ...string) map[string]bool {
		m := map[string]bool{}
		for _, x := range xs {
			m[x] = true
		}
		return m
	}
	result := func(d Delivery, id string) string {
		for _, r := range d.Trace {
			if r.ID == id {
				return r.Result
			}
		}
		return ""
	}
	var q []float32 // recency only: in this short chat scene contrast then removes nothing
	basic := deliver(q, 2000, 4000, nil)
	if len(basic.IDs) != 3 || basic.IDs[0] != ids["short"] || !has(basic.IDs, ids["long"]) || !has(basic.IDs, ids["secret"]) {
		t.Fatalf("fixture: %v (want short, long and the secret)", basic.IDs)
	}

	// every delivered record chosen: the same delivery, byte for byte
	same := deliver(q, 2000, 4000, set(basic.IDs...))
	if same.Block != basic.Block || len(same.IDs) != len(basic.IDs) || same.Chars != basic.Chars || same.SecretChars != basic.SecretChars {
		t.Fatalf("all chosen differs:\n%s\n---\n%s", same.Block, basic.Block)
	}

	// one left out: not delivered, its trace row says so, the rest stays
	left := deliver(q, 2000, 4000, set(ids["secret"]))
	if has(left.IDs, ids["short"]) || !has(left.IDs, ids["secret"]) || result(left, ids["short"]) != "not_chosen" {
		t.Fatalf("short not chosen: %v %q", left.IDs, result(left, ids["short"]))
	}
	if left.Block == basic.Block {
		t.Fatal("leaving a record out did not change the block")
	}

	// nothing chosen: no record (an empty choice is not "no choice")
	none := deliver(q, 2000, 4000, map[string]bool{})
	if len(none.IDs) != 0 || none.Block != "" {
		t.Fatalf("nothing chosen delivered %v", none.IDs)
	}

	// a chosen secret still goes by the secret ceiling
	if d := deliver(q, 2000, 0, set(ids["secret"])); has(d.IDs, ids["secret"]) || result(d, ids["secret"]) != "over_ceiling" {
		t.Fatalf("secret ceiling 0: %v %q", d.IDs, result(d, ids["secret"]))
	}

	// a pinned record is taken although not chosen
	if _, err := db.SetMemoryHold("chat:A", ids["long"], HoldPin); err != nil {
		t.Fatal(err)
	}
	if d := deliver(q, 2000, 4000, map[string]bool{}); len(d.IDs) != 1 || d.IDs[0] != ids["long"] {
		t.Fatalf("pinned and not chosen: %v", d.IDs)
	}
	if _, err := db.SetMemoryHold("chat:A", ids["long"], ""); err != nil {
		t.Fatal(err)
	}

	// a ceiling with room for the first record only: the basic delivery
	// stops there, the wide list (no ceiling) has the next one too, and
	// choosing that next one instead delivers it within the same ceiling
	size := map[string]int{}
	for _, r := range basic.Trace {
		size[r.ID] = r.Size
	}
	room := size[ids["short"]]
	if size[ids["long"]] > room {
		room = size[ids["long"]]
	}
	small := deliver(q, room, 0, nil)
	if len(small.IDs) != 1 || small.IDs[0] != ids["short"] || result(small, ids["long"]) != "over_ceiling" {
		t.Fatalf("fixture: room %d gives %v", room, small.IDs)
	}
	wide := deliver(q, 10000000, 10000000, nil)
	if len(wide.IDs) != 3 {
		t.Fatalf("wide list %v", wide.IDs)
	}
	if d := deliver(q, room, 0, set(ids["long"])); len(d.IDs) != 1 || d.IDs[0] != ids["long"] || result(d, ids["short"]) != "not_chosen" {
		t.Fatalf("a wide-list record chosen in place of the first: %v", d.IDs)
	}
	if d := deliver(q, 10000000, 10000000, set(wide.IDs...)); d.Block != wide.Block {
		t.Fatal("choosing the whole wide list changed it")
	}
}

// A completing fact goes only with its delivered event. With a chosen set,
// an event left out because it was not chosen does not hold back its chosen
// fact (as the wide list showed it after scene contrast took the event);
// an event left out by its ceiling still does.
func TestDeliverOnlyCompletionFact(t *testing.T) {
	db := testDB(t)
	input := "리나가 진홍인장을 떠올렸다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"셋째", "세 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"세계 규칙"}],"memories":[
 {"id":"m1","kind":"event","body":"진홍인장이 빛났다"},
 {"id":"m2","kind":"fact","about":"n2","at":"m1","body":"봉인은 같은 등급의 열쇠로만 열린다"}]}`)
	event, rule := it.Memories[0].ID, it.Memories[1].ID
	deliver := func(chars int, only map[string]bool) Delivery {
		t.Helper()
		d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Chars: chars, Secret: 4000, Only: only})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	row := func(d Delivery, id string) string {
		for _, r := range d.Trace {
			if r.ID == id {
				return r.Result
			}
		}
		return ""
	}
	full := deliver(2000, nil)
	if !has(full.IDs, event) || !has(full.IDs, rule) {
		t.Fatalf("fixture: %v", full.IDs)
	}
	if d := deliver(2000, map[string]bool{rule: true}); has(d.IDs, event) || !has(d.IDs, rule) || row(d, event) != "not_chosen" {
		t.Fatalf("chosen fact of a not-chosen event: %v event %q", d.IDs, row(d, event))
	}
	if d := deliver(0, map[string]bool{event: true, rule: true}); has(d.IDs, rule) || row(d, rule) != "parent_not_taken" {
		t.Fatalf("fact of an event over its ceiling: %v %q", d.IDs, row(d, rule))
	}
	if d := deliver(2000, map[string]bool{}); len(d.IDs) != 0 {
		t.Fatalf("nothing chosen: %v", d.IDs)
	}
}

// An event's disclosure limit stays with its completing fact when the fact
// is delivered without the event (the event not chosen): the fact's line
// shows it and goes by the secret ceiling. With both delivered (no chosen
// set) the delivery is as before.
func TestDeliverOnlyLoneFactKeepsEventProtection(t *testing.T) {
	db := testDB(t)
	input := "리나가 진홍인장을 떠올렸다" // the fact is not a Select candidate: it completes the event
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"셋째", "세 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"세계 규칙"},{"id":"n3","name":"도윤"}],"memories":[
 {"id":"m1","kind":"event","body":"진홍인장의 비밀 해제법을 리나가 밝혔다","hide_from":["n3"]},
 {"id":"m2","kind":"fact","about":"n2","at":"m1","body":"봉인은 같은 등급의 열쇠로만 열린다"}]}`)
	event, rule := it.Memories[0].ID, it.Memories[1].ID
	deliver := func(secret int, only map[string]bool) Delivery {
		t.Helper()
		d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Chars: 2000, Secret: secret, Only: only})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	both := deliver(4000, nil)
	for _, r := range both.Trace {
		if r.ID == rule && r.Parent != event {
			t.Fatalf("fixture: the fact must complete the event, trace %+v", r)
		}
	}
	if !has(both.IDs, event) || !has(both.IDs, rule) || !both.SecretIDs[event] || both.SecretIDs[rule] {
		t.Fatalf("fixture: %v secret %v", both.IDs, both.SecretIDs)
	}
	lone := deliver(4000, map[string]bool{rule: true})
	if has(lone.IDs, event) || !has(lone.IDs, rule) {
		t.Fatalf("lone fact: %v", lone.IDs)
	}
	if !lone.SecretIDs[rule] || lone.SecretChars == 0 || !strings.Contains(lone.Block, "hide from: 도윤") || strings.Contains(lone.PDFBlock, "같은 등급의 열쇠") {
		t.Fatalf("the event's limit was lost: secret %v chars %d\n%s\nPDF:\n%s", lone.SecretIDs, lone.SecretChars, lone.Block, lone.PDFBlock)
	}
	if d := deliver(0, map[string]bool{rule: true}); has(d.IDs, rule) {
		t.Fatalf("secret ceiling 0 delivered the protected lone fact: %v", d.IDs)
	}
	if again := deliver(4000, nil); again.Block != both.Block {
		t.Fatal("no chosen set: delivery changed")
	}
}

// The same with the event's protection one step further (1.3 what rule,
// 11-17): the secret is a fact of its own, the event is recorded at it and
// shows no awareness itself, and the completing fact is recorded at the
// event. Delivered without the event, the fact still keeps the secret's
// protection and goes by the secret ceiling.
func TestDeliverOnlyLoneFactKeepsProtectionThroughItsEvent(t *testing.T) {
	db := testDB(t)
	input := "리나가 진홍인장을 떠올렸다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"셋째", "세 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"세계 규칙"},{"id":"n3","name":"도윤"}],"memories":[
 {"id":"m1","kind":"event","at":"m3","body":"진홍인장의 해제법을 리나가 밝혔다"},
 {"id":"m2","kind":"fact","about":"n2","at":"m1","body":"봉인은 같은 등급의 열쇠로만 열린다"},
 {"id":"m3","kind":"fact","mode":"정체","about":"n1","body":"리나는 진홍인장 해제법을 도윤에게 숨긴다","hide_from":["n3"]}]}`)
	event, rule := it.Memories[0].ID, it.Memories[1].ID
	deliver := func(secret int, only map[string]bool) Delivery {
		t.Helper()
		d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Chars: 2000, Secret: secret, Only: only})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	all := deliver(4000, nil)
	if !has(all.IDs, event) || !all.SecretIDs[event] {
		t.Fatalf("fixture: the event shows the secret's protection: %v secret %v\n%s", all.IDs, all.SecretIDs, all.Block)
	}
	// the completing fact is placed with the secret lane, as when the event
	// itself shows the awareness (not in the general lane after its event)
	for _, r := range all.Trace {
		if r.ID == rule && (r.Parent != event || r.Why != WhySecret) {
			t.Fatalf("completing fact placed as %+v", r)
		}
	}
	lone := deliver(4000, map[string]bool{rule: true})
	if has(lone.IDs, event) || !has(lone.IDs, rule) {
		t.Fatalf("lone fact: %v", lone.IDs)
	}
	if !lone.SecretIDs[rule] || lone.SecretChars == 0 || !strings.Contains(lone.Block, "hide from: 도윤") || strings.Contains(lone.PDFBlock, "같은 등급의 열쇠") {
		t.Fatalf("the secret's limit was lost: secret %v chars %d\n%s\nPDF:\n%s", lone.SecretIDs, lone.SecretChars, lone.Block, lone.PDFBlock)
	}
	if d := deliver(0, map[string]bool{rule: true}); has(d.IDs, rule) {
		t.Fatalf("secret ceiling 0 delivered the protected lone fact: %v", d.IDs)
	}
}

// A delivered event does not repeat its roles (11-17): seen by keeps only
// people with no role in it, and a setting naming the same place as its
// place role is left out; a setting that says something else stays, and the
// stored record keeps both.
func TestDeliverLeavesOutRepeatedRoles(t *testing.T) {
	db := testDB(t)
	input := "가람이 여관에서 다온을 다시 만났다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"셋째", "세 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"가람"},{"id":"n2","name":"다온"},{"id":"n3","name":"여관"},{"id":"n4","name":"보리"}],"memories":[
 {"id":"m1","kind":"event","body":"가람이 여관에서 다온에게 약초를 건넸다","roles":{"actor":["n1"],"target":["n2"],"place":["n3"]},"seen_by":["n1","n2","n4"],"setting":"여관 2층"},
 {"id":"m2","kind":"event","body":"가람이 여관에서 다온과 헤어졌다","roles":{"actor":["n1"],"place":["n3"]},"seen_by":["n1"],"setting":"비 오는 새벽"}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	line := func(id string) string {
		for i, x := range d.IDs {
			if x == id {
				return d.Lines[i]
			}
		}
		t.Fatalf("%s not delivered: %v", id, d.IDs)
		return ""
	}
	first, second := line(it.Memories[0].ID), line(it.Memories[1].ID)
	if !strings.Contains(first, "seen by 보리") || strings.Contains(first, "seen by 가람") || strings.Contains(first, "setting") {
		t.Fatalf("first: %s", first)
	}
	if strings.Contains(second, "seen by") || !strings.Contains(second, "setting 비 오는 새벽") {
		t.Fatalf("second: %s", second)
	}
	var detail string
	if err := db.sql.QueryRow(`SELECT detail FROM memories WHERE id=?`, strings.TrimPrefix(it.Memories[0].ID, "M")).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, `"setting":"여관 2층"`) || !strings.Contains(detail, `"seen_by":[`) {
		t.Fatalf("stored record changed: %s", detail)
	}
}

// An event that a secret fact is recorded at retells what the fact keeps:
// delivered without the fact, its line shows the fact's protection and is
// counted in the general lane (1.3, 11-17); an event with no secret under it
// shows none.
func TestDeliverEventShowsTheSecretRecordedAtIt(t *testing.T) {
	db := testDB(t)
	input := "리나가 진홍인장을 떠올렸다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"셋째", "세 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n3","name":"도윤"}],"memories":[
 {"id":"m1","kind":"event","body":"리나가 진홍인장의 해제법을 정했다"},
 {"id":"m2","kind":"fact","mode":"공동 목표","about":"n1","at":"m1","body":"진홍인장 해제법은 도윤에게 숨긴다","hide_from":["n3"]},
 {"id":"m3","kind":"event","body":"리나가 진홍인장을 닦았다"}]}`)
	event, fact, other := it.Memories[0].ID, it.Memories[1].ID, it.Memories[2].ID
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Chars: 2000, Secret: 4000, Only: map[string]bool{event: true, other: true}})
	if err != nil {
		t.Fatal(err)
	}
	if has(d.IDs, fact) || !has(d.IDs, event) || !has(d.IDs, other) {
		t.Fatalf("fixture: %v", d.IDs)
	}
	subject := " (what: " + it.Memories[1].Body + ")"
	if !d.SecretIDs[event] || d.SecretChars != len([]rune(subject)) || !strings.Contains(d.Block, "hide from: 도윤"+subject) {
		t.Fatalf("event without its secret fact: secret %v chars %d\n%s", d.SecretIDs, d.SecretChars, d.Block)
	}
	if d.SecretIDs[other] {
		t.Fatalf("an event with no secret under it was protected")
	}
	if strings.Contains(d.PDFBlock, "해제법을 정했다") {
		t.Fatalf("the protected event went to the general PDF part:\n%s", d.PDFBlock)
	}
}

// A view quoting an event that a secret fact is recorded at keeps that
// protection on its basis line (11-17, Sol review).
func TestDeliverBasisOfAnEventWithASecretBelow(t *testing.T) {
	db := testDB(t)
	input := "리나가 진홍인장을 떠올렸다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"셋째", "세 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n3","name":"도윤"}],"memories":[
 {"id":"m1","kind":"event","body":"리나가 진홍인장의 해제법을 정했다"},
 {"id":"m2","kind":"fact","mode":"공동 목표","about":"n1","at":"m1","body":"진홍인장 해제법은 도윤에게 숨긴다","hide_from":["n3"]},
 {"id":"m3","kind":"view","owner":"n1","aspect":"belief","body":"리나는 진홍인장을 지켜야 한다고 믿는다","basis":{"via":"경험","ref":"m1"}}]}`)
	event, fact, view := it.Memories[0].ID, it.Memories[1].ID, it.Memories[2].ID
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Chars: 2000, Secret: 4000, Only: map[string]bool{view: true}})
	if err != nil {
		t.Fatal(err)
	}
	if has(d.IDs, event) || has(d.IDs, fact) || !has(d.IDs, view) {
		t.Fatalf("fixture: %v", d.IDs)
	}
	if !d.SecretIDs[view] || !strings.Contains(d.Block, "hide from: 도윤") {
		t.Fatalf("view quoting the event: secret %v\n%s", d.SecretIDs, d.Block)
	}
}
