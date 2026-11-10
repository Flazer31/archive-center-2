package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// embedFake answers the embedding requests of AC1's six providers by path,
// as the services do, and records every request; an unknown path fails.
func embedFake(t *testing.T) (*httptest.Server, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var log []seen
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		r.ParseForm()
		mu.Lock()
		log = append(log, seen{path: r.URL.Path, header: r.Header.Clone(), body: body, form: r.PostForm})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch p := r.URL.Path; {
		case p == "/oauth-token":
			io.WriteString(w, `{"access_token":"vertex-access","expires_in":3600}`)
		case strings.HasSuffix(p, "/contextualizedembeddings"):
			io.WriteString(w, `{"data":[{"index":0,"data":[{"index":0,"embedding":[0.5,0.5]}]}],"usage":{"total_tokens":3}}`)
		case strings.HasSuffix(p, "/embeddings"):
			if _, many := body["input"].([]any); many { // Voyage takes the texts at once
				io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[0,1]}],"usage":{"total_tokens":4}}`)
				return
			}
			io.WriteString(w, `{"data":[{"embedding":[0.1,0.2]}],"usage":{"total_tokens":2}}`)
		case strings.HasSuffix(p, ":embedContent"):
			io.WriteString(w, `{"embedding":{"values":[0.3,0.4]}}`)
		case p == "/api/embed":
			io.WriteString(w, `{"embeddings":[[0.6,0.7]]}`)
		default:
			t.Errorf("unexpected path %s", p)
			http.Error(w, "no", 404)
		}
	}))
	t.Cleanup(s.Close)
	return s, func() []seen { mu.Lock(); defer mu.Unlock(); return append([]seen(nil), log...) }
}

func embedOne(t *testing.T, c EmbedConfig, texts ...string) Embedding {
	t.Helper()
	e, err := NewEmbedder(c)
	if err != nil {
		t.Fatalf("%s: %v", c.Provider, err)
	}
	r, err := e.Embed(context.Background(), texts, "document")
	if err != nil {
		t.Fatalf("%s: %v", c.Provider, err)
	}
	return r
}

// Each provider gets its own request, at the address AC1 completes, with
// its own credential, and its vector is read back (AC1
// turn_extraction_vector.go callSingleEmbedding).
func TestEmbeddingProvidersAsAC1(t *testing.T) {
	s, got := embedFake(t)

	if r := embedOne(t, EmbedConfig{Provider: "openai", Model: "text-embedding-3-small", URL: s.URL + "/v1", Key: "sk-o"}, "가", "나"); len(r.Vectors) != 2 || r.Vectors[1][1] != 0.2 || r.Tokens != 4 {
		t.Fatalf("openai %+v", r)
	}
	g := got()
	if len(g) != 2 || g[0].path != "/v1/embeddings" || g[0].header.Get("Authorization") != "Bearer sk-o" || g[0].body["input"] != "가" || g[1].body["input"] != "나" || g[0].body["model"] != "text-embedding-3-small" {
		t.Fatalf("openai requests %+v", g)
	}

	embedOne(t, EmbedConfig{Provider: "custom", Model: "m", URL: s.URL + "/x/v1/chat/completions"}, "가")
	if g = got()[2:]; g[0].path != "/x/v1/embeddings" || g[0].header.Get("Authorization") != "" {
		t.Fatalf("custom %+v", g[0])
	}

	if r := embedOne(t, EmbedConfig{Provider: "gemini", Model: "gemini-embedding-001", URL: s.URL + "/v1beta", Key: "g-key"}, "가"); r.Vectors[0][1] != 0.4 {
		t.Fatalf("gemini %+v", r)
	}
	g = got()[3:]
	parts, _ := g[0].body["content"].(map[string]any)["parts"].([]any)
	if g[0].path != "/v1beta/models/gemini-embedding-001:embedContent" || g[0].header.Get("x-goog-api-key") != "g-key" || len(parts) != 1 || parts[0].(map[string]any)["text"] != "가" {
		t.Fatalf("gemini %+v", g[0])
	}

	if r := embedOne(t, EmbedConfig{Provider: "ollama", Model: "nomic", URL: s.URL + "/v1"}, "가"); r.Vectors[0][0] != 0.6 {
		t.Fatalf("ollama %+v", r)
	}
	if g = got()[4:]; g[0].path != "/api/embed" || g[0].body["model"] != "nomic" || g[0].header.Get("Authorization") != "" {
		t.Fatalf("ollama %+v", g[0])
	}
	// a typed OpenAI-style address is followed
	embedOne(t, EmbedConfig{Provider: "ollama", Model: "nomic", URL: s.URL + "/v1/embeddings"}, "가")
	if g = got()[5:]; g[0].path != "/v1/embeddings" {
		t.Fatalf("ollama typed %+v", g[0])
	}

	// Voyage: the texts in one request with their input type; a context
	// model in groups at the contextualized address
	if r := embedOne(t, EmbedConfig{Provider: "voyageai", Model: "voyage-4-large", URL: s.URL + "/v1", Key: "pa-v"}, "가", "나"); len(r.Vectors) != 2 || r.Vectors[1][1] != 1 {
		t.Fatalf("voyage %+v", r)
	}
	if g = got()[6:]; len(g) != 1 || g[0].path != "/v1/embeddings" || g[0].body["input_type"] != "document" || g[0].header.Get("Authorization") != "Bearer pa-v" {
		t.Fatalf("voyage %+v", g)
	}
	ctx, err := NewEmbedder(EmbedConfig{Provider: "voyageai", Model: "voyage-context-4", URL: s.URL + "/v1", Key: "pa-v"})
	if err != nil || !ctx.Contextual() || ctx.VectorModel() != "voyage-context-4" {
		t.Fatalf("voyage context %v", err)
	}
	if _, err := ctx.EmbedGroups(context.Background(), [][]string{{"가"}}, "query"); err != nil {
		t.Fatal(err)
	}
	if g = got()[7:]; g[0].path != "/v1/contextualizedembeddings" || g[0].body["input_type"] != "query" {
		t.Fatalf("voyage context %+v", g[0])
	}
	// a typed full address is followed for each request kind (AC1)
	typed, _ := NewEmbedder(EmbedConfig{Provider: "voyageai", Model: "voyage-context-4", URL: s.URL + "/v1/contextualizedembeddings", Key: "pa-v"})
	if _, err := typed.EmbedGroups(context.Background(), [][]string{{"가"}}, "query"); err != nil {
		t.Fatal(err)
	}
	plain, _ := NewEmbedder(EmbedConfig{Provider: "voyageai", Model: "voyage-4-large", URL: s.URL + "/v1/contextualizedembeddings", Key: "pa-v"})
	plain.Embed(context.Background(), []string{"가", "나"}, "document")
	if g = got()[8:]; g[0].path != "/v1/contextualizedembeddings" || g[1].path != "/v1/embeddings" {
		t.Fatalf("typed voyage addresses %s %s", g[0].path, g[1].path)
	}
}

// Only Voyage's context models embed in groups; other providers' vectors
// are named with the provider, so one model name never shares vectors
// across providers.
func TestEmbeddingConnectionNames(t *testing.T) {
	for _, c := range []struct {
		cfg        EmbedConfig
		vector     string
		contextual bool
	}{
		{EmbedConfig{Provider: "voyageai", Model: "voyage-4-large", Key: "k"}, "voyage-4-large", false},
		{EmbedConfig{Provider: "openai", Model: "text-embedding-3-small", Key: "k"}, "openai:text-embedding-3-small", false},
		{EmbedConfig{Provider: "custom", Model: "voyage-context-4", URL: "http://x"}, "custom:voyage-context-4", false},
		{EmbedConfig{Provider: "ollama", Model: "nomic"}, "ollama:nomic", false},
	} {
		e, err := NewEmbedder(c.cfg)
		if err != nil || e.VectorModel() != c.vector || e.Contextual() != c.contextual {
			t.Fatalf("%+v: %v", c.cfg, err)
		}
	}
	for _, c := range []struct {
		cfg  EmbedConfig
		code error
	}{
		{EmbedConfig{Provider: "nope", Model: "m"}, ErrUnknownProvider},
		{EmbedConfig{Provider: "openai"}, ErrModelRequired},
		{EmbedConfig{Provider: "openai", Model: "m"}, ErrKeyRequired},
		{EmbedConfig{Provider: "custom", Model: "m"}, ErrURLRequired},
	} {
		if _, err := NewEmbedder(c.cfg); err != c.code {
			t.Fatalf("%+v: %v", c.cfg, err)
		}
	}
}

// Vertex buys an access token with the service account, fills PROJECT_ID
// and asks embedContent with the token as a bearer.
func TestVertexEmbedding(t *testing.T) {
	s, got := embedFake(t)
	old := vertexTokenURL
	vertexTokenURL = s.URL + "/oauth-token"
	defer func() { vertexTokenURL = old }()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	sa, _ := json.Marshal(map[string]string{"client_email": "bot@p.iam", "project_id": "proj-1", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	r := embedOne(t, EmbedConfig{Provider: "vertex", Model: "gemini-embedding-001", URL: s.URL + "/v1/projects/PROJECT_ID/locations/global/publishers/google/models", Key: string(sa)}, "가", "나")
	if len(r.Vectors) != 2 {
		t.Fatalf("vertex %+v", r)
	}
	g := got()
	if len(g) != 3 || g[0].path != "/oauth-token" || g[1].path != "/v1/projects/proj-1/locations/global/publishers/google/models/gemini-embedding-001:embedContent" || g[1].header.Get("Authorization") != "Bearer vertex-access" {
		t.Fatalf("vertex %+v", g)
	}
	if vertexEmbedEndpoint("https://a/models/m:generateContent", "m") != "https://a/models/m:embedContent" || geminiEmbedEndpoint("https://g/v1beta/models/m:generateContent", "m") != "https://g/v1beta/models/m:embedContent" {
		t.Fatal("embedContent address from a generateContent one")
	}
}

// A refused call names the status and never repeats the key.
func TestEmbeddingErrorHidesKey(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key "+r.Header.Get("Authorization"), 401)
	}))
	defer s.Close()
	e, _ := NewEmbedder(EmbedConfig{Provider: "openai", Model: "m", URL: s.URL, Key: "sk-live-abcdef123456"})
	_, err := e.Embed(context.Background(), []string{"가"}, "document")
	if err == nil || strings.Contains(err.Error(), "abcdef123456") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error %v", err)
	}
}

// Neuralwatt takes the OpenAI embeddings request at its /v1 address.
func TestNeuralwattEmbedding(t *testing.T) {
	s, got := embedFake(t)
	if r := embedOne(t, EmbedConfig{Provider: "neuralwatt", Model: "nw-embed", URL: s.URL + "/v1", Key: "nw-k"}, "가"); r.Vectors[0][1] != 0.2 {
		t.Fatalf("neuralwatt %+v", r)
	}
	if g := got(); g[0].path != "/v1/embeddings" || g[0].header.Get("Authorization") != "Bearer nw-k" || g[0].body["model"] != "nw-embed" {
		t.Fatalf("neuralwatt %+v", g[0])
	}
	if p, ok := FindEmbed("neuralwatt"); !ok || p.DefaultURL != "https://api.neuralwatt.com/v1" || !p.NeedsKey {
		t.Fatalf("neuralwatt info %+v", p)
	}
}
