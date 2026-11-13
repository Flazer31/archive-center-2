package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Decision models (1.0 E02): models that answer typed questions with
// probabilities instead of writing text, over the System One API (POST
// <base>/v1/systemone {model, state, questions}). Ollama's Clef (local
// Ollama signed in to ollama.com with clef:cloud, or ollama.com itself
// with a key) and TypeSafe's Jev.

// DecisionProvider is a decision-model service the settings screen offers.
type DecisionProvider struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	DefaultURL   string `json:"default_url"`
	DefaultModel string `json:"default_model"`
	// KeyOptional: a local Ollama signed in to ollama.com needs no key
	KeyOptional bool `json:"key_optional"`
}

// DecisionProviders are the services offered, in order (Jev and Ollama only
// for now; Ollama in the cloud, the model being large).
var DecisionProviders = []DecisionProvider{
	{ID: "ollama", Label: "Ollama (이 PC의 Ollama, ollama.com 로그인)", DefaultURL: "http://127.0.0.1:11434", DefaultModel: "clef:cloud", KeyOptional: true},
	{ID: "ollama_cloud", Label: "Ollama Cloud (ollama.com 직접, API 키)", DefaultURL: "https://ollama.com", DefaultModel: "clef"},
	{ID: "typesafe", Label: "TypeSafe Jev", DefaultURL: "https://api.typesafe.ai", DefaultModel: "jev-1.13.0"},
}

// FindDecision is the offered service by id.
func FindDecision(id string) (DecisionProvider, bool) {
	for _, p := range DecisionProviders {
		if p.ID == id {
			return p, true
		}
	}
	return DecisionProvider{}, false
}

// DecisionConfig is one decision-model connection.
type DecisionConfig struct {
	Provider string
	URL      string // "" = the provider's default
	Model    string // "" = the provider's default
	Key      string
}

// ErrDecisionNoKey: the service needs a key and none is set.
var ErrDecisionNoKey = errors.New("key_required")

// ErrDecisionUnknown: no such service.
var ErrDecisionUnknown = errors.New("unknown_provider")

// Decider asks a decision model yes/no questions.
type Decider struct {
	cfg      DecisionConfig
	endpoint string
	HTTP     *http.Client
	// Batch is the most questions in one request (System One allows 64).
	Batch int
}

// NewDecider checks the connection and fills the defaults; it calls nothing.
func NewDecider(c DecisionConfig) (*Decider, error) {
	p, ok := FindDecision(c.Provider)
	if !ok {
		return nil, ErrDecisionUnknown
	}
	if strings.TrimSpace(c.URL) == "" {
		c.URL = p.DefaultURL
	}
	if strings.TrimSpace(c.Model) == "" {
		c.Model = p.DefaultModel
	}
	if strings.TrimSpace(c.Key) == "" && !p.KeyOptional {
		return nil, ErrDecisionNoKey
	}
	base := strings.TrimRight(strings.TrimSpace(c.URL), "/")
	endpoint := base
	if !strings.HasSuffix(base, "/v1/systemone") {
		endpoint = strings.TrimSuffix(base, "/v1") + "/v1/systemone"
	}
	// a redirect is not followed: the key and the story text go only to
	// the address set; a 3xx is reported as a failure
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Decider{cfg: c, endpoint: endpoint, HTTP: client, Batch: 64}, nil
}

// Model is the model asked.
func (d *Decider) Model() string { return d.cfg.Model }

// Question is one yes/no question: Text asks it, Yes and No say what each
// answer means.
type Question struct {
	ID   string
	Text string
	Yes  string
	No   string
}

// Answers are the probabilities of yes by question id, with what the calls
// cost.
type Answers struct {
	Yes         map[string]float64
	Calls       int // requests answered
	Rejected    int // requests the service refused as too long (then split)
	InputTokens int64
	Model       string
}

// tooLong is the System One answer to a request over the input limit
// ("Request requires 66310 tokens; the maximum is 65536").
var tooLong = regexp.MustCompile(`requires \d+ tokens`)

// Ask asks every question about state, Batch at a time; a request refused
// as too long is asked again in halves (down to one question). A question
// left without an answer is absent from Yes; err reports the first failure
// other than length.
func (d *Decider) Ask(ctx context.Context, state string, qs []Question) (Answers, error) {
	out := Answers{Yes: map[string]float64{}, Model: d.cfg.Model}
	batch := d.Batch
	if batch <= 0 || batch > 64 {
		batch = 64
	}
	var first error
	var ask func(part []Question)
	ask = func(part []Question) {
		if len(part) == 0 || ctx.Err() != nil {
			return
		}
		yes, tokens, model, status, err := d.request(ctx, state, part)
		if err != nil {
			if status == http.StatusBadRequest && tooLong.MatchString(err.Error()) && len(part) > 1 {
				out.Rejected++
				ask(part[:len(part)/2])
				ask(part[len(part)/2:])
				return
			}
			if first == nil {
				first = err
			}
			return
		}
		out.Calls++
		out.InputTokens += tokens
		if model != "" {
			out.Model = model
		}
		for k, v := range yes {
			out.Yes[k] = v
		}
	}
	for b := 0; b < len(qs); b += batch {
		ask(qs[b:min(b+batch, len(qs))])
	}
	if first == nil && ctx.Err() != nil {
		first = ctx.Err()
	}
	return out, first
}

// request is one System One call. Ollama asks yes/no as "noul" (the form
// measured 11-07); TypeSafe's yes/no name is not confirmed, so it is asked
// as a two-option choice and the probability of "yes" read.
func (d *Decider) request(ctx context.Context, state string, part []Question) (map[string]float64, int64, string, int, error) {
	questions := map[string]any{}
	noul := d.cfg.Provider != "typesafe"
	for _, q := range part {
		if noul {
			questions[q.ID] = map[string]any{"type": "noul", "instructions": q.Text, "criteria": map[string]string{"true": q.Yes, "false": q.No}}
		} else {
			questions[q.ID] = map[string]any{"type": "choice", "instructions": q.Text, "criteria": map[string]string{"yes": q.Yes, "no": q.No}}
		}
	}
	headers := map[string]string{}
	if k := strings.TrimSpace(d.cfg.Key); k != "" {
		headers["Authorization"] = "Bearer " + k
	}
	status, raw, err := doJSONClient(ctx, d.HTTP, d.endpoint, headers, map[string]any{"model": d.cfg.Model, "state": state, "questions": questions}, d.cfg.Key)
	if err != nil {
		return nil, 0, "", status, err
	}
	var res struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type          string             `json:"type"`
			Noul          *float64           `json:"noul"`
			Choice        string             `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, 0, "", status, fmt.Errorf("decision answer: %w", err)
	}
	yes := map[string]float64{}
	for _, q := range part {
		a, ok := res.Answers[q.ID]
		if !ok {
			continue
		}
		switch {
		case a.Noul != nil:
			yes[q.ID] = *a.Noul
		case a.Probabilities != nil:
			if p, ok := a.Probabilities["yes"]; ok {
				yes[q.ID] = p
			}
		}
	}
	return yes, res.Usage.InputTokens, res.Model, status, nil
}
