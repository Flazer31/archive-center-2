package store

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Audit 2026-11-10: a completing fact delivered with its secret event keeps
// the event's protection (review 17 A-01) when scene contrast then removes
// the event and keeps the fact.
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
