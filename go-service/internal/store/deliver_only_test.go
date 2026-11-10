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

// Review 17 A-01: an event's disclosure limit stays with its completing fact
// when the fact is delivered without the event (the event not chosen): the
// fact's line shows it and goes by the secret ceiling. With both delivered
// (no chosen set) the delivery is as before.
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
