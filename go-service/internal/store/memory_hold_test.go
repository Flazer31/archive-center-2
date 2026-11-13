package store

import (
	"errors"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// C18 pin and suppress. In deliverChat a query close to the long record
// delivers it alone: scene contrast takes the short one out. A pinned record
// is taken first, past Select, ranking and scene contrast, within its lane's
// ceiling; one the request holds (turn 4) or one a later record replaced is
// not; a muted record is never delivered, nor shown as a basis; with no flag
// the block is as before.
func TestDeliverPinAndMute(t *testing.T) {
	db, s, ids := deliverChat(t)
	scope := memory.ScopeOf(s)
	payload := printsOf(turnSource(s, 3))
	deliver := func(q []float32, chars, secret int) Delivery {
		t.Helper()
		d, err := db.Deliver(scope, DeliveryRequest{Input: "리나에게 묻는다", Payload: payload, Query: q, Model: "m", Chars: chars, Secret: secret})
		if err != nil {
			t.Fatal(err)
		}
		if d.Chars > chars || d.SecretChars > secret {
			t.Fatalf("over a ceiling: %d/%d %d/%d", d.Chars, chars, d.SecretChars, secret)
		}
		return d
	}
	hold := func(id, h string) {
		t.Helper()
		if _, err := db.SetMemoryHold("chat:A", id, h); err != nil {
			t.Fatal(err)
		}
	}
	q := []float32{1, 0, 0}
	before := deliver(q, 2000, 0)
	if len(before.IDs) != 1 || before.IDs[0] != ids["long"] {
		t.Fatalf("fixture: %v", before.IDs)
	}

	// pinned: first, and kept past scene contrast
	hold(ids["short"], HoldPin)
	d := deliver(q, 2000, 0)
	if len(d.IDs) != 2 || d.IDs[0] != ids["short"] || d.IDs[1] != ids["long"] {
		t.Fatalf("pinned short first, then the ranked long: %v", d.IDs)
	}
	pinRow := false
	for _, r := range d.Trace {
		if r.ID == ids["short"] {
			pinRow = r.Hold == HoldPin && r.Result == "taken"
		}
	}
	if !pinRow {
		t.Fatalf("trace does not show the pin taken: %+v", d.Trace)
	}
	// first into the ceiling: with room for one, the pinned one
	if d := deliver(q, d.Chars-1, 0); len(d.IDs) != 1 || d.IDs[0] != ids["short"] {
		t.Fatalf("ceiling for one: %v", d.IDs)
	}
	// without a query vector too (no similarity at all)
	if d := deliver(nil, 2000, 0); len(d.IDs) == 0 || d.IDs[0] != ids["short"] {
		t.Fatalf("no query: %v", d.IDs)
	}

	// the request holds turn 4: its pinned record stays out
	hold(ids["recent"], HoldPin)
	if d := deliver(q, 2000, 0); has(d.IDs, ids["recent"]) {
		t.Fatalf("pinned record of a turn the request holds: %v", d.IDs)
	}

	// a pinned secret goes by the secret ceiling
	hold(ids["secret"], HoldPin)
	if d := deliver(q, 2000, 0); has(d.IDs, ids["secret"]) {
		t.Fatalf("secret ceiling 0 delivered a pinned secret: %v", d.IDs)
	}
	if d := deliver(q, 0, 4000); len(d.IDs) != 1 || d.IDs[0] != ids["secret"] || d.SecretChars == 0 {
		t.Fatalf("pinned secret under its own ceiling: %+v", d.IDs)
	}

	// muted: never delivered, even when it ranks first
	hold(ids["short"], "")
	hold(ids["recent"], "")
	hold(ids["secret"], "")
	hold(ids["long"], HoldMute)
	if d := deliver(q, 2000, 0); has(d.IDs, ids["long"]) {
		t.Fatalf("muted record delivered: %v", d.IDs)
	}
	for _, r := range deliver(q, 2000, 0).Trace {
		if r.ID == ids["long"] {
			t.Fatalf("muted record considered: %+v", r)
		}
	}
	// an edit keeps the flag
	body := "리나는 마을 약국의 약사다"
	if err := db.EditMemory("chat:A", ids["long"], &body, nil); err != nil {
		t.Fatal(err)
	}
	if d := deliver(q, 2000, 0); has(d.IDs, ids["long"]) {
		t.Fatalf("muted record delivered after an edit: %v", d.IDs)
	}

	// before the next index only the short record has a vector among the
	// candidates: the contrast has no reference and the baseline goes (both)
	hold(ids["long"], "")
	if d := deliver(q, 2000, 0); len(d.IDs) != 2 || !has(d.IDs, ids["long"]) || !has(d.IDs, ids["short"]) {
		t.Fatalf("one candidate vector, baseline: %v", d.IDs)
	}
	// cleared: the block is as before any flag (the edited body aside). The
	// edit unlinked its turn's vectors until the next index; link them again
	// as that index does, so the scene contrast reads the same vectors as
	// before (11-11 B2: its mean is the candidates, so with one vector left
	// it has no reference and keeps the baseline)
	if err := db.SetDocHashes(map[string]string{ids["long"]: TextHash("doc long"), ids["secret"]: TextHash("doc secret")}); err != nil {
		t.Fatal(err)
	}
	hold(ids["long"], "")
	again := deliver(q, 2000, 0)
	if len(again.IDs) != 1 || again.IDs[0] != ids["long"] {
		t.Fatalf("cleared: %v", again.IDs)
	}

	// a value a later record replaced is not delivered, pinned or not
	lina := ""
	k, err := db.CurrentKnown(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range k.Names {
		if n.Name == "리나" {
			lina = n.ID
		}
	}
	a24Save(t, db, turnSource(s, 2), `{"memories":[{"id":"m1","kind":"fact","about":"`+lina+`","slot":"취미","mode":"성향","body":"리나는 이제 등산을 좋아한다","was":"`+ids["short"]+`"}]}`)
	hold(ids["short"], HoldPin)
	if d := deliver(nil, 2000, 0); has(d.IDs, ids["short"]) {
		t.Fatalf("replaced pinned value delivered: %v", d.IDs)
	}

	// only pin, mute or none; another chat's memory is not found
	if _, err := db.SetMemoryHold("chat:A", ids["long"], "always"); !errors.Is(err, ErrBadHold) {
		t.Fatalf("bad hold: %v", err)
	}
	if _, err := db.SetMemoryHold("chat:B", ids["long"], HoldPin); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("other chat: %v", err)
	}
	if changed, err := db.SetMemoryHold("chat:A", ids["short"], HoldPin); err != nil || changed {
		t.Fatalf("same flag again: changed %v %v", changed, err)
	}
}

// With no flag set the delivery is byte for byte the same whether or not
// the flags were read: a flag set and cleared leaves the block as it was.
func TestDeliverNoHoldSameBlock(t *testing.T) {
	db, s, ids := deliverChat(t)
	scope := memory.ScopeOf(s)
	payload := printsOf(turnSource(s, 3))
	req := DeliveryRequest{Input: "리나에게 묻는다", Payload: payload, Query: []float32{0, 1, 0}, Model: "m", Chars: 2000, Secret: 4000}
	a, err := db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		db.SetMemoryHold("chat:A", id, HoldMute)
		db.SetMemoryHold("chat:A", id, "")
	}
	b, err := db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Block == "" || a.Block != b.Block || len(a.Trace) != len(b.Trace) {
		t.Fatalf("block changed:\n%q\n%q", a.Block, b.Block)
	}
}

// A muted record's text does not come back as the basis quoted on a view
// that is delivered (the view itself still is).
func TestDeliverMutedBasisNotQuoted(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 소문을 들었다", "들음"}, {"걸었다", "길"}}, "서진이 배신자라고?")
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m0","kind":"event","body":"도윤이 서진의 배신을 말했다"},{"id":"m1","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진이 배신자라고 믿는다","basis":{"via":"소문","from":"n3","ref":"m0","confidence":"낮음"}}]}`)
	if _, err := db.SetMemoryHold("chat:A", it.Memories[0].ID, HoldMute); err != nil {
		t.Fatal(err)
	}
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진이 배신자라고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Block, "배신자라고 믿는다") || strings.Contains(d.Block, "도윤이 서진의 배신을 말했다") || has(d.IDs, it.Memories[0].ID) {
		t.Fatalf("muted basis shown: %q", d.Block)
	}
}
