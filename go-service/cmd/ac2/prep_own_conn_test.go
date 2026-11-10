package main

import (
	"context"
	"testing"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// A jev delivery with nothing to ask is not a judge failure; a judge that
// fails on lines asked still is.
func TestPreprocessJevNothingAskedIsNotFailure(t *testing.T) {
	h := hudFixture(t, "", 0)
	s, _ := injectionSeed(t, h, "NA")
	r := r18Request()
	b, err := h.db.Deliver(s, r)
	if err != nil || len(b.IDs) != 0 {
		t.Fatalf("fixture: %v %v", b.IDs, err)
	}
	f := newPrepFakes(t)
	dec, err := provider.NewDecider(provider.DecisionConfig{Provider: "ollama", URL: f.dec.URL})
	if err != nil {
		t.Fatal(err)
	}
	d, rec := preprocess(context.Background(), &prepDeps{mode: store.PreprocessJev, decider: dec}, h.db, s, r, b, r.Input)
	if rec.Fallback != "nothing_to_choose" || rec.DecisionCalls != 0 || rec.Asked != 0 || rec.Error != "" || len(d.IDs) != 0 {
		t.Fatalf("nothing asked: fallback %q calls %d asked %d err %q ids %v", rec.Fallback, rec.DecisionCalls, rec.Asked, rec.Error, d.IDs)
	}

	// negative: lines asked and the judge down is still decision_failed
	s2, _ := injectionSeed(t, h, "NB")
	injectionSave(t, h.db, s2[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"리나는 약사다"}]}`)
	b2, err := h.db.Deliver(s2, r)
	if err != nil || len(b2.IDs) != 1 {
		t.Fatalf("fixture 2: %v %v", b2.IDs, err)
	}
	f.decFail = true
	d2, rec2 := preprocess(context.Background(), &prepDeps{mode: store.PreprocessJev, decider: dec}, h.db, s2, r, b2, r.Input)
	if rec2.Fallback != "decision_failed" || rec2.Asked == 0 || rec2.Error == "" || len(d2.IDs) != 1 {
		t.Fatalf("judge down: fallback %q asked %d err %q ids %v", rec2.Fallback, rec2.Asked, rec2.Error, d2.IDs)
	}
}
