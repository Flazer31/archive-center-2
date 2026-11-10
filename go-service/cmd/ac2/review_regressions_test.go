package main

// Regressions from the 11-05 full review (R1 status/HUD error echo, R5 the
// support report dropping the concurrent counters).

import (
	"archivecenter2/goservice/internal/observe"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A loopback failure body is the only provider boundary. No model/embedding
// service is contacted, and no credentials or user content are read.
// Since user decision 11-08 the status and HUD show the provider's error text
// as received; only the credential-shaped value inside it is masked.
func TestStatusHUDShowProviderErrorTextMasksCredentials(t *testing.T) {
	const marker = "SYNTHETIC_INPUT_BODY SYNTHETIC_PROTECTED_NAME"
	credential := "sk-" + "synthetic-echoed-credential" // two parts: no literal key in source
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Error("unexpected local path")
			http.Error(w, "unexpected", 400)
			return
		}
		var req map[string]any
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("bad local request")
		}
		http.Error(w, "echo "+marker+" api_key="+credential, http.StatusBadRequest)
	}))
	defer server.Close()
	h := hudFixture(t, server.URL, 0)
	u, a := memRow(0, "user", "u1", "", "synthetic input"), memRow(1, "char", "a1", "g1", "synthetic reply")
	for seq, rows := range [][]string{{u}, {u, a}, {u, a, memRow(2, "user", "u2", "", "next")}} {
		if _, err := h.pipe.Process([]observe.Record{memEvent(seq+1, "beforeRequest", "A", rows...)}); err != nil {
			t.Fatal(err)
		}
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool { return h.worker.Chat("chat:A").GivenUp > 0 && !h.worker.Chat("chat:A").Running })
	status := callMem(t, h.st.handler(), "GET", "/status?chat_id=A", "")
	hud := callMem(t, h.handler(), "GET", "/hud?chat_id=A", "")
	t.Logf("status_echo=%v hud_echo=%v", strings.Contains(status.Raw, marker), strings.Contains(hud.Raw, marker))
	if !strings.Contains(status.Raw, marker) {
		t.Error("status dropped the upstream error text")
	}
	if !strings.Contains(hud.Raw, marker) {
		t.Error("HUD dropped the upstream error text")
	}
	if strings.Contains(status.Raw, credential) || strings.Contains(hud.Raw, credential) {
		t.Error("status/HUD exposed the credential echoed by the upstream")
	}
}

func TestSupportReportKeepsConcurrentCounts(t *testing.T) {
	// Run's live map is covered by the real concurrency suite. Here the
	// Processor interface supplies only safe counters to the report owner.
	s := observe.NewServer(nil)
	s.After = reportCountsProcessor{}
	s.Token = observe.FixedToken("synthetic-report-token")
	req := httptest.NewRequest("GET", "/diagnostics/report", nil)
	req.Header.Set(observe.TokenHeader, "synthetic-report-token")
	r := httptest.NewRecorder()
	s.Public().ServeHTTP(r, req)
	if r.Code != 200 {
		t.Fatalf("report status %d", r.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	life := out["diagnostics"].(map[string]any)["lifecycle"].(map[string]any)
	t.Logf("report safe counters: %v", life)
	if life["generation_running_chats"] != float64(2) || life["index_running_chats"] != float64(1) || life["index_retry_waiting_chats"] != float64(1) {
		t.Fatal("shareable report drops concurrent generation/index counters")
	}
}

type reportCountsProcessor struct{}

func (reportCountsProcessor) Process(_ []observe.Record) (bool, error) { return false, nil }
func (reportCountsProcessor) Diagnostics() map[string]any {
	return map[string]any{"generation_running": true, "generation_running_chats": 2, "index_running_chats": 1, "index_retry_waiting_chats": 1}
}
