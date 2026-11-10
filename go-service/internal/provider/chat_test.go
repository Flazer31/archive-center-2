package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// seen is one request the fake provider received.
type seen struct {
	path   string
	header http.Header
	body   map[string]any
	form   url.Values
}

// fake answers each transport by path, like the real services, and records
// every request; an unknown path fails the test.
func fake(t *testing.T) (*httptest.Server, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var got []seen
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := seen{path: r.URL.Path, header: r.Header.Clone()}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			rec.form, _ = url.ParseQuery(string(raw))
		} else if len(raw) > 0 {
			json.Unmarshal(raw, &rec.body)
		}
		mu.Lock()
		got = append(got, rec)
		mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/fail"):
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"bad key sk-live-abcdef123456"}`))
		case p == "/copilot-token":
			w.Write([]byte(`{"token":"copilot-session"}`))
		case p == "/oauth-token":
			w.Write([]byte(`{"access_token":"vertex-access","expires_in":3600}`))
		case strings.HasSuffix(p, "/chat/completions"):
			w.Write([]byte(`{"choices":[{"message":{"content":"chat ok","reasoning_content":"why"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
		case strings.HasSuffix(p, "/responses"):
			w.Write([]byte(`{"output":[{"type":"reasoning","content":[]},{"type":"message","content":[{"type":"output_text","text":"responses ok"}]}],"usage":{"input_tokens":5,"output_tokens":6}}`))
		case strings.HasSuffix(p, "/messages"):
			w.Write([]byte(`{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"claude ok"}],"usage":{"input_tokens":7,"output_tokens":8}}`))
		case strings.HasSuffix(p, ":generateContent"):
			w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"secret thought","thought":true},{"text":"gemini ok"}]}}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":10}}`))
		case p == "/api/chat":
			w.Write([]byte(`{"message":{"content":"ollama ok"}}`))
		default:
			t.Errorf("unexpected path %s", p)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	return s, func() []seen { mu.Lock(); defer mu.Unlock(); return append([]seen(nil), got...) }
}

func chat(t *testing.T, c Config) Result {
	t.Helper()
	ch, err := New(c)
	if err != nil {
		t.Fatalf("%s: %v", c.Provider, err)
	}
	r, err := ch.Chat(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("%s: %v", c.Provider, err)
	}
	return r
}

func TestProviderListIsAC1s(t *testing.T) {
	want := "openai claude gemini llmgateway vertex openrouter opencode opencode-go neuralwatt vercel copilot ollama custom"
	var ids []string
	for _, p := range Providers {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, " ") != want {
		t.Fatalf("providers %v", ids)
	}
}

// What a configuration lacks is named before any call.
func TestCheckNamesWhatIsMissing(t *testing.T) {
	for _, c := range []struct {
		cfg  Config
		want error
	}{
		{Config{Provider: "nope", Model: "m"}, ErrUnknownProvider},
		{Config{Provider: "openai", Key: "k"}, ErrModelRequired},
		{Config{Provider: "openai", Model: "m"}, ErrKeyRequired},
		{Config{Provider: "custom", Model: "m"}, ErrURLRequired},
		{Config{Provider: "openai", Model: "m", Key: "k", Reasoning: "max"}, ErrBadReasoning},
		{Config{Provider: "ollama", Model: "m"}, nil},
		{Config{Provider: "custom", Model: "m", URL: "http://x/v1"}, nil},
	} {
		if err := c.cfg.Check(); !errors.Is(err, c.want) {
			t.Fatalf("%+v: %v, want %v", c.cfg, err, c.want)
		}
	}
}

// OpenAI-compatible providers: the chat completions path, the key as a
// bearer, the effort in the field each provider takes, and the answer read
// back with its reasoning.
func TestOpenAICompatibleRequests(t *testing.T) {
	s, got := fake(t)
	r := chat(t, Config{Provider: "openai", Model: "gpt-5", URL: s.URL + "/v1", Key: "sk-o", Reasoning: "high"})
	if r.Content != "chat ok" || r.Thinking != "why" || r.PromptTokens != 3 || r.OutputTokens != 4 {
		t.Fatalf("answer %+v", r)
	}
	chat(t, Config{Provider: "openrouter", Model: "x/y", URL: s.URL + "/api/v1", Key: "sk-r", Reasoning: "low"})
	chat(t, Config{Provider: "custom", Model: "m", URL: s.URL})
	g := got()
	if g[0].path != "/v1/chat/completions" || g[0].header.Get("Authorization") != "Bearer sk-o" || g[0].body["reasoning_effort"] != "high" || g[0].body["model"] != "gpt-5" {
		t.Fatalf("openai request %+v", g[0])
	}
	if g[1].path != "/api/v1/chat/completions" || g[1].header.Get("HTTP-Referer") == "" || g[1].body["reasoning"].(map[string]any)["effort"] != "low" || g[1].body["reasoning_effort"] != nil {
		t.Fatalf("openrouter request %+v", g[1])
	}
	if g[2].path != "/v1/chat/completions" || g[2].header.Get("Authorization") != "" || g[2].body["reasoning_effort"] != nil {
		t.Fatalf("custom without key or reasoning %+v", g[2])
	}
}

// Claude: the messages path and its headers; 4.6+/5 models get adaptive
// thinking with the effort, 3.7-4.5 a budget on top of the answer ceiling,
// "off" none; thinking text is kept apart from the answer.
func TestClaudeRequests(t *testing.T) {
	s, got := fake(t)
	r := chat(t, Config{Provider: "claude", Model: "claude-opus-5-5", URL: s.URL, Key: "sk-a", Reasoning: "high"})
	if r.Content != "claude ok" || r.Thinking != "hmm" {
		t.Fatalf("answer %+v", r)
	}
	chat(t, Config{Provider: "claude", Model: "claude-sonnet-4-5", URL: s.URL, Key: "sk-a", Reasoning: "medium"})
	chat(t, Config{Provider: "claude", Model: "claude-opus-5-5", URL: s.URL, Key: "sk-a", Reasoning: "off"})
	g := got()
	if g[0].path != "/v1/messages" || g[0].header.Get("x-api-key") != "sk-a" || g[0].header.Get("anthropic-version") == "" {
		t.Fatalf("claude request %+v", g[0])
	}
	if g[0].body["thinking"].(map[string]any)["type"] != "adaptive" || g[0].body["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("adaptive thinking %+v", g[0].body)
	}
	th := g[1].body["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"].(float64) != 8192 || g[1].body["max_tokens"].(float64) != claudeMaxTokens+8192 {
		t.Fatalf("budget thinking %+v", g[1].body)
	}
	if g[2].body["thinking"] != nil || g[2].body["max_tokens"].(float64) != claudeMaxTokens {
		t.Fatalf("off %+v", g[2].body)
	}
}

// Gemini: the model's generateContent path with the key header; 3.x takes a
// level, 2.5 a budget; thought parts are not the answer.
func TestGeminiRequests(t *testing.T) {
	s, got := fake(t)
	r := chat(t, Config{Provider: "gemini", Model: "gemini-3.8-flash", URL: s.URL + "/v1beta", Key: "g-key", Reasoning: "medium"})
	if r.Content != "gemini ok" || r.PromptTokens != 9 {
		t.Fatalf("answer %+v", r)
	}
	chat(t, Config{Provider: "gemini", Model: "gemini-2.5-pro", URL: s.URL + "/v1beta", Key: "g-key", Reasoning: "low"})
	g := got()
	if g[0].path != "/v1beta/models/gemini-3.8-flash:generateContent" || g[0].header.Get("x-goog-api-key") != "g-key" {
		t.Fatalf("gemini request %+v", g[0])
	}
	if tc := g[0].body["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any); tc["thinkingLevel"] != "medium" {
		t.Fatalf("level %+v", tc)
	}
	if tc := g[1].body["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any); tc["thinkingBudget"].(float64) != 1024 {
		t.Fatalf("budget %+v", tc)
	}
}

// OpenCode routes by model as AC1 does.
func TestOpenCodeRoutesByModel(t *testing.T) {
	s, got := fake(t)
	chat(t, Config{Provider: "opencode", Model: "claude-sonnet-4-5", URL: s.URL + "/zen/v1", Key: "oc"})
	if r := chat(t, Config{Provider: "opencode", Model: "gpt-5.5", URL: s.URL + "/zen/v1", Key: "oc", Reasoning: "low"}); r.Content != "responses ok" || r.OutputTokens != 6 {
		t.Fatalf("responses answer %+v", r)
	}
	chat(t, Config{Provider: "opencode", Model: "gemini-3.8-flash", URL: s.URL + "/zen/v1", Key: "oc"})
	chat(t, Config{Provider: "opencode-go", Model: "minimax-m3", URL: s.URL + "/zen/go/v1", Key: "oc"})
	chat(t, Config{Provider: "opencode-go", Model: "glm-5.3", URL: s.URL + "/zen/go/v1", Key: "oc"})
	want := []string{"/zen/v1/messages", "/zen/v1/responses", "/zen/v1/models/gemini-3.8-flash:generateContent", "/zen/go/v1/messages", "/zen/go/v1/chat/completions"}
	g := got()
	for i, p := range want {
		if g[i].path != p {
			t.Fatalf("request %d went to %s, want %s", i, g[i].path, p)
		}
	}
	if g[1].body["input"] != "prompt" || g[1].body["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("responses body %+v", g[1].body)
	}
}

// Copilot exchanges the GitHub token first and calls with the Copilot token
// and editor headers.
func TestCopilotExchangesToken(t *testing.T) {
	s, got := fake(t)
	old := copilotTokenURL
	copilotTokenURL = s.URL + "/copilot-token"
	defer func() { copilotTokenURL = old }()
	chat(t, Config{Provider: "copilot", Model: "gpt-5", URL: s.URL, Key: "ghu_user"})
	g := got()
	if g[0].path != "/copilot-token" || g[0].header.Get("Authorization") != "Bearer ghu_user" {
		t.Fatalf("exchange %+v", g[0])
	}
	if g[1].path != "/chat/completions" || g[1].header.Get("Authorization") != "Bearer copilot-session" || g[1].header.Get("Copilot-Integration-Id") != "vscode-chat" {
		t.Fatalf("copilot call %+v", g[1])
	}
}

// Vertex signs an assertion with the service account, uses the access token
// as a bearer, fills PROJECT_ID, and keeps the token for the next call.
func TestVertexServiceAccount(t *testing.T) {
	s, got := fake(t)
	old := vertexTokenURL
	vertexTokenURL = s.URL + "/oauth-token"
	defer func() { vertexTokenURL = old }()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	sa, _ := json.Marshal(map[string]string{"client_email": "bot@p.iam", "project_id": "proj-1", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	ch, err := New(Config{Provider: "vertex", Model: "gemini-3.8-flash", URL: s.URL + "/v1/projects/PROJECT_ID/locations/global/publishers/google/models", Key: string(sa)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if r, err := ch.Chat(context.Background(), "prompt"); err != nil || r.Content != "gemini ok" {
			t.Fatalf("call %d: %+v %v", i, r, err)
		}
	}
	g := got()
	if len(g) != 3 || g[0].path != "/oauth-token" || strings.Count(g[0].form.Get("assertion"), ".") != 2 {
		t.Fatalf("token request %+v (of %d)", g[0], len(g))
	}
	if g[1].path != "/v1/projects/proj-1/locations/global/publishers/google/models/gemini-3.8-flash:generateContent" || g[1].header.Get("Authorization") != "Bearer vertex-access" || g[1].header.Get("x-goog-api-key") != "" {
		t.Fatalf("vertex call %+v", g[1])
	}
}

// Ollama keeps its own API; "off" turns thinking off explicitly.
func TestOllamaThink(t *testing.T) {
	s, got := fake(t)
	chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL, Reasoning: "off"})
	chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL, Reasoning: "low"})
	chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL})
	g := got()
	if g[0].body["think"] != false || g[1].body["think"] != "low" || g[2].body["think"] != nil {
		t.Fatalf("think %v %v %v", g[0].body["think"], g[1].body["think"], g[2].body["think"])
	}
}

// A refused call names the status and never repeats the key.
func TestErrorHidesKey(t *testing.T) {
	s, _ := fake(t)
	ch, _ := New(Config{Provider: "custom", Model: "m", URL: s.URL + "/fail/chat/completions", Key: "sk-live-abcdef123456"})
	_, err := ch.Chat(context.Background(), "prompt")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "abcdef123456") {
		t.Fatalf("error %v", err)
	}
}

// Ollama follows the typed address: an OpenAI-style one is called as it is
// with the effort ("none" for off), an /api/chat one is not completed again,
// a base gets /api/chat; with none typed, Ollama's cloud API.
func TestOllamaFollowsTypedAddress(t *testing.T) {
	s, got := fake(t)
	if r := chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL + "/v1/chat/completions", Key: "ok-k", Reasoning: "off"}); r.Content != "chat ok" {
		t.Fatalf("openai-style %+v", r)
	}
	chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL + "/v1", Reasoning: "high"})
	chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL + "/api/chat"})
	chat(t, Config{Provider: "ollama", Model: "m", URL: s.URL + "/"})
	g := got()
	if g[0].path != "/v1/chat/completions" || g[0].body["reasoning_effort"] != "none" || g[0].body["think"] != nil || g[0].header.Get("Authorization") != "Bearer ok-k" {
		t.Fatalf("openai-style %+v", g[0])
	}
	if g[1].path != "/v1/chat/completions" || g[1].body["reasoning_effort"] != "high" {
		t.Fatalf("/v1 %+v", g[1])
	}
	if g[2].path != "/api/chat" || g[3].path != "/api/chat" {
		t.Fatalf("native %s %s", g[2].path, g[3].path)
	}
	if p, _ := Find("ollama"); p.DefaultURL != "https://ollama.com" {
		t.Fatalf("default %s", p.DefaultURL)
	}
}
