package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// EmbedConfig is the embedding connection (memory vectors), as the settings
// screen sets it. The providers and their addresses follow AC1
// (source/go-service/internal/httpapi/turn_extraction_vector.go).
type EmbedConfig struct {
	Provider string // an EmbedProviders ID
	Model    string
	URL      string // empty: the provider's default address
	Key      string // Vertex AI: the service account JSON
}

// EmbedProviders are AC1's six embedding providers, in its order, and
// Neuralwatt (its OpenAI-style /v1/embeddings).
var EmbedProviders = []Info{
	{ID: "openai", Label: "OpenAI", DefaultURL: "https://api.openai.com/v1", NeedsKey: true, DefaultModel: "text-embedding-3-small"},
	{ID: "gemini", Label: "Gemini", DefaultURL: "https://generativelanguage.googleapis.com/v1beta", NeedsKey: true, DefaultModel: "gemini-embedding-001"},
	{ID: "vertex", Label: "Vertex AI", DefaultURL: "https://aiplatform.googleapis.com/v1/projects/PROJECT_ID/locations/global/publishers/google/models", NeedsKey: true, DefaultModel: "gemini-embedding-001"},
	{ID: "voyageai", Label: "VoyageAI", DefaultURL: "https://api.voyageai.com/v1", NeedsKey: true, DefaultModel: "voyage-context-4"},
	{ID: "ollama", Label: "Ollama", DefaultURL: "http://127.0.0.1:11434", DefaultModel: "nomic-embed-text"},
	{ID: "neuralwatt", Label: "Neuralwatt", DefaultURL: "https://api.neuralwatt.com/v1", NeedsKey: true},
	{ID: "custom", Label: "직접 입력 (OpenAI 호환)", NeedsURL: true},
}

// FindEmbed returns the embedding provider with this id.
func FindEmbed(id string) (Info, bool) {
	for _, p := range EmbedProviders {
		if p.ID == id {
			return p, true
		}
	}
	return Info{}, false
}

// Check reports what the configuration lacks for a call (the codes of
// Config.Check).
func (c EmbedConfig) Check() error {
	p, ok := FindEmbed(c.Provider)
	switch {
	case !ok:
		return ErrUnknownProvider
	case strings.TrimSpace(c.Model) == "":
		return ErrModelRequired
	case p.NeedsURL && strings.TrimSpace(c.URL) == "":
		return ErrURLRequired
	case p.NeedsKey && strings.TrimSpace(c.Key) == "":
		return ErrKeyRequired
	}
	return nil
}

// Embedder is one embedding connection.
type Embedder struct {
	c      EmbedConfig
	base   string
	vertex *vertexChat // the Vertex access token, kept between calls
}

// NewEmbedder makes the connection the configuration describes.
func NewEmbedder(c EmbedConfig) (*Embedder, error) {
	if err := c.Check(); err != nil {
		return nil, err
	}
	p, _ := FindEmbed(c.Provider)
	base := strings.TrimRight(strings.TrimSpace(c.URL), "/")
	if base == "" {
		base = p.DefaultURL
	}
	e := &Embedder{c: c, base: base}
	if c.Provider == "vertex" {
		e.vertex = &vertexChat{c: Config{Provider: "vertex", Model: c.Model, URL: base, Key: c.Key}}
	}
	return e, nil
}

// Model is the model the connection asks for.
func (e *Embedder) Model() string { return e.c.Model }

// VectorModel names the vectors this connection makes: a Voyage model by
// its name (as stored before other providers came), another as
// provider:model, so two providers' models of one name never share vectors.
func (e *Embedder) VectorModel() string {
	if e.c.Provider == "voyageai" {
		return e.c.Model
	}
	return e.c.Provider + ":" + e.c.Model
}

// Contextual reports whether the connection embeds a turn's memories as one
// group (Voyage's voyage-context-* models).
func (e *Embedder) Contextual() bool { return e.c.Provider == "voyageai" && Contextual(e.c.Model) }

func (e *Embedder) voyage() Voyage {
	return Voyage{URL: e.base, Model: e.c.Model, Key: e.c.Key} // completed per request kind
}

// EmbedGroups embeds groups of chunks, each chunk conditioned on its group
// (a contextual connection only).
func (e *Embedder) EmbedGroups(ctx context.Context, groups [][]string, inputType string) (GroupEmbedding, error) {
	if !e.Contextual() {
		return GroupEmbedding{}, errors.New("embedding: grouped embedding needs a voyage-context model")
	}
	return e.voyage().EmbedGroups(ctx, groups, inputType)
}

// Embed embeds documents (inputType "document") or queries ("query"). Voyage
// takes the texts in one request with their input type; the others take one
// request per text, as AC1 sends them.
func (e *Embedder) Embed(ctx context.Context, texts []string, inputType string) (Embedding, error) {
	if e.c.Provider == "voyageai" {
		return e.voyage().Embed(ctx, texts, inputType)
	}
	var out Embedding
	for _, t := range texts {
		v, tokens, err := e.one(ctx, t)
		if err != nil {
			return Embedding{}, err
		}
		if len(v) == 0 {
			return Embedding{}, fmt.Errorf("%s embedding: no vector", e.c.Provider)
		}
		out.Vectors = append(out.Vectors, v)
		out.Tokens += tokens
	}
	return out, nil
}

// one embeds a text through the provider's own request (AC1 callSingleEmbedding).
func (e *Embedder) one(ctx context.Context, text string) ([]float32, int, error) {
	switch e.c.Provider {
	case "gemini":
		return e.gemini(ctx, geminiEmbedEndpoint(e.base, e.c.Model), map[string]string{"x-goog-api-key": e.c.Key}, text)
	case "vertex":
		var sa serviceAccount
		if err := json.Unmarshal([]byte(e.c.Key), &sa); err != nil || sa.ClientEmail == "" || sa.PrivateKey == "" {
			return nil, 0, errors.New("vertex: the key must be a service account JSON with client_email and private_key")
		}
		base := e.base
		if strings.Contains(base, "PROJECT_ID") {
			if sa.ProjectID == "" {
				return nil, 0, errors.New("vertex: the address has PROJECT_ID but the service account has no project_id")
			}
			base = strings.ReplaceAll(base, "PROJECT_ID", url.PathEscape(sa.ProjectID))
		}
		tok, err := e.vertex.accessToken(ctx, sa)
		if err != nil {
			return nil, 0, err
		}
		return e.gemini(ctx, vertexEmbedEndpoint(base, e.c.Model), map[string]string{"Authorization": "Bearer " + tok}, text)
	case "ollama":
		if strings.HasSuffix(urlPath(e.base), "/embeddings") {
			break // an OpenAI-style address typed in is called as it is
		}
		endpoint := strings.TrimSuffix(e.base, "/v1")
		if !strings.HasSuffix(endpoint, "/api/embed") {
			endpoint += "/api/embed"
		}
		headers := map[string]string{}
		if e.c.Key != "" {
			headers["Authorization"] = "Bearer " + e.c.Key
		}
		_, raw, err := doJSON(ctx, endpoint, headers, map[string]any{"model": e.c.Model, "input": text}, e.c.Key)
		if err != nil {
			return nil, 0, err
		}
		var res struct {
			Embedding  []float32   `json:"embedding"`
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, 0, fmt.Errorf("ollama embedding: %v", err)
		}
		if res.Embedding == nil && len(res.Embeddings) > 0 {
			res.Embedding = res.Embeddings[0]
		}
		return res.Embedding, 0, nil
	}
	// openai, neuralwatt, custom: the OpenAI embeddings request
	headers := map[string]string{}
	if e.c.Key != "" {
		headers["Authorization"] = "Bearer " + e.c.Key
	}
	_, raw, err := doJSON(ctx, openAIEmbedEndpoint(e.base), headers, map[string]any{"model": e.c.Model, "input": text}, e.c.Key)
	if err != nil {
		return nil, 0, err
	}
	var res struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, 0, fmt.Errorf("%s embedding: %v", e.c.Provider, err)
	}
	if len(res.Data) == 0 {
		return nil, 0, nil
	}
	return res.Data[0].Embedding, res.Usage.TotalTokens, nil
}

// gemini posts Gemini's embedContent request (Gemini and Vertex).
func (e *Embedder) gemini(ctx context.Context, endpoint string, headers map[string]string, text string) ([]float32, int, error) {
	_, raw, err := doJSON(ctx, endpoint, headers, map[string]any{"content": map[string]any{"parts": []map[string]any{{"text": text}}}}, e.c.Key)
	if err != nil {
		return nil, 0, err
	}
	var res struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, 0, fmt.Errorf("%s embedding: %v", e.c.Provider, err)
	}
	return res.Embedding.Values, 0, nil
}

// openAIEmbedEndpoint completes an OpenAI-style address as AC1 does
// (normalizeEmbeddingEndpoint): /embeddings is kept, a chat address takes
// its place, a base takes /embeddings.
func openAIEmbedEndpoint(base string) string {
	switch {
	case strings.HasSuffix(base, "/embeddings"):
		return base
	case strings.HasSuffix(base, "/chat/completions"):
		return strings.TrimSuffix(base, "/chat/completions") + "/embeddings"
	}
	return base + "/embeddings"
}

// geminiEmbedEndpoint completes a Gemini address for embedContent as AC1 does
// (proxyNormalizeGeminiEndpoint with embedContent).
func geminiEmbedEndpoint(base, model string) string {
	switch {
	case regexp.MustCompile(`:embedContent$`).MatchString(base):
		return base
	case regexp.MustCompile(`:[a-zA-Z]+$`).MatchString(base):
		return regexp.MustCompile(`:[a-zA-Z]+$`).ReplaceAllString(base, ":embedContent")
	case regexp.MustCompile(`(?i)/models/[^/:]+$`).MatchString(base):
		return base + ":embedContent"
	case regexp.MustCompile(`(?i)generativelanguage\.googleapis\.com$`).MatchString(base):
		base += "/v1beta"
	}
	return base + "/models/" + model + ":embedContent"
}

// vertexEmbedEndpoint completes a Vertex address for embedContent as AC1
// does (proxyNormalizeVertexEmbeddingEndpoint).
func vertexEmbedEndpoint(base, model string) string {
	switch {
	case strings.Contains(base, ":streamGenerateContent"):
		return strings.Replace(base, ":streamGenerateContent", ":embedContent", 1)
	case strings.Contains(base, ":generateContent"):
		return strings.Replace(base, ":generateContent", ":embedContent", 1)
	case strings.Contains(base, ":embedContent"):
		return base
	}
	return base + "/" + model + ":embedContent"
}
