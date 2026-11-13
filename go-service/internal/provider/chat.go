package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config is the critic's model connection (memory generation), as the
// settings screen sets it. The providers and their addresses follow AC1
// (source/go-service/internal/httpapi/proxy_provider.go, llm_settings_view.go).
type Config struct {
	Options
	Provider  string // an Info.ID
	Model     string
	URL       string // empty: the provider's default address
	Key       string // Vertex AI: the service account JSON
	Reasoning string // "" (the provider's default) or one of the provider's Info.Reasoning
	// NoRedirect: a redirect is not followed, so the key and the text go
	// only to the address set (preprocessing); a 3xx is a failure. Off
	// for the critic, as before.
	NoRedirect bool
}

// Info describes a provider for the settings screen.
type Info struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	DefaultURL string   `json:"default_url"`
	NeedsURL   bool     `json:"needs_url"` // the address must be given (no default)
	NeedsKey   bool     `json:"needs_key"`
	Reasoning  []string `json:"reasoning"` // choices besides "" (default)
	// DefaultModel: the model suggested when the provider is picked
	// (embedding providers)
	DefaultModel string `json:"default_model,omitempty"`
}

var (
	effort      = []string{"low", "medium", "high"}
	claudeLevel = []string{"off", "low", "medium", "high", "max"}
	ollamaLevel = []string{"off", "low", "medium", "high"}
)

// Providers are AC1's thirteen, in its order.
var Providers = []Info{
	{ID: "openai", Label: "OpenAI", DefaultURL: "https://api.openai.com/v1", NeedsKey: true, Reasoning: effort},
	{ID: "claude", Label: "Claude", DefaultURL: "https://api.anthropic.com", NeedsKey: true, Reasoning: claudeLevel},
	{ID: "gemini", Label: "Gemini", DefaultURL: "https://generativelanguage.googleapis.com/v1beta", NeedsKey: true, Reasoning: effort},
	{ID: "llmgateway", Label: "LLM Gateway", DefaultURL: "https://api.llmgateway.io/v1", NeedsKey: true, Reasoning: effort},
	{ID: "vertex", Label: "Vertex AI", DefaultURL: "https://aiplatform.googleapis.com/v1/projects/PROJECT_ID/locations/global/publishers/google/models", NeedsKey: true, Reasoning: effort},
	{ID: "openrouter", Label: "OpenRouter", DefaultURL: "https://openrouter.ai/api/v1", NeedsKey: true, Reasoning: effort},
	{ID: "opencode", Label: "OpenCode", DefaultURL: "https://opencode.ai/zen/v1", NeedsKey: true, Reasoning: effort},
	{ID: "opencode-go", Label: "OpenCode Go", DefaultURL: "https://opencode.ai/zen/go/v1", NeedsKey: true, Reasoning: effort},
	{ID: "neuralwatt", Label: "Neuralwatt", DefaultURL: "https://api.neuralwatt.com/v1", NeedsKey: true, Reasoning: effort},
	{ID: "vercel", Label: "Vercel AI Gateway", DefaultURL: "https://ai-gateway.vercel.sh/v1", NeedsKey: true, Reasoning: effort},
	{ID: "copilot", Label: "GitHub Copilot", DefaultURL: "https://api.githubcopilot.com", NeedsKey: true, Reasoning: effort},
	// Ollama's own cloud API by default (users run the large models there,
	// not on their PC); a local Ollama is its address typed in
	{ID: "ollama", Label: "Ollama", DefaultURL: "https://ollama.com", Reasoning: ollamaLevel},
	{ID: "custom", Label: "직접 입력 (OpenAI 호환)", NeedsURL: true, NeedsKey: false, Reasoning: effort},
}

// Find returns the provider with this id.
func Find(id string) (Info, bool) {
	for _, p := range Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Info{}, false
}

// Errors for a configuration that cannot make a call (stable codes on the
// settings screen).
var (
	ErrUnknownProvider = errors.New("unknown_provider")
	ErrModelRequired   = errors.New("model_required")
	ErrURLRequired     = errors.New("url_required")
	ErrKeyRequired     = errors.New("key_required")
	ErrBadReasoning    = errors.New("bad_reasoning")
)

// Check reports what the configuration lacks for a call.
func (c Config) Check() error {
	if err := c.Options.Check(); err != nil {
		return err
	}
	p, ok := Find(c.Provider)
	if !ok {
		return ErrUnknownProvider
	}
	if strings.TrimSpace(c.Model) == "" {
		return ErrModelRequired
	}
	if p.NeedsURL && strings.TrimSpace(c.URL) == "" {
		return ErrURLRequired
	}
	if p.NeedsKey && strings.TrimSpace(c.Key) == "" {
		return ErrKeyRequired
	}
	if c.Reasoning != "" {
		found := false
		for _, r := range p.Reasoning {
			found = found || r == c.Reasoning
		}
		if !found {
			return ErrBadReasoning
		}
	}
	return nil
}

// Chatter is one model connection.
type Chatter interface {
	Chat(ctx context.Context, prompt string) (Result, error)
}

// New makes the connection the configuration describes.
func New(c Config) (Chatter, error) {
	p, err := newChat(c)
	if err != nil {
		return nil, err
	}
	if c.CallTimeout() > 0 {
		return timedChat{p, c.Options}, nil
	}
	return p, nil
}

func newChat(c Config) (Chatter, error) {
	if err := c.Check(); err != nil {
		return nil, err
	}
	p, _ := Find(c.Provider)
	base := strings.TrimRight(strings.TrimSpace(c.URL), "/")
	if base == "" {
		base = p.DefaultURL
	}
	c.URL = base
	switch c.Provider {
	case "ollama":
		// the typed address is followed: an OpenAI-style one
		// (…/v1/chat/completions, …/v1) is called as it is, Ollama's
		// reasoning_effort taking the setting ("none" for off); otherwise
		// the native /api/chat at it
		if p := urlPath(base); strings.HasSuffix(p, "/chat/completions") || strings.HasSuffix(p, "/v1") {
			if c.Reasoning == "off" {
				c.Reasoning = "none"
			}
			return openAIChat{c: c, endpoint: openAIChatEndpoint(base, c.Provider, false)}, nil
		}
		base = strings.TrimSuffix(base, "/api/chat")
		var think any
		switch c.Reasoning {
		case "off":
			think = false
		case "low", "medium", "high":
			think = c.Reasoning
		}
		o := Ollama{URL: base, Model: c.Model, Think: think, Key: c.Key, Options: c.Options}
		if c.NoRedirect {
			o.HTTP = &http.Client{Timeout: 10 * time.Minute, CheckRedirect: noRedirect}
		}
		return o, nil
	case "claude":
		return claudeChat{c: c, endpoint: claudeEndpoint(base)}, nil
	case "gemini":
		return geminiChat{c: c, endpoint: geminiEndpoint(base, c.Model)}, nil
	case "vertex":
		return &vertexChat{c: c}, nil
	case "opencode", "opencode-go":
		return openCode(c, base), nil
	default:
		if strings.HasSuffix(urlPath(base), "/responses") {
			return responsesChat{c: c, endpoint: base}, nil
		}
		glm := isGLM(c.Model, base, c.Provider)
		return openAIChat{c: c, endpoint: openAIChatEndpoint(base, c.Provider, glm), glm: glm}, nil
	}
}

// urlPath is an address's path without a trailing slash, in lower case.
func urlPath(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimRight(u.Path, "/"))
}

// openAIChatEndpoint completes a base address as AC1 does
// (proxyOpenAIChatEndpoint): a GLM base (/api/paas/v4) takes no /v1.
func openAIChatEndpoint(base, provider string, glm bool) string {
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"), provider == "copilot", glm:
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// isGLM is AC1's proxyIsGLMLike: a GLM model or a Zhipu address.
func isGLM(model, endpoint, provider string) bool {
	model, endpoint = strings.ToLower(strings.TrimSpace(model)), strings.ToLower(endpoint)
	return regexp.MustCompile(`^glm[-\d.]`).MatchString(model) ||
		regexp.MustCompile(`(?:open\.)?bigmodel\.cn|zhipu`).MatchString(endpoint) ||
		(provider == "custom" && strings.HasPrefix(model, "glm"))
}

// glmEffort is the reasoning_effort GLM takes for a setting, as AC1 sends it:
// GLM-5.3 takes low and high, earlier models high.
func glmEffort(model, reasoning string) string {
	if reasoning == "low" && glm53(model) {
		return "low"
	}
	return "high"
}

func glm53(model string) bool {
	m := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), "_", "-")
	return regexp.MustCompile(`(^|/)glm-5[.-]3($|[-:])`).MatchString(m)
}

// geminiEndpoint completes a Gemini address as AC1 does
// (proxyNormalizeGeminiEndpoint): a full :generateContent address or a
// /models/<model> one is kept; a base takes /models/<model>:generateContent.
func geminiEndpoint(base, model string) string {
	switch {
	case regexp.MustCompile(`:[a-zA-Z]+$`).MatchString(base):
		return base
	case regexp.MustCompile(`(?i)/models/[^/:]+$`).MatchString(base):
		return base + ":generateContent"
	case regexp.MustCompile(`(?i)generativelanguage\.googleapis\.com$`).MatchString(base):
		base += "/v1beta"
	}
	return base + "/models/" + model + ":generateContent"
}

func claudeEndpoint(base string) string {
	if strings.Contains(base, "/v1/") || strings.HasSuffix(base, "/messages") {
		return base
	}
	return base + "/v1/messages"
}

// openCode picks the model's own transport, as AC1 does: Claude-style for
// claude-/qwen3. (and minimax- on Go), Gemini for gemini-, the Responses API
// for gpt-/grok-/muse-spark-, chat completions otherwise.
func openCode(c Config, base string) Chatter {
	m := strings.ToLower(c.Model)
	// an address with the transport's own path takes it (AC1)
	switch path := urlPath(base); {
	case strings.HasSuffix(path, "/chat/completions"):
		return openAIChat{c: c, endpoint: base, glm: isGLM(c.Model, base, c.Provider)}
	case strings.HasSuffix(path, "/responses"):
		return responsesChat{c: c, endpoint: base}
	case strings.HasSuffix(path, "/messages"):
		return claudeChat{c: c, endpoint: base}
	case strings.Contains(path, "/models/"):
		return geminiChat{c: c, endpoint: geminiEndpoint(base, c.Model)}
	}
	switch {
	case strings.HasPrefix(m, "claude-"), strings.HasPrefix(m, "qwen3."), c.Provider == "opencode-go" && strings.HasPrefix(m, "minimax-"):
		return claudeChat{c: c, endpoint: base + "/messages"}
	case strings.HasPrefix(m, "gemini-"):
		return geminiChat{c: c, endpoint: base + "/models/" + c.Model + ":generateContent"}
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "grok-"), strings.HasPrefix(m, "muse-spark-"):
		return responsesChat{c: c, endpoint: base + "/responses"}
	}
	return openAIChat{c: c, endpoint: base + "/chat/completions", glm: isGLM(c.Model, base, c.Provider)}
}

var httpClient = &http.Client{Timeout: 15 * time.Minute}

// noRedirect answers a redirect with the redirect itself (not followed).
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// doJSON posts body and returns the status and the raw answer. Every
// credential the request carries (its auth headers, and the keys passed) is
// masked in an error text before it is shortened.
func doJSON(ctx context.Context, url string, headers map[string]string, body any, keys ...string) (int, []byte, error) {
	return doJSONClient(ctx, httpClient, url, headers, body, keys...)
}

func doJSONClient(ctx context.Context, client *http.Client, url string, headers map[string]string, body any, keys ...string) (int, []byte, error) {
	secrets := append(credentials(headers), keys...)
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, errors.New(scrub(err.Error(), secrets...)) // the address can hold a key
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, errors.New(scrub(err.Error(), secrets...))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return res.StatusCode, nil, errors.New(scrub(err.Error(), secrets...))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, raw, fmt.Errorf("provider %d: %s", res.StatusCode, truncate(scrub(string(raw), secrets...), 8000))
	}
	return res.StatusCode, raw, nil
}

type sessionKey struct{}

// WithSession gives the calls made with ctx the chat they are for, as AC1
// gives a provider its chat session id (OpenCode Go's x-opencode-session).
func WithSession(ctx context.Context, chat string) context.Context {
	return context.WithValue(ctx, sessionKey{}, chat)
}

// SessionOf is the chat WithSession gave ctx ("" = none).
func SessionOf(ctx context.Context) string {
	sid, _ := ctx.Value(sessionKey{}).(string)
	return sid
}

// openCodeGoSession is the x-opencode-session OpenCode Go gets, as AC1
// sends it: the chat's id hashed, "archive-center-auxiliary" outside a chat.
func openCodeGoSession(ctx context.Context) string {
	sid := SessionOf(ctx)
	if strings.TrimSpace(sid) == "" {
		sid = "archive-center-auxiliary"
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(sid)))
	return fmt.Sprintf("archive-center-%x", sum[:16])
}

// withProvider adds the headers a provider needs on every transport: OpenCode
// Go takes a session and a User-Agent (AC1 proxy_provider.go).
func withProvider(ctx context.Context, c Config, headers map[string]string) map[string]string {
	if c.Provider == "opencode-go" {
		headers["x-opencode-session"] = openCodeGoSession(ctx)
		headers["User-Agent"] = "ArchiveCenter/4.5.0"
	}
	return headers
}

// credentials are the secret values of a request's auth headers.
func credentials(headers map[string]string) []string {
	var out []string
	for k, v := range headers {
		switch strings.ToLower(k) {
		case "authorization":
			out = append(out, strings.TrimPrefix(v, "Bearer "), v)
		// headers that only say what the request is carry no credential; any
		// other header, a user's extra one included, may
		case "content-type", "accept", "anthropic-version", "anthropic-beta", "user-agent":
		default:
			out = append(out, v)
		}
	}
	return out
}

// scrub masks every non-empty secret in s, the longest first so one that
// contains another is masked whole.
func scrub(s string, secrets ...string) string {
	var list []string
	for _, k := range secrets {
		if k = strings.TrimSpace(k); k != "" {
			list = append(list, k)
		}
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, k := range list {
		s = strings.ReplaceAll(s, k, "***")
	}
	return s
}

// ---- OpenAI-compatible chat completions ----

type openAIChat struct {
	c        Config
	endpoint string
	glm      bool // GLM takes thinking and its own effort values
}

func (o openAIChat) Chat(ctx context.Context, prompt string) (Result, error) {
	headers := map[string]string{}
	key := o.c.Key
	switch o.c.Provider {
	case "copilot":
		tok, err := copilotToken(ctx, key)
		if err != nil {
			return Result{}, err
		}
		for k, v := range copilotHeaders {
			headers[k] = v
		}
		headers["Authorization"] = "Bearer " + tok
	case "openrouter":
		headers["HTTP-Referer"] = "https://risuai.xyz"
		headers["X-Title"] = "Archive Center"
		fallthrough
	default:
		if key != "" {
			headers["Authorization"] = "Bearer " + key
		}
	}
	body := map[string]any{"model": o.c.Model, "stream": false, "messages": []map[string]string{{"role": "user", "content": prompt}}}
	if o.c.Reasoning != "" {
		// OpenRouter and Vercel take a reasoning object, the others the effort
		switch {
		case o.glm && o.c.Provider != "openrouter" && o.c.Provider != "vercel":
			// OpenCode Go's GLM-5.3 takes the effort only (AC1)
			if !(o.c.Provider == "opencode-go" && glm53(o.c.Model)) {
				body["thinking"] = map[string]any{"type": "enabled"}
			}
			body["reasoning_effort"] = glmEffort(o.c.Model, o.c.Reasoning)
		case o.c.Provider == "openrouter" || o.c.Provider == "vercel":
			body["reasoning"] = map[string]any{"effort": o.c.Reasoning}
		default:
			body["reasoning_effort"] = o.c.Reasoning
		}
	}
	started := time.Now()
	_, raw, err := requestJSON(ctx, o.c, "chat", o.endpoint, withProvider(ctx, o.c, headers), body, key)
	if err != nil {
		return Result{}, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return Result{}, fmt.Errorf("%s: unreadable answer: %s", o.c.Provider, truncate(scrub(string(raw), o.c.Key), 8000))
	}
	m := out.Choices[0].Message
	thinking := m.ReasoningContent
	if thinking == "" {
		thinking = m.Reasoning
	}
	return Result{Content: m.Content, Thinking: thinking, PromptTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens, Seconds: time.Since(started).Seconds()}, nil
}

// ---- OpenAI Responses API (OpenCode gpt-/grok- models) ----

type responsesChat struct {
	c        Config
	endpoint string
}

func (o responsesChat) Chat(ctx context.Context, prompt string) (Result, error) {
	body := map[string]any{"model": o.c.Model, "input": prompt, "stream": false}
	if o.c.Reasoning != "" {
		body["reasoning"] = map[string]any{"effort": o.c.Reasoning}
	}
	started := time.Now()
	_, raw, err := requestJSON(ctx, o.c, "responses", o.endpoint, withProvider(ctx, o.c, map[string]string{"Authorization": "Bearer " + o.c.Key}), body, o.c.Key)
	if err != nil {
		return Result{}, err
	}
	var out struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("%s: unreadable answer: %s", o.c.Provider, truncate(scrub(string(raw), o.c.Key), 8000))
	}
	var text strings.Builder
	for _, item := range out.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
	}
	return Result{Content: text.String(), PromptTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens, Seconds: time.Since(started).Seconds()}, nil
}

// ---- Claude (Anthropic messages) ----

// claudeMaxTokens is the answer ceiling a Claude request must name; thinking
// budgets come on top.
const claudeMaxTokens = 32000

type claudeChat struct {
	c        Config
	endpoint string
}

// claudeThinking is how a Claude model takes reasoning (AC1
// proxyClaudeThinkingMode): "adaptive" from 4.6 and 5 on, "budget" for 3.7
// and 4.0-4.5, "none" otherwise.
func claudeThinking(model string) string {
	m := strings.NewReplacer(".", "-", "_", "-").Replace(strings.ToLower(model))
	match := regexp.MustCompile(`claude(?:-[a-z]+)*-(\d+)(?:-(\d{1,2})(?:-|$))?`).FindStringSubmatch(m)
	if match == nil {
		return "none"
	}
	major, _ := strconv.Atoi(match[1])
	minor := -1
	if match[2] != "" {
		minor, _ = strconv.Atoi(match[2])
	}
	switch {
	case major >= 5 || (major == 4 && minor >= 6):
		return "adaptive"
	case (major == 3 && minor == 7) || (major == 4 && minor <= 5):
		return "budget"
	}
	return "none"
}

var claudeBudget = map[string]int{"low": 2048, "medium": 8192, "high": 16384, "max": 32000}

func (o claudeChat) Chat(ctx context.Context, prompt string) (Result, error) {
	body := map[string]any{"model": o.c.Model, "max_tokens": claudeMaxTokens, "stream": false, "messages": []map[string]string{{"role": "user", "content": prompt}}}
	if o.c.Reasoning != "" && o.c.Reasoning != "off" {
		switch claudeThinking(o.c.Model) {
		case "adaptive":
			body["thinking"] = map[string]any{"type": "adaptive"}
			body["output_config"] = map[string]any{"effort": o.c.Reasoning}
		case "budget":
			b := claudeBudget[o.c.Reasoning]
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": b}
			body["max_tokens"] = claudeMaxTokens + b
		}
	}
	started := time.Now()
	_, raw, err := requestJSON(ctx, o.c, "claude", o.endpoint, withProvider(ctx, o.c, map[string]string{"x-api-key": o.c.Key, "anthropic-version": "2023-06-01"}), body, o.c.Key)
	if err != nil {
		return Result{}, err
	}
	var out struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("%s: unreadable answer: %s", o.c.Provider, truncate(scrub(string(raw), o.c.Key), 8000))
	}
	var text, thinking strings.Builder
	for _, part := range out.Content {
		switch part.Type {
		case "text":
			text.WriteString(part.Text)
		case "thinking":
			thinking.WriteString(part.Thinking)
		}
	}
	return Result{Content: text.String(), Thinking: thinking.String(), PromptTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens, Seconds: time.Since(started).Seconds()}, nil
}

// ---- Gemini (generateContent) ----

type geminiChat struct {
	c        Config
	endpoint string
	bearer   string // Vertex AI: an access token instead of the key header
}

// geminiThinking: 2.5 models take a budget, 3.x a level (AC1
// proxyGeminiThinkingMode).
func geminiThinking(model, level string) map[string]any {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gemini-2.5"):
		return map[string]any{"includeThoughts": false, "thinkingBudget": map[string]int{"low": 1024, "medium": 8192, "high": 24576}[level]}
	case regexp.MustCompile(`gemini-3(?:\D|$)`).MatchString(m):
		if level == "medium" && strings.Contains(m, "gemini-3-pro-preview") {
			level = "high" // that model takes low and high only
		}
		return map[string]any{"includeThoughts": false, "thinkingLevel": level}
	}
	return nil
}

func (o geminiChat) Chat(ctx context.Context, prompt string) (Result, error) {
	body := map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": prompt}}}}}
	if o.c.Reasoning != "" {
		if tc := geminiThinking(o.c.Model, o.c.Reasoning); tc != nil {
			body["generationConfig"] = map[string]any{"thinkingConfig": tc}
		}
	}
	headers := map[string]string{"x-goog-api-key": o.c.Key}
	if o.bearer != "" {
		headers = map[string]string{"Authorization": "Bearer " + o.bearer}
	}
	started := time.Now()
	_, raw, err := requestJSON(ctx, o.c, "gemini", o.endpoint, withProvider(ctx, o.c, headers), body, o.c.Key)
	if err != nil {
		return Result{}, err
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Usage struct {
			Prompt     int `json:"promptTokenCount"`
			Candidates int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Candidates) == 0 {
		return Result{}, fmt.Errorf("%s: unreadable answer: %s", o.c.Provider, truncate(scrub(string(raw), o.c.Key), 8000))
	}
	var text strings.Builder
	for _, part := range out.Candidates[0].Content.Parts {
		if !part.Thought {
			text.WriteString(part.Text)
		}
	}
	return Result{Content: text.String(), PromptTokens: out.Usage.Prompt, OutputTokens: out.Usage.Candidates, Seconds: time.Since(started).Seconds()}, nil
}
