package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"archivecenter2/goservice/internal/secret"
)

// The decision-model connection (1.0 E02): saved and shown with the key's
// end only, the key sealed in the store and dropped when the provider
// changes, a key-needing service refused without one, and the connection
// test asking the saved connection exactly one question.
func TestDecisionSettings(t *testing.T) {
	db := modelsDB(t)
	box, err := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var lastAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastAuth.Store(r.Header.Get("Authorization"))
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		qs := body["questions"].(map[string]any)
		if len(qs) != 1 || qs["test"] == nil || r.URL.Path != "/v1/systemone" {
			w.WriteHeader(400)
			return
		}
		fmt.Fprintf(w, `{"model":%q,"answers":{"test":{"type":"noul","noul":0.97}},"usage":{"input_tokens":120}}`, body["model"])
	}))
	defer srv.Close()
	h := decisionHandler(db, box)
	call := func(method, path, body string) memAnswer {
		t.Helper()
		return callMem(t, h, method, path, body)
	}
	offered := func(a memAnswer) []string {
		var ids []string
		for _, p := range a.V["providers"].([]any) {
			ids = append(ids, p.(map[string]any)["id"].(string))
		}
		return ids
	}
	// the local Ollama is not offered (user 11-08) unless already saved
	if a := call("GET", "/settings/decision", ""); a.Code != 200 || a.V["provider"] != "" || a.V["key_set"] != false || strings.Join(offered(a), ",") != "ollama_cloud,typesafe" {
		t.Fatalf("empty view %d %s", a.Code, a.Raw)
	}
	if a := call("POST", "/settings/decision/test", ""); a.Code != 400 || a.V["code"] != "decision_off" {
		t.Fatalf("test while off %d %s", a.Code, a.Raw)
	}
	for _, c := range []struct{ body, code string }{
		{`{"provider":"nope"}`, "unknown_provider"},
		{`{"provider":"typesafe"}`, "key_required"},
		{`{"provider":"ollama_cloud","model":"clef"}`, "key_required"},
		{`{"provider":`, "bad_request"},
	} {
		if a := call("POST", "/settings/decision", c.body); a.Code != 400 || a.V["code"] != c.code {
			t.Fatalf("%s: %d %s", c.body, a.Code, a.Raw)
		}
	}
	// local Ollama: no key needed; the defaults are shown
	a := call("POST", "/settings/decision", `{"provider":"ollama","url":"`+srv.URL+`"}`)
	if strings.Join(offered(a), ",") != "ollama,ollama_cloud,typesafe" {
		t.Fatalf("a saved local Ollama is not shown: %s", a.Raw)
	}
	if a.Code != 200 || a.V["model"] != "clef:cloud" || a.V["url"] != srv.URL {
		t.Fatalf("ollama save %d %s", a.Code, a.Raw)
	}
	if a := call("POST", "/settings/decision/test", ""); a.Code != 200 || a.V["ok"] != true || a.V["model"] != "clef:cloud" || calls.Load() != 1 || lastAuth.Load() != "" {
		t.Fatalf("test %d %s calls %d", a.Code, a.Raw, calls.Load())
	}
	// a key: stored sealed, shown by its end; another provider drops it
	a = call("POST", "/settings/decision", `{"provider":"typesafe","url":"`+srv.URL+`","key":"sk-test-abcd9876"}`)
	if a.Code != 200 || a.V["key_set"] != true || a.V["key_last4"] != "9876" || strings.Contains(a.Raw, "sk-test-abcd9876") {
		t.Fatalf("typesafe save %d %s", a.Code, a.Raw)
	}
	var raw string
	dbSQL(t, db).QueryRow(`SELECT value FROM settings WHERE name='decision_key'`).Scan(&raw)
	if raw == "" || strings.Contains(raw, "sk-test-abcd9876") {
		t.Fatalf("key not sealed: %q", raw)
	}
	if a := call("POST", "/settings/decision/test", ""); a.V["ok"] != true || lastAuth.Load() != "Bearer sk-test-abcd9876" {
		t.Fatalf("typesafe test %s auth %v", a.Raw, lastAuth.Load())
	}
	// saving the same provider without a key keeps it
	if a := call("POST", "/settings/decision", `{"provider":"typesafe","url":"`+srv.URL+`"}`); a.V["key_set"] != true {
		t.Fatalf("kept key %s", a.Raw)
	}
	if a := call("POST", "/settings/decision", `{"provider":"ollama","url":"`+srv.URL+`"}`); a.V["key_set"] != false {
		t.Fatalf("another provider kept the key: %s", a.Raw)
	}
	// a reply without the test question's answer says so (review 16 B-01)
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"answers":{}}`) }))
	defer empty.Close()
	call("POST", "/settings/decision", `{"provider":"ollama","url":"`+empty.URL+`"}`)
	if a := call("POST", "/settings/decision/test", ""); a.V["ok"] != false || a.V["error"] != "no_answer" {
		t.Fatalf("empty answer %s", a.Raw)
	}
	if a := call("POST", "/settings/decision", `{"provider":""}`); a.Code != 200 || a.V["provider"] != "" {
		t.Fatalf("off %s", a.Raw)
	}
}
