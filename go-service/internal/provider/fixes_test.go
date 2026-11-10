package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AC1's addresses reach the same endpoints: a GLM base without /v1, a full
// Gemini or Vertex address, and an explicit transport path.
func TestAddressesAsAC1(t *testing.T) {
	s, got := fake(t)
	for _, c := range []struct {
		cfg  Config
		path string
	}{
		{Config{Provider: "custom", Model: "glm-5.3", URL: s.URL + "/api/paas/v4"}, "/api/paas/v4/chat/completions"},
		{Config{Provider: "custom", Model: "m", URL: s.URL + "/api"}, "/api/v1/chat/completions"},
		{Config{Provider: "gemini", Model: "gemini-3.8-flash", Key: "k", URL: s.URL + "/v1beta/models/gemini-3.8-flash:generateContent"}, "/v1beta/models/gemini-3.8-flash:generateContent"},
		{Config{Provider: "gemini", Model: "gemini-3.8-flash", Key: "k", URL: s.URL + "/v1beta/models/gemini-3.8-flash"}, "/v1beta/models/gemini-3.8-flash:generateContent"},
		{Config{Provider: "gemini", Model: "gemini-3.8-flash", Key: "k", URL: s.URL + "/v1beta"}, "/v1beta/models/gemini-3.8-flash:generateContent"},
		{Config{Provider: "opencode", Model: "gpt-5", Key: "k", URL: s.URL + "/zen/v1/responses"}, "/zen/v1/responses"},
		{Config{Provider: "opencode", Model: "gpt-5", Key: "k", URL: s.URL + "/zen/v1/chat/completions"}, "/zen/v1/chat/completions"},
		{Config{Provider: "opencode", Model: "gpt-5", Key: "k", URL: s.URL + "/zen/v1"}, "/zen/v1/responses"},
		{Config{Provider: "openai", Model: "gpt-5", Key: "k", URL: s.URL + "/v1/responses"}, "/v1/responses"},
	} {
		before := len(got())
		chat(t, c.cfg)
		if g := got(); len(g) != before+1 || g[before].path != c.path {
			t.Fatalf("%+v went to %v, want %s", c.cfg, g[before:], c.path)
		}
	}
	if e := vertexEndpoint("https://x/v1/projects/p/locations/global/publishers/google/models/gemini-3:streamGenerateContent", "gemini-3"); !strings.HasSuffix(e, "/models/gemini-3:generateContent") {
		t.Fatalf("vertex full address %s", e)
	}
	if e := vertexEndpoint("https://x/models", "gemini-3"); e != "https://x/models/gemini-3:generateContent" {
		t.Fatalf("vertex base %s", e)
	}
}

// GLM takes thinking with its own effort values, as AC1 sends them; other
// models keep the value as set.
func TestGLMReasoning(t *testing.T) {
	s, got := fake(t)
	for _, c := range []struct{ model, set, want string }{
		{"glm-5.3", "medium", "high"},
		{"glm-5.3", "low", "low"},
		{"glm-5.2", "low", "high"},
		{"glm-4.7", "medium", "high"},
	} {
		n := len(got())
		chat(t, Config{Provider: "custom", Model: c.model, URL: s.URL + "/api/paas/v4", Reasoning: c.set})
		b := got()[n].body
		if b["reasoning_effort"] != c.want || b["thinking"].(map[string]any)["type"] != "enabled" {
			t.Fatalf("%s %s: %v", c.model, c.set, b)
		}
	}
	n := len(got())
	chat(t, Config{Provider: "custom", Model: "qwen", URL: s.URL + "/v1", Reasoning: "medium"})
	if b := got()[n].body; b["reasoning_effort"] != "medium" || b["thinking"] != nil {
		t.Fatalf("other model %v", b)
	}
}

// A key saved for Ollama is sent as a Bearer token (Ollama's cloud API);
// without one nothing is sent.
func TestOllamaSendsItsKey(t *testing.T) {
	s, got := fake(t)
	chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL, Key: "ollama-key-1"})
	chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL})
	g := got()
	if g[0].header.Get("Authorization") != "Bearer ollama-key-1" || g[1].header.Get("Authorization") != "" {
		t.Fatalf("auth %q %q", g[0].header.Get("Authorization"), g[1].header.Get("Authorization"))
	}
}

// echo refuses every call with the credentials it received in the error
// text, after pad characters (so a secret can sit where the text is cut);
// the token endpoints answer as the services do.
func echo(t *testing.T, pad int) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot-token":
			w.Write([]byte(`{"token":"copilot-session-SECRET"}`))
			return
		case "/oauth-token":
			w.Write([]byte(`{"access_token":"vertex-access-SECRET","expires_in":3600}`))
			return
		}
		cred := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + r.Header.Get("x-api-key") + r.Header.Get("x-goog-api-key")
		w.WriteHeader(401)
		w.Write([]byte(strings.Repeat("x", pad) + "bad credential " + cred))
	}))
	t.Cleanup(s.Close)
	return s
}

// An error text never carries a credential the request used: an exchanged
// token, a short key, a key where the text is cut, or a Voyage key.
func TestErrorsMaskEveryCredential(t *testing.T) {
	call := func(c Config) string {
		t.Helper()
		ch, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ch.Chat(context.Background(), "p")
		if err == nil {
			t.Fatalf("%+v: no error", c)
		}
		return err.Error()
	}
	s := echo(t, 0)
	oldC, oldV := copilotTokenURL, vertexTokenURL
	copilotTokenURL, vertexTokenURL = s.URL+"/copilot-token", s.URL+"/oauth-token"
	defer func() { copilotTokenURL, vertexTokenURL = oldC, oldV }()
	if e := call(Config{Provider: "copilot", Model: "gpt-5", URL: s.URL, Key: "ghu_user"}); strings.Contains(e, "SECRET") || !strings.Contains(e, "***") {
		t.Fatalf("copilot: %s", e)
	}
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	sa, _ := json.Marshal(map[string]string{"client_email": "bot@p.iam", "project_id": "p", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	if e := call(Config{Provider: "vertex", Model: "g", URL: s.URL + "/models", Key: string(sa)}); strings.Contains(e, "SECRET") {
		t.Fatalf("vertex: %s", e)
	}
	if e := call(Config{Provider: "custom", Model: "m", URL: s.URL + "/v1", Key: "k9z"}); strings.Contains(e, "k9z") {
		t.Fatalf("short key: %s", e)
	}
	if e := call(Config{Provider: "claude", Model: "claude-x", URL: s.URL, Key: "sk-ant-0123456789"}); strings.Contains(e, "sk-ant") {
		t.Fatalf("claude: %s", e)
	}
	if e := call(Config{Provider: "ollama", Model: "m", URL: s.URL, Key: "ollama-key-77"}); strings.Contains(e, "ollama-key") {
		t.Fatalf("ollama: %s", e)
	}
	// the key across the 600-byte cut: no part of it is left
	cut := echo(t, 600-len("bad credential ")-4)
	if e := call(Config{Provider: "custom", Model: "m", URL: cut.URL + "/v1", Key: "sk-live-abcdefgh"}); strings.Contains(e, "sk-l") {
		t.Fatalf("cut key: %s", e)
	}
	v := Voyage{URL: s.URL + "/embeddings", Model: "voyage-4-large", Key: "pa-voyage-1234"}
	if _, err := v.Embed(context.Background(), []string{"a"}, "query"); err == nil || strings.Contains(err.Error(), "pa-voyage") {
		t.Fatalf("voyage: %v", err)
	}
	if _, err := v.EmbedGroups(context.Background(), [][]string{{"a"}}, "query"); err == nil || strings.Contains(err.Error(), "pa-voyage") {
		t.Fatalf("voyage groups: %v", err)
	}
}

// OpenCode Go gets a session and a User-Agent on every transport, as AC1
// sends them; OpenCode Zen and others do not. Its GLM-5.3 takes the effort
// without native thinking.
func TestOpenCodeGoHeaders(t *testing.T) {
	s, got := fake(t)
	for _, m := range []string{"kimi-k2.7-code", "gpt-5", "minimax-m2.7", "gemini-3.8-flash"} {
		chat(t, Config{Provider: "opencode-go", Model: m, Key: "k", URL: s.URL + "/zen/go/v1"})
	}
	chat(t, Config{Provider: "opencode", Model: "kimi-k2.7-code", Key: "k", URL: s.URL + "/zen/v1"})
	g := got()
	for i, r := range g[:4] {
		if !strings.HasPrefix(r.header.Get("x-opencode-session"), "archive-center-") || r.header.Get("User-Agent") != "ArchiveCenter/4.5.0" {
			t.Fatalf("go call %d (%s) headers %v", i, r.path, r.header)
		}
	}
	if g[4].header.Get("x-opencode-session") != "" {
		t.Fatalf("zen got the Go session")
	}
	chat(t, Config{Provider: "opencode-go", Model: "glm-5.3-flash", Key: "k", URL: s.URL + "/zen/go/v1", Reasoning: "low"})
	chat(t, Config{Provider: "opencode", Model: "glm-5.3-flash", Key: "k", URL: s.URL + "/zen/v1", Reasoning: "low"})
	g = got()
	if b := g[5].body; b["reasoning_effort"] != "low" || b["thinking"] != nil {
		t.Fatalf("go glm-5.3 %v", b)
	}
	if b := g[6].body; b["reasoning_effort"] != "low" || b["thinking"] == nil {
		t.Fatalf("zen glm-5.3 %v", b)
	}
}

// An OpenCode address given whole (…/chat/completions) gets the same GLM
// handling as the base address.
func TestOpenCodeFullChatAddressGLM(t *testing.T) {
	s, got := fake(t)
	chat(t, Config{Provider: "opencode", Model: "glm-5.3", Key: "k", URL: s.URL + "/zen/v1/chat/completions", Reasoning: "medium"})
	chat(t, Config{Provider: "opencode-go", Model: "glm-5.3", Key: "k", URL: s.URL + "/zen/go/v1/chat/completions", Reasoning: "medium"})
	g := got()
	if b := g[0].body; b["reasoning_effort"] != "high" || b["thinking"] == nil {
		t.Fatalf("zen full address %v", b)
	}
	if b := g[1].body; b["reasoning_effort"] != "high" || b["thinking"] != nil {
		t.Fatalf("go full address %v", b)
	}
}

// OpenCode Go's session is the chat's, as AC1 sends it (its chat session id
// hashed); a call outside a chat gets AC1's auxiliary value.
func TestOpenCodeGoSessionIsTheChats(t *testing.T) {
	s, got := fake(t)
	want := func(sid string) string {
		sum := sha256.Sum256([]byte(sid))
		return "archive-center-" + hex.EncodeToString(sum[:16])
	}
	ch, err := New(Config{Provider: "opencode-go", Model: "kimi-k2.7-code", Key: "k", URL: s.URL + "/zen/go/v1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"chat-a", "chat-b", ""} {
		ctx := context.Background()
		if sid != "" {
			ctx = WithSession(ctx, sid)
		}
		if _, err := ch.Chat(ctx, "p"); err != nil {
			t.Fatal(err)
		}
	}
	g := got()
	if g[0].header.Get("x-opencode-session") != want("chat-a") || g[1].header.Get("x-opencode-session") != want("chat-b") || g[2].header.Get("x-opencode-session") != want("archive-center-auxiliary") {
		t.Fatalf("sessions %q %q %q", g[0].header.Get("x-opencode-session"), g[1].header.Get("x-opencode-session"), g[2].header.Get("x-opencode-session"))
	}
}
