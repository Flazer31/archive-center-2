package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Voyage calls the Voyage AI embeddings endpoint. The key comes from the
// process environment (never stored or logged here).
type Voyage struct {
	URL   string // default https://api.voyageai.com/v1/embeddings
	Model string // e.g. voyage-4-large
	Key   string
	HTTP  *http.Client
}

// Embedding is one batch's vectors, in input order, with the token count
// the provider reported.
type Embedding struct {
	Vectors [][]float32
	Tokens  int
}

// Contextual reports whether the model is embedded through the contextualized
// endpoint (Voyage's voyage-context-* models).
func Contextual(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "voyage-context-")
}

// GroupEmbedding is one request's vectors: per group, per chunk, in input
// order, with the token count the provider reported.
type GroupEmbedding struct {
	Vectors [][][]float32
	Tokens  int
}

// EmbedGroups embeds groups of chunks in one request to the contextualized
// endpoint: each chunk's vector is conditioned on the other chunks of its group.
func (v Voyage) EmbedGroups(ctx context.Context, groups [][]string, inputType string) (GroupEmbedding, error) {
	url := voyageContextEndpoint(v.URL)
	b, _ := json.Marshal(map[string]any{"model": v.Model, "inputs": groups, "input_type": inputType})
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return GroupEmbedding{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.Key)
	client := v.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	res, err := client.Do(req)
	if err != nil {
		return GroupEmbedding{}, errors.New(scrub(err.Error(), v.Key))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return GroupEmbedding{}, err
	}
	if res.StatusCode >= 300 {
		return GroupEmbedding{}, fmt.Errorf("voyage %d: %s", res.StatusCode, truncate(scrub(string(raw), v.Key), 8000))
	}
	var out struct {
		Data []struct {
			Index *int `json:"index"`
			Data  []struct {
				Index     *int      `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return GroupEmbedding{}, fmt.Errorf("voyage: %v", err)
	}
	if len(out.Data) != len(groups) {
		return GroupEmbedding{}, fmt.Errorf("voyage: %d groups for %d inputs", len(out.Data), len(groups))
	}
	vecs := make([][][]float32, len(groups))
	seenGroups := make([]bool, len(groups))
	for _, group := range out.Data {
		if group.Index == nil {
			return GroupEmbedding{}, fmt.Errorf("voyage: missing group index")
		}
		gi := *group.Index
		if gi < 0 || gi >= len(groups) || seenGroups[gi] {
			return GroupEmbedding{}, fmt.Errorf("voyage: bad group index %d", gi)
		}
		if len(group.Data) != len(groups[gi]) {
			return GroupEmbedding{}, fmt.Errorf("voyage: %d vectors for %d inputs in group %d", len(group.Data), len(groups[gi]), gi)
		}
		vecs[gi] = make([][]float32, len(groups[gi]))
		seenChunks := make([]bool, len(groups[gi]))
		for _, chunk := range group.Data {
			if chunk.Index == nil {
				return GroupEmbedding{}, fmt.Errorf("voyage: missing chunk index in group %d", gi)
			}
			ci := *chunk.Index
			if ci < 0 || ci >= len(groups[gi]) || seenChunks[ci] {
				return GroupEmbedding{}, fmt.Errorf("voyage: bad chunk index %d in group %d", ci, gi)
			}
			vecs[gi][ci] = chunk.Embedding
			seenChunks[ci] = true
		}
		seenGroups[gi] = true
	}
	return GroupEmbedding{Vectors: vecs, Tokens: out.Usage.TotalTokens}, nil
}

// Embed embeds documents (inputType "document") or queries ("query").
func (v Voyage) Embed(ctx context.Context, texts []string, inputType string) (Embedding, error) {
	url := voyageEmbedEndpoint(v.URL)
	b, _ := json.Marshal(map[string]any{"model": v.Model, "input": texts, "input_type": inputType})
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return Embedding{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.Key)
	client := v.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	res, err := client.Do(req)
	if err != nil {
		return Embedding{}, errors.New(scrub(err.Error(), v.Key))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return Embedding{}, fmt.Errorf("voyage %d: %s", res.StatusCode, truncate(scrub(string(raw), v.Key), 8000))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Embedding{}, fmt.Errorf("voyage: %v", err)
	}
	if len(out.Data) != len(texts) {
		return Embedding{}, fmt.Errorf("voyage: %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) || vecs[d.Index] != nil {
			return Embedding{}, fmt.Errorf("voyage: bad index %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return Embedding{Vectors: vecs, Tokens: out.Usage.TotalTokens}, nil
}

// truncate shortens an upstream body for an error message and keeps it on
// one line, so a body can never look like a new log record.
func truncate(s string, n int) string {
	if len(s) > n {
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return oneLine.Replace(s) // one log record: a body's lines never start records
}

var oneLine = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// voyageEmbedEndpoint and voyageContextEndpoint complete an address as AC1
// does (normalizeVoyageEmbeddingEndpoint, normalizeVoyageContextualized-
// EmbeddingEndpoint): a typed full address of the request's kind is used as
// it is, one of the other kind is turned into it, a base gets the path.
func voyageEmbedEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	switch {
	case endpoint == "":
		return "https://api.voyageai.com/v1/embeddings"
	case strings.HasSuffix(endpoint, "/contextualizedembeddings"):
		return strings.TrimSuffix(endpoint, "/contextualizedembeddings") + "/embeddings"
	case strings.HasSuffix(endpoint, "/embeddings"):
		return endpoint
	}
	return endpoint + "/embeddings"
}

func voyageContextEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	switch {
	case endpoint == "":
		return "https://api.voyageai.com/v1/contextualizedembeddings"
	case strings.HasSuffix(endpoint, "/contextualizedembeddings"):
		return endpoint
	case strings.HasSuffix(endpoint, "/embeddings"):
		return strings.TrimSuffix(endpoint, "/embeddings") + "/contextualizedembeddings"
	}
	return endpoint + "/contextualizedembeddings"
}
