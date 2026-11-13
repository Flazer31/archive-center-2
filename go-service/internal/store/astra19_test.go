package store

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// Kept as a regression: after a backend restart a fingerprint-only candidate
// is filled for plain, GigaTrans and Yumi rows, rows without chatId and
// chats without id. Events go through the production HTTP handler and
// RawSink, as the plugin's do (an earlier version called Process directly,
// which skips the raw store the recovery reads).
func TestAstra19RestartObservationPreserved(t *testing.T) {
	db := testDB(t)
	for _, kind := range []string{"plain", "giga", "yumi", "no_row_id", "no_chat_id"} {
		t.Run(kind, func(t *testing.T) {
			srv := observe.NewServer(RawSink{DB: db})
			srv.After = pipeline.New(db)
			field := func(s string, full bool) map[string]any {
				n, h := ingest.Fingerprint(s)
				v := map[string]any{"len": n, "h": h}
				if full {
					v["text"] = s
				}
				return v
			}
			chat := func(body string, full bool) map[string]any {
				display := body
				vars := map[string]any{}
				if kind == "giga" {
					display = "<GigaTrans>" + body + "</GigaTrans>display " + body
				}
				if kind == "yumi" {
					display = "<!--yumi-tr:v1:t:start-->display " + body + "<!--yumi-tr:v1:t:end-->"
					b, _ := json.Marshal(map[string]string{"model": body})
					f := field(string(b), full)
					f["translation_record"] = true
					vars["$__yumi_tr.t"] = f
				}
				r := map[string]any{"i": 1, "role": "char", "chat_id": "a", "generation_id": "g1", "data": field(display, full)}
				if kind == "no_row_id" {
					delete(r, "chat_id")
				}
				c := map[string]any{"chat_id": kind, "rows": []any{map[string]any{"i": 0, "role": "user", "chat_id": "u", "data": field("q", true)}, r}, "scriptstate": vars}
				if kind == "no_chat_id" {
					delete(c, "chat_id")
				}
				return c
			}
			post := func(seq int, hook string, c, rr map[string]any) map[string]any {
				e := map[string]any{"boot_id": "a19-" + kind, "seq": seq, "hook": hook, "coords": map[string]int{"character_index": 0, "chat_index": 0}, "listener": map[string]int{"character_index": 0, "chat_index": 0, "message_index": 1}}
				if hook == "output" {
					e["event_chat"], e["reread_chat"] = c, rr
				} else {
					e["stored_chat"] = c
				}
				b, _ := json.Marshal(map[string]any{"contract": "observation.v1", "events": []any{e}})
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/observe", strings.NewReader(string(b))))
				var out map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &out)
				if w.Code != 200 {
					t.Fatalf("HTTP %d %v", w.Code, out)
				}
				return out
			}
			post(1, "load", chat("A", true), nil)
			srv.After = pipeline.New(db) // restart
			out := post(2, "output", chat("B", true), chat("A", false))
			key := "chat:" + kind
			if kind == "no_chat_id" {
				key = "coord:0/0"
			}
			s, ok, err := db.LoadSession(key)
			if !ok || err != nil {
				t.Fatalf("load %v %v", ok, err)
			}
			var current string
			for _, r := range s.Turns[0].Responses {
				if r.State == lifecycle.Active && len(r.Revisions) > 0 {
					current = r.Revisions[len(r.Revisions)-1].Data
				}
			}
			if current != "A" || out["resend_text"] == true {
				t.Fatalf("last observed candidate A: current=%q reply=%v", current, out)
			}
		})
	}
}

// ---- the chat id delimiter, kept as regressions ----

func a20Post(t *testing.T, s *observe.Server, evs ...json.RawMessage) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"contract": "observation.v1", "events": evs})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/observe", strings.NewReader(string(b))))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 {
		t.Fatalf("HTTP %d %v", w.Code, out)
	}
	return out
}

func a20Field(s string, full bool) map[string]any {
	n, h := ingest.Fingerprint(s)
	f := map[string]any{"len": n, "h": h}
	if full {
		f["text"] = s
	}
	return f
}

func a20Chat(id, body string, full bool) map[string]any {
	return map[string]any{"chat_id": id, "scriptstate": map[string]any{}, "rows": []any{
		map[string]any{"i": 0, "role": "user", "chat_id": "u", "data": a20Field("q", true)},
		map[string]any{"i": 1, "role": "char", "chat_id": "a", "generation_id": "g", "data": a20Field("<GigaTrans>"+body+"</GigaTrans>display "+body, full)}}}
}

func a20Event(seq int, c, rr map[string]any) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"boot_id": "a20", "seq": seq, "hook": "output", "listener": map[string]int{"character_index": 0, "chat_index": 0, "message_index": 1}, "event_chat": c, "reread_chat": rr})
	return b
}

func a20Current(t *testing.T, d *DB, key string) string {
	t.Helper()
	s, ok, e := d.LoadSession(key)
	if e != nil || !ok {
		t.Fatalf("load %v %v", ok, e)
	}
	for _, turn := range s.Turns {
		for _, r := range turn.Responses {
			if r.State == lifecycle.Active && len(r.Revisions) > 0 {
				return r.Revisions[len(r.Revisions)-1].Data
			}
		}
	}
	return ""
}

// An output event whose own chat (origin) differs from its re-read
// (destination): the destination's texts are found after a restart.
// testdata/astra20-mixed-chat-wire.json was produced by the real plugin.
func TestAstra20MixedChatRawRecovery(t *testing.T) {
	d := testDB(t)
	s := observe.NewServer(RawSink{DB: d})
	s.After = pipeline.New(d)
	b, err := os.ReadFile("testdata/astra20-mixed-chat-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var events []json.RawMessage
	if err := json.Unmarshal(b, &events); err != nil {
		t.Fatal(err)
	}
	a20Post(t, s, events[0])
	if got := a20Current(t, d, "chat:destination"); got != "A" {
		t.Fatal(got)
	}
	s.After = pipeline.New(d) // restart
	reply := a20Post(t, s, events[1])
	if got := a20Current(t, d, "chat:destination"); got != "A" || reply["resend_text"] == true {
		t.Fatalf("re-read A not restored: %q %v", got, reply)
	}
}

func TestAstra20ChatIDDelimiterRecovery(t *testing.T) {
	d := testDB(t)
	s := observe.NewServer(RawSink{DB: d})
	s.After = pipeline.New(d)
	a20Post(t, s, a20Event(1, a20Chat("host|chat", "A", true), nil))
	s.After = pipeline.New(d)
	a20Post(t, s, a20Event(2, a20Chat("host|chat", "B", true), a20Chat("host|chat", "A", false)))
	if got := a20Current(t, d, "chat:host|chat"); got != "A" {
		t.Fatalf("want A got %q", got)
	}
}

// ---- a row chatId containing "|" ----
func TestAstra21RowDelimiterRecovery(t *testing.T) {
	d := testDB(t)
	s := observe.NewServer(RawSink{DB: d})
	s.After = pipeline.New(d)
	chat := func(body string, full bool) map[string]any {
		c := a20Chat("row-delimiter", body, full)
		c["rows"].([]any)[1].(map[string]any)["chat_id"] = "a|suffix"
		return c
	}
	a20Post(t, s, a20Event(1, chat("A", true), nil))
	s.After = pipeline.New(d)
	reply := a20Post(t, s, a20Event(2, chat("B", true), chat("A", false)))
	if got := a20Current(t, d, "chat:row-delimiter"); got != "A" || reply["resend_text"] == true {
		t.Fatalf("current=%q reply=%v", got, reply)
	}
}
