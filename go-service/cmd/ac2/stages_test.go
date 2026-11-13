package main

import (
	"reflect"
	"testing"
	"time"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/store"
)

// A memory request tells where its time went (1.1 A1): the steps in the
// order they ran, adding up to total_ms, the same in the answer, the
// kept trace and the last-delivery view; the publisher's wait is the
// narrative step, and its two calls run at once.
func TestDeliveryStagesSplitTotal(t *testing.T) {
	h := hudFixture(t, "", 0)
	injectionSeed(t, h, "S")
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	f := newNarrFake(t)
	f.guide = `{"connections":[{"text":"리나의 약속이 남았다","refs":["D"]}]}`
	f.input = `{"unchanged":true}`
	f.delay = 300 * time.Millisecond
	narrSet(t, h, f, store.GuideWeak, store.InputClarify, store.DefaultGuideChars)
	a := injectionRequest(t, srv, h, "S")

	want := []string{"observe", "session", "embed", "body", "select", "preprocess", "narrative", "other"}
	raw, _ := a.V["stages"].([]any)
	var names []string
	var sum, narrative int64
	for _, x := range raw {
		m := x.(map[string]any)
		names = append(names, m["name"].(string))
		ms := int64(m["ms"].(float64))
		sum += ms
		if m["name"] == "narrative" {
			narrative = ms
		}
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("stages %v, want %v", names, want)
	}
	total := int64(a.V["total_ms"].(float64))
	// each step is rounded down to whole ms, so the parts may sum a few ms
	// under the total, never over it
	if sum > total || total-sum > int64(len(want)) {
		t.Fatalf("stages sum %d, total %d", sum, total)
	}
	// both publisher calls wait 300 ms at once: the narrative step holds
	// that wait, once
	if narrative < 300 || narrative >= 600 {
		t.Fatalf("narrative step %d ms (two 300 ms calls at once)", narrative)
	}
	rec := lastTrace(t, h, "S").Summary
	if len(rec.Stages) != len(want) || rec.TotalMS != total || rec.Narrative == nil || rec.Narrative.GuideMS < 300 || rec.Narrative.InputMS < 300 {
		t.Fatalf("trace: stages %+v total %d narrative %+v", rec.Stages, rec.TotalMS, rec.Narrative)
	}
	got := injectionGet(t, h, "S")
	if v, _ := got.V["stages"].([]any); len(v) != len(want) {
		t.Fatalf("last-delivery view stages: %v", got.V["stages"])
	}
}
