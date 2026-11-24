package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

func TestHUDInjectionSnapshotIncludesDeliveryStages(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := narrSeed(t, h, "S")
	reply := injectionRequest(t, srv, h, "S")
	stages, _ := reply.V["stages"].([]any)
	if len(stages) == 0 {
		t.Fatal("delivery returned no timing stages")
	}
	turns := hudGet(t, h, "S")["turns"].([]any)
	in := turns[0].(map[string]any)["injection"].(map[string]any)
	if !reflect.DeepEqual(in["stages"], stages) || !reflect.DeepEqual(injectionGet(t, h, "S").V["stages"], stages) {
		t.Fatalf("published stages %v, delivery %v", in["stages"], stages)
	}
}

func TestMaintenanceResetForgetsResendAndRequestPrefix(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := narrSeed(t, h, "R")
	inject := injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st)
	send := func(chat string, back []string) memAnswer {
		t.Helper()
		data, err := json.Marshal(map[string]any{"chat_id": chat, "payload": prints("user", "hello"), "handed_back": back})
		if err != nil {
			t.Fatal(err)
		}
		reply := callMem(t, inject, "POST", "/memory/inject", string(data))
		if reply.Code != 200 {
			t.Fatal(reply.Raw)
		}
		return reply
	}
	first := send("R", nil)
	back := titlePrints(first.V["block"].(string))
	if len(back) == 0 || send("R", back).V["delivery_id"] != first.V["delivery_id"] {
		t.Fatal("expected a reusable answer before reset")
	}
	send("P", nil)
	m := &maintenance{db: h.db, mods: h.st.mods, worker: h.worker, pipe: h.pipe, server: srv, status: h.st}
	if reply := callMem(t, m.reset, "POST", "/maintenance/reset", `{"phrase":"전체 DB 초기화"}`); reply.Code != 200 {
		t.Fatal(reply.Raw)
	}
	// Restore the same host rows, without the erased generated memories.
	injectionSeed(t, h, "R")
	if after := send("R", back); after.V["block"] == first.V["block"] {
		t.Error("resent request returned the erased memory block")
	}
	send("P", nil)
	if prefix := lastTrace(t, h, "P").Summary.Prefix; prefix != nil {
		t.Errorf("first request after reset compared with the erased request: %+v", prefix)
	}
}

func TestDiagnosticsMasksDecisionPreprocessAndNarrativeKeys(t *testing.T) {
	h := hudFixture(t, "", 0)
	m := h.st.mods
	if err := h.db.SetDecisionSettings(m.box, store.DecisionSettings{Key: "autumn glass"}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: store.PreprocessOff, AI: provider.Config{Key: "summer pencil"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetNarrativeSettings(m.box, store.NarrativeSettings{Level: store.GuideOff, Input: store.InputOff, AI: provider.Config{Key: "winter paper"}}); err != nil {
		t.Fatal(err)
	}
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.ReportSecrets = m.diagnosticSecrets
	reply := callMem(t, srv.Handler().ServeHTTP, "POST", "/diagnostics/report", `{"plugin":{"recent_errors":[{"message":"context: autumn glass / summer pencil / winter paper"}]}}`)
	if reply.Code != 200 || !strings.Contains(reply.Raw, "context: [가림] / [가림] / [가림]") {
		t.Fatalf("expected all three configured values to be masked (status %d)", reply.Code)
	}
}
