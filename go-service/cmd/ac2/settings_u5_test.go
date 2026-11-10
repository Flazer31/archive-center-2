package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
)

func u5Call(t *testing.T, h http.HandlerFunc, method, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, "/", strings.NewReader(body)))
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return rec.Code, v
}

func TestU5ModelSettingsRoundTripAndRequest(t *testing.T) {
	db := modelsDB(t)
	box, err := secret.LoadBox(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = nil
		json.NewDecoder(r.Body).Decode(&sent)
		header = r.Header.Get("X-U5")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	m := &models{db: db, box: box}
	saved, _ := db.ModelSettings(box)
	m.apply(saved, false)
	c := map[string]any{"provider": "custom", "model": "test", "url": srv.URL, "retries": 0, "timeout_ms": 2500, "temperature": 0, "max_tokens": 987, "extra_headers_json": `{"X-U5":"saved"}`, "extra_body_json": `{"top_p":0.6}`, "service_tier": "flex", "claude_prompt_cache": "off"}
	b, _ := json.Marshal(map[string]any{"critic": c})
	code, v := u5Call(t, m.handler(), "POST", string(b))
	if code != 200 {
		t.Fatalf("save %d %v", code, v)
	}
	// A new owner after restart reads every value from MariaDB, then sends it.
	saved, err = db.ModelSettings(box)
	if err != nil {
		t.Fatal(err)
	}
	m2 := &models{db: db, box: box}
	m2.apply(saved, false)
	v = m2.view()
	cv := v["critic"].(map[string]any)
	encoded, _ := json.Marshal(cv)
	var read map[string]any
	json.Unmarshal(encoded, &read)
	for _, name := range []string{"timeout_ms", "temperature", "max_tokens", "extra_headers_json", "extra_body_json", "service_tier", "claude_prompt_cache"} {
		wantBytes, _ := json.Marshal(c[name])
		gotBytes, _ := json.Marshal(read[name])
		if string(wantBytes) != string(gotBytes) {
			t.Fatalf("lost %s on restart", name)
		}
	}
	cfg, _ := m2.critic(saved)
	p, err := provider.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Chat(context.Background(), "source"); err != nil {
		t.Fatal(err)
	}
	if sent["temperature"] != float64(0) || sent["max_tokens"] != float64(987) || sent["top_p"] != 0.6 || sent["service_tier"] != "flex" || header != "saved" {
		t.Fatal("saved options not applied to the next request")
	}
	before := saved
	for _, name := range []string{"extra_headers_json", "extra_body_json"} {
		for _, bad := range []string{"{", "[]", "null", `{} {}`} {
			c[name] = bad
			b, _ = json.Marshal(map[string]any{"critic": c})
			code, v = u5Call(t, m.handler(), "POST", string(b))
			if code != 400 || v["code"] != "bad_"+name {
				t.Fatalf("bad JSON %s: %d %v", name, code, v)
			}
			after, _ := db.ModelSettings(box)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("invalid JSON changed saved settings")
			}
		}
		c[name] = ""
	}
	for _, name := range []string{"timeout_ms", "temperature", "max_tokens"} {
		c[name] = nil
	}
	c["service_tier"] = ""
	c["claude_prompt_cache"] = ""
	b, _ = json.Marshal(map[string]any{"critic": c})
	code, _ = u5Call(t, m.handler(), "POST", string(b))
	if code != 200 {
		t.Fatal("clear")
	}
	saved, _ = db.ModelSettings(box)
	cfg, _ = m.critic(saved)
	p, _ = provider.New(cfg)
	p.Chat(context.Background(), "source")
	if sent["temperature"] != nil || sent["max_tokens"] != nil || sent["top_p"] != nil || sent["service_tier"] != nil || header != "" || cfg.TimeoutMS != nil {
		t.Fatal("clearing did not restore defaults")
	}
}

func TestU5PromptEndpointControlsGeneration(t *testing.T) {
	db := modelsDB(t)
	h := promptHandler(db)
	code, v := u5Call(t, h, "GET", "")
	if code != 200 || v["content"] != memory.Instructions || v["custom"] != false {
		t.Fatal("default view")
	}
	custom := "Custom critic instructions\nJSON shape: {\"memories\":[]}"
	b, _ := json.Marshal(map[string]any{"content": custom})
	code, v = u5Call(t, h, "POST", string(b))
	if code != 200 || v["custom"] != true || v["content"] != custom {
		t.Fatal("save prompt")
	}
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		prompts = append(prompts, body.Messages[0].Content)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"memories\":[]}"}}]}`))
	}))
	defer srv.Close()
	p, _ := provider.New(provider.Config{Provider: "custom", Model: "fake", URL: srv.URL})
	src := memory.Source{Session: "chat:U5", Turn: 1, Members: []memory.SourceMember{{Key: "u1", Data: "첫 입력"}}, Responses: []memory.SourceResponse{{Key: "a1", Seq: 1, Data: "첫 응답"}}, Dates: map[string]string{"date": "day 3"}}
	if _, err := generate.Turn(context.Background(), db, p, "fake", nil, src); err != nil {
		t.Fatal(err)
	}
	if prompts[0] != memory.PromptWithInstructions(src, "", custom) {
		t.Fatal("stored instructions did not reach actual provider request with source/date")
	}
	// A restore deletes the override; subsequent generation uses today's code default.
	code, v = u5Call(t, h, "POST", `{"reset":true}`)
	if code != 200 || v["content"] != memory.Instructions || v["custom"] != false {
		t.Fatal("restore prompt")
	}
	var n int
	if err := dbSQL(t, db).QueryRow(`SELECT COUNT(*) FROM settings WHERE name='critic_prompt'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("restore did not delete override")
	}
	src.Turn = 2
	if _, err := generate.Turn(context.Background(), db, p, "fake", nil, src); err != nil {
		t.Fatal(err)
	}
	if prompts[1] != memory.Prompt(src, "") {
		t.Fatal("default not restored in generation")
	}
}

func TestU5MemorySettingsControlInject(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	h := settingsHandler(db)
	_, v := u5Call(t, h, "GET", "")
	if v["placement"] != "auto" || v["anchor"] != "" {
		t.Fatal("placement defaults")
	}
	msgs := []memory.PayloadMessage{{Role: "system", Marker: true}, {Role: "user"}, {Role: "assistant", CachePoint: true}, {Role: "system", Marker: true}, {Role: "user"}, {Role: "assistant"}}
	body, _ := json.Marshal(map[string]any{"chat_id": "U5", "placement_messages": msgs})
	inject := injectHandler(&observe.Server{}, pipe, db, func() *provider.Embedder { return nil }, nil)
	for _, mode := range memory.PlacementModes {
		b, _ := json.Marshal(map[string]any{"placement": mode, "anchor": " <U5> "})
		code, v := u5Call(t, h, "POST", string(b))
		if code != 200 || v["placement"] != mode || v["anchor"] != "<U5>" || v["memory_chars"] != float64(store.DefaultMemoryChars) || v["secret_chars"] != float64(store.DefaultSecretChars) {
			t.Fatalf("settings %d %v", code, v)
		}
		code, v = u5Call(t, inject, "POST", string(body))
		if code != 200 {
			t.Fatalf("inject %d %v", code, v)
		}
		p := v["placement"].(map[string]any)
		want := memory.ResolvePlacement(msgs, mode, "<U5>")
		if p["index"] != float64(want.Index) || p["requested"] != mode {
			t.Fatal("API ignored saved placement")
		}
	}
	code, v := u5Call(t, h, "POST", `{"memory_chars":1700}`)
	if code != 200 || v["placement"] != "end" || v["anchor"] != "<U5>" {
		t.Fatal("budget save lost placement")
	}
}

// The real plugin calls the real settings/inject HTTP handlers against a
// disposable MariaDB. RisuAI is the simulated boundary; the provider is off.
func TestU5PluginPlacementThroughBackend(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	ev := json.RawMessage(`{"boot_id":"u5-seed","seq":1,"hook":"load","stored_chat":{"chat_id":"chat-1","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"과거 입력"}},{"i":1,"role":"char","chat_id":"a1","data":{"text":"과거 응답"}},{"i":2,"role":"user","chat_id":"u2","data":{"text":"다음 입력"}},{"i":3,"role":"char","chat_id":"a2","data":{"text":"다음 응답"}},{"i":4,"role":"user","chat_id":"u3","data":{"text":"최근 입력"}},{"i":5,"role":"char","chat_id":"a3","data":{"text":"최근 응답"}}]}}`)
	if _, err := pipe.Process([]observe.Record{{Event: ev}}); err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	pipe.WithSession("chat:chat-1", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.ActiveTurns()[0]) })
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	answer := `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"리나는 약사다"}]}`
	out, _ := memory.Parse(answer)
	if _, err := db.SaveGeneration(j, store.Generation{Request: "synthetic", Response: answer, Output: out}); err != nil {
		t.Fatal(err)
	}
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.After = pipe
	mux := http.NewServeMux()
	mux.HandleFunc("/settings/memory", settingsHandler(db))
	mux.HandleFunc("/memory/inject", injectHandler(srv, pipe, db, func() *provider.Embedder { return nil }, nil))
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	cmd := exec.Command("node", "--test", "--test-reporter=tap", "--test-name-pattern=U5 backend placement integration", "../../../tools/plugin-sim.test.cjs")
	cmd.Env = append(os.Environ(), "AC2_U5_BACKEND="+httpServer.URL)
	result, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plugin/backend integration: %v\n%s", err, result)
	}
	if !strings.Contains(string(result), "# pass 1") {
		t.Fatalf("integration did not execute: %s", result)
	}
}
