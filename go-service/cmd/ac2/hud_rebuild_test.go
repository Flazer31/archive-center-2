package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/store"
)

// HUD rebuild (user 11-08: "기억 생성 실패해도 뭐 알려주는 게 없는데 HUD가
// 왜 필요해", "다시 시도를 했을 때에도 HUD에서 알려줬으면"). A confirmed
// turn is followed until its generation ends even when nothing is queued
// for it at that moment; a failed call tells which try it was of how many.
func TestHUDFollowsConfirmedTurnUntilItEnds(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[]}`}})
	}))
	defer model.Close()
	h := hudFixture(t, model.URL, 0)
	// the turn is confirmed by the next input, but the worker is not told
	// (the gap between being confirmed and being queued)
	h.pipe.Changed = nil
	if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "GAP", memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "답"), memRow(2, "user", "u2", "", "둘"))}); err != nil {
		t.Fatal(err)
	}
	view := hudGet(t, h, "GAP")
	row := hudTurnView(t, view, 1)
	if gen := hudStageView(t, row, "generation"); gen["state"] != "waiting" || view["active"] != true {
		t.Fatalf("a confirmed turn waiting for its generation is not followed: %v / %v", gen, view["active"])
	}
	// it runs and makes nothing: done, and nothing to index
	if err := h.pipe.WithSession("chat:GAP", func(s *lifecycle.Session) { h.worker.Notify(s.Key, memory.Own(s)) }); err != nil {
		t.Fatal(err)
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool {
		return hudStageView(t, hudTurnView(t, hudGet(t, h, "GAP"), 1), "generation")["state"] == "done"
	})
	view = hudGet(t, h, "GAP")
	row = hudTurnView(t, view, 1)
	if vec := hudStageView(t, row, "vectors"); vec["state"] != "skipped" {
		t.Fatalf("no memory made: vectors %v", vec)
	}
	if view["active"] != false {
		t.Fatalf("ended: still followed %v", view["active"])
	}
}

func TestHUDTellsTheTryOfAFailedCall(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "synthetic failure", 503) }))
	defer model.Close()
	h := hudFixture(t, model.URL, store.DefaultRetries)
	if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "TRY", memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "답"), memRow(2, "user", "u2", "", "둘"))}); err != nil {
		t.Fatal(err)
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool { return hudTurnView(t, hudGet(t, h, "TRY"), 1)["retry"] == true })
	row := hudTurnView(t, hudGet(t, h, "TRY"), 1)
	if row["attempt"] != float64(1) || row["attempts"] != float64(1+store.DefaultRetries) {
		t.Fatalf("try %v of %v", row["attempt"], row["attempts"])
	}
	if gen := hudStageView(t, row, "generation"); gen["state"] != "retry_waiting" || gen["detail"] == "" {
		t.Fatalf("failed call: %v", gen)
	}
}
