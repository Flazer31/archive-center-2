package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Defects once reproduced, asserted fixed. Each case failed against the
// HUD before these fixes.
func TestHUDReview19FirstCallIsNotARetry(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[]}`}})
	}))
	defer func() { finish(); model.Close() }()
	h := hudFixture(t, model.URL, 0)
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "R19", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"))})
	if err != nil {
		t.Fatal(err)
	}
	hudRun(t, h.worker)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stand-in not reached")
	}
	row := hudTurnView(t, hudGet(t, h, "R19"), 1)
	if row["attempt"] != float64(1) || row["attempts"] != float64(1) || row["retried"] != false {
		t.Fatalf("first call: %v", row)
	}
	finish()
	hudWait(t, func() bool {
		return hudStageView(t, hudTurnView(t, hudGet(t, h, "R19"), 1), "generation")["state"] == "done"
	})
}

func TestHUDReview19ManualRetryIsToldAsAsked(t *testing.T) {
	h := hudFixture(t, "http://127.0.0.1:1", 0) // no worker is started, so no HTTP call
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "R19M", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"))})
	if err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	h.pipe.WithSession("chat:R19M", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.Turns[0]) })
	j, err := h.db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.StartAttempt(j); err != nil {
		t.Fatal(err)
	}
	if err = h.db.FailJob(j, "synthetic", "", "", "synthetic prior failure", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	response := callMem(t, h.handler(), "POST", "/hud/retry", `{"chat_id":"R19M","turn":1}`)
	if response.Code != 200 {
		t.Fatal(response.Raw)
	}
	row := hudTurnView(t, response.V, 1)
	if hudStageView(t, row, "generation")["state"] != "waiting" || row["next_at"] != nil || row["retry"] != false || row["retried"] != true {
		t.Fatalf("manual retry asked: %v", row)
	}
}

func TestHUDReview19OlderFailureIsTold(t *testing.T) {
	h := hudFixture(t, "", 0)
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "R19OLD", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"), memRow(3, "char", "a2", "g2", "answer2"), memRow(4, "user", "u3", "", "three"))})
	if err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	h.pipe.WithSession("chat:R19OLD", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.Turns[0]) })
	j, err := h.db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.StartAttempt(j); err != nil {
		t.Fatal(err)
	}
	if err = h.db.FailJob(j, "synthetic", "", "", "synthetic failure", time.Now()); err != nil {
		t.Fatal(err)
	}
	var second memory.Source
	h.pipe.WithSession("chat:R19OLD", func(s *lifecycle.Session) { second = memory.SourceOf(s.Key, s.Turns[1]) })
	injectionSave(t, h.db, second, `{"memories":[]}`)
	view := hudGet(t, h, "R19OLD")
	if hudStageView(t, hudTurnView(t, view, 2), "generation")["state"] != "done" {
		t.Fatal(view)
	}
	fs, _ := view["failures"].([]any)
	if len(fs) != 1 {
		t.Fatalf("older failure not told: %v", view["failures"])
	}
	f := fs[0].(map[string]any)
	if f["turn"] != float64(1) || f["state"] != "failed" || f["retry"] != true || f["detail"] == "" {
		t.Fatalf("older failure: %v", f)
	}
}

func TestHUDReview19PreprocessErrorIsShownWhole(t *testing.T) {
	h := hudFixture(t, "", 0)
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "R19P", memRow(0, "user", "u1", "", "one"))})
	if err != nil {
		t.Fatal(err)
	}
	h.st.injected("R19P", store.Delivery{Preprocess: &store.PreprocessRecord{Mode: "ai", Ran: "go", Fallback: "ai_failed", Error: "provider 503: synthetic protected phrase api_key=sk-" + "synthetic123456"}}, "", 2000, 4000, 0, nil, false)
	view := hudGet(t, h, "R19P")
	row := hudTurnView(t, view, 1)
	prep := row["injection"].(map[string]any)["preprocess"].(map[string]any)
	if e, _ := prep["error"].(string); e != "provider 503: synthetic protected phrase api_key=[가림]" {
		t.Fatalf("preprocess error: %v", prep)
	}
}

// Since 11-08 the provider's error text (here echoing a phrase) is shown in the
// HUD's preprocess error; the stored memory body itself is still not part of it.
func TestHUDReview19PreprocessErrorShowsProviderText(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "R19ECHO")
	const protected = "SYNTHETIC-PROTECTED-PHRASE-19"
	injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n2","name":"도윤"}],"memories":[{"id":"m1","kind":"event","body":"리나의 약속 SYNTHETIC-PROTECTED-PHRASE-19","hide_from":["n2"]}]}`)
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Messages) == 0 || !strings.Contains(request.Messages[len(request.Messages)-1].Content, protected) {
			t.Error("synthetic protected memory absent from request")
		}
		http.Error(w, "provider echoed protected input: "+protected+" api_key=sk-"+"synthetic-echo-credential", 503)
	}))
	defer ai.Close()
	err := h.db.SetPreprocessSettings(h.st.mods.box, store.PreprocessSettings{Mode: store.PreprocessAI, AIOwn: true, AI: provider.Config{Provider: "custom", Model: "synthetic", URL: ai.URL + "/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	injectionRequest(t, srv, h, "R19ECHO")
	response := callMem(t, h.handler(), "GET", "/hud?chat_id=R19ECHO", "")
	if response.Code != 200 || !strings.Contains(response.Raw, "provider echoed protected input: "+protected+" api_key=[가림]") || !strings.Contains(response.Raw, `"fallback":"ai_failed"`) {
		t.Fatalf("hud: %s", response.Raw)
	}
	if strings.Contains(response.Raw, "synthetic-echo-credential") || strings.Contains(response.Raw, "리나의 약속") {
		t.Fatalf("hud shows a credential or the stored memory body: %s", response.Raw)
	}
}

func TestHUDReview19UnsavedFailureIsTold(t *testing.T) {
	h := hudFixture(t, "http://127.0.0.1:1", 0) // StartAttempt fails before the provider boundary
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "R19WRITE", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"))})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_, err = conn.Exec(`CREATE TRIGGER review19_fail_update BEFORE UPDATE ON gen_jobs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic write denied'`)
	if err != nil {
		t.Fatal(err)
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		c := h.chats["chat:R19WRITE"]
		return c != nil && c.turns[1] != nil && c.turns[1].state == "failed"
	})
	view := hudGet(t, h, "R19WRITE")
	row := hudTurnView(t, view, 1)
	gen := hudStageView(t, row, "generation")
	if (gen["state"] != "retry_waiting" && gen["state"] != "failed") || gen["detail"] == nil || row["retry"] != true {
		t.Fatalf("unsaved failure: %v", view)
	}
}

func TestHUDReview19UnsavedAnswerFailureIsTold(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[]}`}})
	}))
	defer model.Close()
	h := hudFixture(t, model.URL, 0)
	_, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "R19SAVE", memRow(0, "user", "u1", "", "one"), memRow(1, "char", "a1", "g1", "answer"), memRow(2, "user", "u2", "", "two"))})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_, err = conn.Exec(`CREATE TRIGGER review19_fail_save BEFORE UPDATE ON gen_jobs FOR EACH ROW BEGIN IF NEW.state IN ('received','done') THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic save denied'; END IF; END`)
	if err != nil {
		t.Fatal(err)
	}
	hudRun(t, h.worker)
	hudWait(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		c := h.chats["chat:R19SAVE"]
		return c != nil && c.turns[1] != nil && c.turns[1].state == "failed"
	})
	view := hudGet(t, h, "R19SAVE")
	row := hudTurnView(t, view, 1)
	gen := hudStageView(t, row, "generation")
	if (gen["state"] != "retry_waiting" && gen["state"] != "failed") || gen["detail"] == nil || row["retry"] != true {
		t.Fatalf("unsaved failure: %v", view)
	}
}
