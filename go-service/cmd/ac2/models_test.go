package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
)

func modelsDB(t *testing.T) *store.DB {
	t.Helper()
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, _ := sql.Open("mysql", base)
	boot.Exec("DROP DATABASE IF EXISTS ac2_test_models")
	if _, err := boot.Exec("CREATE DATABASE ac2_test_models CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE ac2_test_models"); boot.Close() })
	db, err := store.Open(base + "ac2_test_models?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func callModels(t *testing.T, h http.HandlerFunc, method, body string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, "/settings/models", strings.NewReader(body)))
	var v map[string]any
	json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, v, rec.Body.String()
}

// The settings screen saves the critic and the embedding: what a provider
// lacks is refused with its code, a key is stored sealed and shown only by
// its end, the worker and delivery take the new connections at once, and a
// restart reads them back. Saving calls no provider.
func TestModelSettingsEndpoint(t *testing.T) {
	db := modelsDB(t)
	box, err := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	w := generate.NewWorker(db, nil, "", time.Minute)
	m := &models{db: db, box: box, worker: w, flagEmbed: "voyage-context-4", flagRetries: 3}
	saved, err := db.ModelSettings(box)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(saved, false)
	h := m.handler()

	code, v, _ := callModels(t, h, "GET", "")
	critic := v["critic"].(map[string]any)
	if code != 200 || critic["source"] != "none" || critic["key_set"] != false || critic["retries"].(float64) != 3 || critic["known_chars"].(float64) != store.DefaultKnownChars || len(v["providers"].([]any)) != 13 {
		t.Fatalf("first view %d %v", code, v)
	}
	if m.query() != nil {
		t.Fatalf("query vectors without a key")
	}

	for _, c := range []struct{ body, code string }{
		{`{"critic":{"provider":"openai","model":"gpt-5"}}`, "key_required"},
		{`{"critic":{"provider":"custom","model":"m"}}`, "url_required"},
		{`{"critic":{"provider":"nope","model":"m"}}`, "unknown_provider"},
		{`{"critic":{"provider":"openai","model":"gpt-5","key":"sk-x","reasoning":"max"}}`, "bad_reasoning"},
		{`{"critic":{"provider":"ollama","model":"m","retries":11}}`, "bad_retries"},
		{`{"critic":{"provider":"ollama","model":"m","known_chars":-1}}`, "bad_known_chars"},
		{`{"embedding":{"provider":"nope","model":"m"}}`, "unknown_provider"},
		{`{"embedding":{"provider":"custom","model":"m"}}`, "url_required"},
		{`{"critic":`, "bad_request"},
	} {
		code, v, raw := callModels(t, h, "POST", c.body)
		if code != 400 || v["code"] != c.code {
			t.Fatalf("%s: %d %s", c.body, code, raw)
		}
	}
	if s, _ := db.ModelSettings(box); s.Provider != "" || s.Key != "" {
		t.Fatalf("a refused setting was stored: %+v", s)
	}

	const key = "sk-live-0123456789wxyz"
	code, v, raw := callModels(t, h, "POST", `{"critic":{"provider":"openai","model":"gpt-5","reasoning":"high","retries":5,"known_chars":50000,"key":"`+key+`"},"embedding":{"provider":"voyageai","model":"voyage-4-large","key":"pa-embed-5678abcd"}}`)
	if code != 200 || strings.Contains(raw, key) || strings.Contains(raw, "pa-embed-5678abcd") {
		t.Fatalf("save answer %d %s", code, raw)
	}
	critic = v["critic"].(map[string]any)
	emb := v["embedding"].(map[string]any)
	if critic["source"] != "saved" || critic["key_last4"] != "wxyz" || critic["retries"].(float64) != 5 || critic["known_chars"].(float64) != 50000 || critic["problem"] != "" || emb["key_last4"] != "abcd" || emb["model"] != "voyage-4-large" {
		t.Fatalf("saved view %v", v)
	}
	var stored string
	if err := dbSQL(t, db).QueryRow(`SELECT value FROM settings WHERE name='critic_key'`).Scan(&stored); err != nil || strings.Contains(stored, "0123456789") || !strings.HasPrefix(stored, "v1:") {
		t.Fatalf("key not sealed in the database: %q %v", stored, err)
	}
	if q := m.query(); q == nil || q.VectorModel() != "voyage-4-large" || m.vectors != signature("voyageai|", "voyage-4-large", "pa-embed-5678abcd") {
		t.Fatalf("delivery query provider %+v", q)
	}
	if d := w.Diagnostics(); d["generation_retries"] != 5 || d["generation_known_max"] != 50000 {
		t.Fatalf("worker retries %v reading ceiling %v", d["generation_retries"], d["generation_known_max"])
	}

	// a change without a key keeps the stored one
	if code, v, _ := callModels(t, h, "POST", `{"critic":{"provider":"openai","model":"gpt-5-mini","reasoning":"low","retries":5}}`); code != 200 || v["critic"].(map[string]any)["key_last4"] != "wxyz" || v["critic"].(map[string]any)["known_chars"].(float64) != 50000 {
		t.Fatalf("key or reading ceiling lost on a change: %d %v", code, v)
	}
	// a restart reads the saved settings back
	m2 := &models{db: db, box: box, worker: generate.NewWorker(db, nil, "", time.Minute), flagEmbed: "voyage-context-4"}
	s2, err := db.ModelSettings(box)
	if err != nil {
		t.Fatal(err)
	}
	m2.apply(s2, false)
	if c := m2.view()["critic"].(map[string]any); c["model"] != "gpt-5-mini" || c["key_last4"] != "wxyz" || c["reasoning"] != "low" {
		t.Fatalf("after restart %v", c)
	}
	// generation off, key cleared
	if code, v, _ := callModels(t, h, "POST", `{"critic":{"provider":"","clear_key":true}}`); code != 200 || v["critic"].(map[string]any)["source"] != "off" || v["critic"].(map[string]any)["key_set"] != false {
		t.Fatalf("turn off: %d %v", code, v)
	}
}

// Until a critic is saved, the -gen-* flags serve; a saved one takes over.
func TestModelSettingsFlagsUntilSaved(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	w := generate.NewWorker(db, nil, "", time.Minute)
	m := &models{db: db, box: box, worker: w, flagRetries: 2, flagCritic: provider.Config{Provider: "ollama", Model: "flag-model", URL: "http://127.0.0.1:11434"}}
	m.apply(store.ModelSettings{Retries: store.DefaultRetries}, false)
	if c := m.view()["critic"].(map[string]any); c["source"] != "flags" || c["model"] != "flag-model" {
		t.Fatalf("flags view %v", c)
	}
	if d := w.Diagnostics(); d["generation_retries"] != 2 {
		t.Fatalf("flag retries %v", d["generation_retries"])
	}
	if code, v, _ := callModels(t, m.handler(), "POST", `{"critic":{"provider":"ollama","model":"saved-model","retries":4}}`); code != 200 || v["critic"].(map[string]any)["source"] != "saved" || v["critic"].(map[string]any)["model"] != "saved-model" {
		t.Fatalf("saved over flags: %d %v", code, v)
	}
}

// dbSQL opens the test database directly, to look at what is stored.
func dbSQL(t *testing.T, _ *store.DB) *sql.DB {
	t.Helper()
	c, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// Saving another critic (provider, model or key) gives the jobs left
// without an answer their calls again; a start with the saved critic, or
// saving the same one again, does not. A start with generation off, then a
// critic saved, is another critic too.
func TestSavingAnotherCriticRearmsFailedJobs(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	raw := dbSQL(t, db)
	attempts := func() int {
		var n int
		if err := raw.QueryRow(`SELECT attempts FROM gen_jobs WHERE source_key='rearm'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	settled := func(want int) bool { // the worker rearms between chats
		for i := 0; i < 50; i++ {
			if attempts() == want {
				time.Sleep(100 * time.Millisecond)
				return attempts() == want
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}
	fail := func() {
		if _, err := raw.Exec(`UPDATE gen_jobs SET attempts=4 WHERE source_key='rearm'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO gen_jobs (session_key, turn_id, source_key, source, state, attempts, created_at) VALUES ('chat:X', 1, 'rearm', '{}', 'failed', 4, NOW())`); err != nil {
		t.Fatal(err)
	}
	start := func(s store.ModelSettings) (*models, func()) {
		if err := db.SetModelSettings(box, s); err != nil {
			t.Fatal(err)
		}
		w := generate.NewWorker(db, nil, "", time.Minute)
		m := &models{db: db, box: box, worker: w}
		loaded, _ := db.ModelSettings(box)
		m.apply(loaded, false)
		ctx, cancel := context.WithCancel(context.Background())
		go w.Run(ctx)
		return m, cancel
	}
	m, stop := start(store.ModelSettings{CriticSet: true, Provider: "ollama", Model: "a", Retries: 3})
	if c := m.view()["critic"].(map[string]any); c["model"] != "a" || c["source"] != "saved" {
		t.Fatalf("saved critic not loaded at start %v", c)
	}
	if !settled(4) {
		t.Fatalf("a start rearmed the job")
	}
	h := m.handler()
	if code, _, raw := callModels(t, h, "POST", `{"critic":{"provider":"ollama","model":"a","retries":3}}`); code != 200 || !settled(4) {
		t.Fatalf("saving the same critic rearmed: %d %s attempts %d", code, raw, attempts())
	}
	if code, _, raw := callModels(t, h, "POST", `{"critic":{"provider":"ollama","model":"a","reasoning":"low","retries":3}}`); code != 200 || !settled(4) {
		t.Fatalf("a reasoning change rearmed: %d %s attempts %d", code, raw, attempts())
	}
	if code, _, raw := callModels(t, h, "POST", `{"critic":{"provider":"ollama","model":"b","retries":3}}`); code != 200 || !settled(0) {
		t.Fatalf("another critic did not rearm: %d %s attempts %d", code, raw, attempts())
	}
	stop()
	// generation off at the start, then a critic saved
	fail()
	m, stop = start(store.ModelSettings{CriticSet: true, Retries: 3})
	defer stop()
	if code, _, raw := callModels(t, m.handler(), "POST", `{"critic":{"provider":"ollama","model":"c","retries":3}}`); code != 200 || !settled(0) {
		t.Fatalf("a critic saved after an off start did not rearm: %d %s attempts %d", code, raw, attempts())
	}
}

// A key belongs to its provider: switching provider without a new key does
// not carry the old provider's key to the new address.
func TestProviderSwitchDropsTheOldKey(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	m := &models{db: db, box: box, worker: generate.NewWorker(db, nil, "", time.Minute)}
	m.apply(store.ModelSettings{Retries: store.DefaultRetries}, false)
	h := m.handler()
	if code, _, raw := callModels(t, h, "POST", `{"critic":{"provider":"openai","model":"gpt-5","key":"sk-openai-1234"}}`); code != 200 {
		t.Fatal(raw)
	}
	code, v, raw := callModels(t, h, "POST", `{"critic":{"provider":"custom","model":"m","url":"http://127.0.0.1:9/v1"}}`)
	if code != 200 || v["critic"].(map[string]any)["key_set"] != false {
		t.Fatalf("switch kept the key: %d %s", code, raw)
	}
	if s, _ := db.ModelSettings(box); s.Key != "" {
		t.Fatalf("stored key after the switch")
	}
	// a provider that needs one asks for its own
	if code, v, _ := callModels(t, h, "POST", `{"critic":{"provider":"claude","model":"c"}}`); code != 400 || v["code"] != "key_required" {
		t.Fatalf("claude without its key: %d %v", code, v)
	}
}

// "사용 안 함" saved on the screen stays off even with -gen-* flags, and a
// restart reads it back; the flags' retry count is what the screen shows
// while they serve.
func TestSavedOffOverridesFlags(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	flag := provider.Config{Provider: "ollama", Model: "flag-model", URL: "http://127.0.0.1:11434"}
	w := generate.NewWorker(db, nil, "", time.Minute)
	m := &models{db: db, box: box, worker: w, flagCritic: flag, flagRetries: 2}
	s, _ := db.ModelSettings(box)
	m.apply(s, false)
	if c := m.view()["critic"].(map[string]any); c["source"] != "flags" || c["retries"] != 2 {
		t.Fatalf("flags view %v", c)
	}
	// saving only the embedding leaves the flags serving
	if code, v, _ := callModels(t, m.handler(), "POST", `{"embedding":{"model":"voyage-4-large"}}`); code != 200 || v["critic"].(map[string]any)["source"] != "flags" {
		t.Fatalf("embedding save: %v", v)
	}
	if code, v, _ := callModels(t, m.handler(), "POST", `{"critic":{"provider":""}}`); code != 200 || v["critic"].(map[string]any)["source"] != "off" || v["critic"].(map[string]any)["provider"] != "" {
		t.Fatalf("off: %d %v", code, v)
	}
	m2 := &models{db: db, box: box, worker: generate.NewWorker(db, nil, "", time.Minute), flagCritic: flag, flagRetries: 2}
	s2, _ := db.ModelSettings(box)
	m2.apply(s2, false)
	if c := m2.view()["critic"].(map[string]any); c["source"] != "off" {
		t.Fatalf("after restart %v", c)
	}
}

// Saves at the same time are one after another: what is stored is what is
// in use.
func TestConcurrentSavesEndAlike(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	m := &models{db: db, box: box, worker: generate.NewWorker(db, nil, "", time.Minute)}
	m.apply(store.ModelSettings{Retries: store.DefaultRetries}, false)
	h := m.handler()
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		body := fmt.Sprintf(`{"critic":{"provider":"ollama","model":"m%d","retries":3}}`, i)
		if i%2 == 1 {
			body = fmt.Sprintf(`{"embedding":{"model":"voyage-4-large","key":"pa-key-%04d"}}`, i)
		}
		go func() { callModels(t, h, "POST", body); done <- struct{}{} }()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	stored, _ := db.ModelSettings(box)
	m.mu.RLock()
	inUse := m.saved
	m.mu.RUnlock()
	if stored != inUse || stored.Model == "" || stored.EmbedKey == "" {
		t.Fatalf("stored %+v in use %+v", stored, inUse)
	}
}

// A new embedding connection (a key set, another key or model) sends every
// stored chat to have its missing vectors filled; a save that keeps it does
// not.
func TestEmbeddingChangeFillsMissingVectors(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	pipe := pipeline.New(db)
	ev := `{"boot_id":"b","seq":1,"hook":"load","stored_chat":{"chat_id":"V","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"하나"}}]}}`
	if _, err := pipe.Process([]observe.Record{{Event: json.RawMessage(ev)}}); err != nil {
		t.Fatal(err)
	}
	w := generate.NewWorker(db, nil, "", time.Minute)
	w.Index = func(context.Context, []int64) error { return nil }
	m := &models{db: db, box: box, worker: w, flagEmbed: "voyage-context-4"}
	m.apply(store.ModelSettings{Retries: store.DefaultRetries}, false)
	queued := func() int { return w.Diagnostics()["index_backlog_chats"].(int) }
	h := m.handler()
	if callModels(t, h, "POST", `{"critic":{"provider":"ollama","model":"a"}}`); queued() != 0 {
		t.Fatalf("a critic save without an embedding queued %d", queued())
	}
	if code, _, raw := callModels(t, h, "POST", `{"embedding":{"key":"pa-first-1234"}}`); code != 200 || queued() != 1 {
		t.Fatalf("a key set queued %d: %s", queued(), raw)
	}
}

// The embedding connection follows AC1's providers: another provider
// starts without the last one's key, a provider with no key needed is
// ready without one, VOYAGE_API_KEY serves Voyage only, and the status
// names the vectors in use.
func TestEmbeddingProviderSettings(t *testing.T) {
	db := modelsDB(t)
	box, _ := secret.LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	w := generate.NewWorker(db, nil, "", time.Minute)
	m := &models{db: db, box: box, worker: w, flagEmbed: "voyage-context-4", envKey: "pa-env-0000"}
	m.apply(store.ModelSettings{Retries: store.DefaultRetries}, false)
	h := m.handler()
	emb := func(v map[string]any) map[string]any { return v["embedding"].(map[string]any) }

	// before a save: Voyage with the flag's model and the environment key
	_, v, _ := callModels(t, h, "GET", "")
	if e := emb(v); e["provider"] != "voyageai" || e["model"] != "voyage-context-4" || e["key_source"] != "env" || e["ready"] != true || len(v["embed_providers"].([]any)) != 7 {
		t.Fatalf("first view %v", e)
	}
	// OpenAI without a key: saved, not ready, and the environment key is not used
	code, v, raw := callModels(t, h, "POST", `{"embedding":{"provider":"openai","model":""}}`)
	if e := emb(v); code != 200 || e["model"] != "text-embedding-3-small" || e["key_set"] != false || e["ready"] != false || m.query() != nil {
		t.Fatalf("openai without a key %d %s", code, raw)
	}
	code, v, raw = callModels(t, h, "POST", `{"embedding":{"provider":"openai","model":"text-embedding-3-large","key":"sk-emb-1111"}}`)
	if e := emb(v); code != 200 || e["key_last4"] != "1111" || e["vector_model"] != "openai:text-embedding-3-large" || m.query() == nil {
		t.Fatalf("openai %d %s", code, raw)
	}
	// Ollama: the OpenAI key is dropped, and no key is needed
	code, v, raw = callModels(t, h, "POST", `{"embedding":{"provider":"ollama","model":"nomic-embed-text"}}`)
	if e := emb(v); code != 200 || e["key_set"] != false || e["ready"] != true || e["vector_model"] != "ollama:nomic-embed-text" {
		t.Fatalf("ollama %d %s", code, raw)
	}
	if s, _ := db.ModelSettings(box); s.EmbedKey != "" || s.EmbedProvider != "ollama" {
		t.Fatalf("stored %+v", s)
	}
	st := newStatus(db, m, w)
	if e := st.view("")["embedding"].(map[string]any); e["model"] != "ollama:nomic-embed-text" || e["state"] != "unknown" {
		t.Fatalf("status %v", e)
	}
	// back to Voyage without a key of its own: the environment key serves
	code, v, raw = callModels(t, h, "POST", `{"embedding":{"provider":"voyageai","model":"voyage-4-large"}}`)
	if e := emb(v); code != 200 || e["key_source"] != "env" || e["vector_model"] != "voyage-4-large" {
		t.Fatalf("voyage %d %s", code, raw)
	}
}
