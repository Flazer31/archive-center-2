package store

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// Recheck counterexamples through the real store, kept as regressions.
func TestAstra18BranchParentSurvivesRestart(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	raw := `{"boot_id":"b18","seq":1,"hook":"load","stored_chat":{"chat_id":"C","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"m","disabled":true,"is_comment":true,"data":{"text":"{{specialcomment::branchedfrom::A::name::a1::}}"}}]}}`
	_, err := p.Process([]observe.Record{{Event: json.RawMessage(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	s, ok, err := db.LoadSession("chat:C")
	if err != nil || !ok {
		t.Fatalf("load %v %v", ok, err)
	}
	if s.BranchOf != [2]string{"chat:A", "c:a1"} {
		t.Fatalf("branch parent lost after DB restore: %v", s.BranchOf)
	}
}
func TestAstra18HTTPRetryDuplicatesRevisionHistory(t *testing.T) {
	db := testDB(t)
	srv := observe.NewServer(RawSink{DB: db})
	srv.After = pipeline.New(db)
	body := `{"contract":"observation.v1","events":[{"boot_id":"b18","seq":1,"hook":"output","event_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a","data":{"text":"event"}}]},"reread_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a","data":{"text":"reread"}}]}}]}`
	var counts []int
	for i := 0; i < 3; i++ {
		if i == 2 {
			srv.After = pipeline.New(db)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/observe", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		s, _, err := db.LoadSession("chat:A")
		if err != nil {
			t.Fatal(err)
		}
		counts = append(counts, len(s.Turns[0].Responses[0].Revisions))
	}
	raw, _ := db.RawAfter(0, 10)
	t.Logf("raw records=%d revisions after original/retry/restart-retry=%v", len(raw), counts)
	if counts[1] != counts[0] || counts[2] != counts[0] {
		t.Fatal("raw exactly once does not imply lifecycle exactly once")
	}
}
func TestAstra18ProgressPersistsPerChat(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	raw := func(chat, body string, seq int) observe.Record {
		e := map[string]any{"boot_id": "b18", "seq": seq, "hook": "beforeRequest", "stored_chat": map[string]any{"chat_id": chat, "rows": []any{map[string]any{"i": 0, "role": "user", "chat_id": "u", "data": map[string]any{"text": body}}}}}
		b, _ := json.Marshal(e)
		return observe.Record{Event: b}
	}
	for _, r := range []observe.Record{raw("A", "current", 2), raw("B", "other", 3)} {
		if _, err := p.Process([]observe.Record{r}); err != nil {
			t.Fatal(err)
		}
	}
	p = pipeline.New(db)
	if _, err := p.Process([]observe.Record{raw("A", "old", 1)}); err != nil {
		t.Fatal(err)
	}
	a, _, _ := db.LoadSession("chat:A")
	b, _, _ := db.LoadSession("chat:B")
	if a.Progress["b18"] != 2*4 || b.Progress["b18"] != 3*4 || a.Turns[0].Members[0].Data != "current" {
		t.Fatalf("bad progress/restore A=%v B=%v", a.Progress, b.Progress)
	}
}
