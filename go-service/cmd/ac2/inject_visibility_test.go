package main

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
)

// 1.4 I257: a pair's memory can be in the making while a request is being
// prepared. Production /memory/inject reads only finished jobs: an answer
// received but not yet stored adds nothing (no half-stored memory), and
// once stored it goes with the next request. (The pair the current input
// confirms is shown in the request itself, so its memory is not delivered
// then anyway; an earlier pair, not shown, is used here.)
func TestInjectReadsOnlyFinishedMemory(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "V")
	srv := narrSeed(t, h, "V2") // an observe server on the same pipeline
	injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[
	 {"id":"m1","kind":"fact","about":"n1","slot":"약속","mode":"상태","body":"리나는 해 지기 전에 돌아오겠다고 약속했다"}]}`)
	late := scope[1].Source // a pair the request does not show
	raw := `{"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[
	 {"id":"m1","kind":"fact","about":"n1","slot":"생각","mode":"상태","body":"리나는 오늘 늦게 돌아온 것을 미안해한다"}]}`
	j, err := h.db.JobFor(late)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.KeepResponse(j, "m", "req", "[]", raw); err != nil {
		t.Fatal(err)
	}
	before := injectionRequest(t, srv, h, "V")
	block, _ := before.V["block"].(string)
	if !strings.Contains(block, "약속했다") || strings.Contains(block, "미안해한다") {
		t.Fatalf("while the last pair's answer is only received: %q", block)
	}
	out, notes := memory.Parse(raw)
	if len(notes) > 0 {
		t.Fatal(notes)
	}
	if _, err := h.db.SaveGeneration(j, store.Generation{Request: "req", ReadItems: "[]", Response: raw, Output: out}); err != nil {
		t.Fatal(err)
	}
	after := injectionRequest(t, srv, h, "V")
	if block, _ := after.V["block"].(string); !strings.Contains(block, "미안해한다") {
		t.Fatalf("once stored, the memory goes with the next request: %q", block)
	}
}
