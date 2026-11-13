package store

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Audit 2026-11-10: a completing fact delivered with its secret event keeps
// the event's protection when scene contrast then removes the event and
// keeps the fact.
func TestContrastRemovedEventKeepsCompletingFactProtection(t *testing.T) {
	mems := []memory.Memory{
		// M1: the secret event of the open scene, against the scene
		{Kind: "event", Scene: "S1", Body: "몰래 장부를 옮겼다", Awareness: memory.Awareness{HideFrom: memory.Who{"N1"}}},
		// M2: a fact recorded at it that Select does not find (another
		// scene, about no name in the turn): a completion candidate
		{Kind: "fact", Scene: "S2", About: "N2", Mode: "상태", At: "M1", Body: "장부는 창고 밑에 있다"},
	}
	f := &contrastFixture{memories: mems, vectors: map[int64][]float32{1: {0, 1}, 2: {1, 0}}}
	db := fixtureDB(t, f)
	req := syntheticRequest()
	lineOf := func(d Delivery, id string) string {
		for i, x := range d.IDs {
			if x == id {
				return d.Lines[i]
			}
		}
		return ""
	}
	baseReq := req
	baseReq.Query = nil
	base, err := db.Deliver(syntheticScope(), baseReq)
	if err != nil {
		t.Fatal(err)
	}
	// without contrast both are delivered: the event carries the protection
	if !has(base.IDs, "M1") || !has(base.IDs, "M2") || !base.SecretIDs["M1"] {
		t.Fatalf("baseline does not deliver the secret event with its fact: %+v", base)
	}
	got, err := db.Deliver(syntheticScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	removed := false
	for _, tr := range got.Trace {
		removed = removed || (tr.ID == "M1" && tr.Result == "contrast_removed")
	}
	if !removed || has(got.IDs, "M1") || !has(got.IDs, "M2") {
		t.Fatalf("contrast did not remove the event and keep its fact: %+v", got)
	}
	line := lineOf(got, "M2")
	if !strings.Contains(line, "hide from: 대상") {
		t.Fatalf("the fact shown without its event lost the event's protection: %q", line)
	}
	if !got.SecretIDs["M2"] || got.SecretChars == 0 || got.Chars != 0 {
		t.Fatalf("the fact is not in the secret lane: secret %v chars %d/%d", got.SecretIDs, got.Chars, got.SecretChars)
	}
	if strings.Contains(got.PDFBlock, "장부는 창고") || !strings.Contains(got.RetainedBlock, "장부는 창고") {
		t.Fatalf("lanes: general %q secret %q", got.PDFBlock, got.RetainedBlock)
	}
}

// Audit 2026-11-10 (source trace only before 1.1): the text of a record
// recorded at a secret event keeps the event's protection wherever it is
// shown away from the event's line: quoted as a pinned view's basis, and on
// its own line when it is pinned; also when the event is muted, and along a
// record recorded at such a record. Scene contrast removes the event. A
// record at an event that is no secret stays in the general lane.
func TestSecretEventFactKeepsProtectionWhenPinnedOrQuoted(t *testing.T) {
	secretText := "장부는 창고 밑에 있다"
	deliver := func(mems []memory.Memory, vectors map[int64][]float32, holds map[int64]string) Delivery {
		t.Helper()
		f := &contrastFixture{memories: mems, vectors: vectors, holds: holds}
		got, err := fixtureDB(t, f).Deliver(syntheticScope(), syntheticRequest())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	lineOf := func(d Delivery, id string) string {
		for i, x := range d.IDs {
			if x == id {
				return d.Lines[i]
			}
		}
		return ""
	}
	check := func(name string, mems []memory.Memory, vectors map[int64][]float32, holds map[int64]string, shown string) {
		t.Helper()
		got := deliver(mems, vectors, holds)
		if has(got.IDs, "M1") || !has(got.IDs, shown) {
			t.Fatalf("%s: want the event left out and %s delivered: %+v", name, shown, got)
		}
		if !got.SecretIDs[shown] || !strings.Contains(lineOf(got, shown), "hide from: 대상") {
			t.Fatalf("%s: %s, which shows the secret event's record, lacks its protection: secret %v line %q", name, shown, got.SecretIDs, lineOf(got, shown))
		}
		if strings.Contains(got.PDFBlock, secretText) || !strings.Contains(got.RetainedBlock, secretText) {
			t.Fatalf("%s: lanes: general %q secret %q", name, got.PDFBlock, got.RetainedBlock)
		}
	}
	event := memory.Memory{Kind: "event", Scene: "S1", Body: "몰래 장부를 옮겼다", Awareness: memory.Awareness{HideFrom: memory.Who{"N1"}}}
	fact := memory.Memory{Kind: "fact", Scene: "S2", About: "N2", Mode: "상태", At: "M1", Body: secretText}
	view := memory.Memory{Kind: "view", Scene: "S3", Owner: "N3", Body: "그는 장부가 안전하다고 믿는다", Basis: &memory.Basis{Via: "경험", Ref: "M2"}}
	two := map[int64][]float32{1: {0, 1}, 2: {1, 0}}
	three := map[int64][]float32{1: {0, 1}, 2: {1, 0}, 3: {1, 0}}
	// a pinned view of another scene quotes the fact
	check("pinned view", []memory.Memory{event, fact, view}, three, map[int64]string{3: HoldPin}, "M3")
	// the fact itself is pinned
	check("pinned fact", []memory.Memory{event, fact}, two, map[int64]string{2: HoldPin}, "M2")
	// the event is muted (never delivered): its protection still goes
	check("muted event", []memory.Memory{event, fact}, two, map[int64]string{1: HoldMute, 2: HoldPin}, "M2")
	// a record recorded at the fact, pinned: the protection goes along
	deeper := memory.Memory{Kind: "fact", Scene: "S3", About: "N3", Mode: "상태", At: "M2", Body: "창고 열쇠는 " + secretText}
	check("record at the fact", []memory.Memory{event, fact, deeper}, map[int64][]float32{1: {0, 1}, 2: {0, 1}, 3: {1, 0}}, map[int64]string{3: HoldPin}, "M3")
	// an event that is no secret: the pinned fact stays in the general lane
	open := event
	open.Awareness = memory.Awareness{}
	got := deliver([]memory.Memory{open, fact}, two, map[int64]string{2: HoldPin})
	if !has(got.IDs, "M2") || got.SecretIDs["M2"] || !strings.Contains(got.PDFBlock, secretText) {
		t.Fatalf("a fact at an event that is no secret moved to the secret lane: %+v", got)
	}
}
