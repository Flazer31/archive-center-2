// Package provider calls generation models. Go owns the provider settings;
// the settings screen comes later (step 9).
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Ollama calls an Ollama /api/chat endpoint (local, or cloud models through
// the local Ollama).
type Ollama struct {
	Options
	URL   string // e.g. http://127.0.0.1:11434
	Model string // e.g. deepseek-v4.1-flash:cloud
	Think any    // reasoning setting passed as "think": false, true or "low"
	Key   string // optional: sent as a Bearer token (Ollama's cloud API)
	HTTP  *http.Client
}

// Result is one model answer with what the provider reported.
type Result struct {
	Content      string
	Thinking     string
	PromptTokens int
	OutputTokens int
	Seconds      float64
}

func (o Ollama) Chat(ctx context.Context, prompt string) (Result, error) {
	body := map[string]any{"model": o.Model, "stream": false, "messages": []map[string]string{{"role": "user", "content": prompt}}}
	if o.Think != nil {
		body["think"] = o.Think
	}
	headers := map[string]string{}
	if o.Key != "" {
		headers["Authorization"] = "Bearer " + o.Key
	}
	if err := applyOptions(Config{Options: o.Options}, "ollama", headers, body); err != nil {
		return Result{}, err
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", o.URL+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return Result{}, errors.New(scrub(err.Error(), o.Key)) // the address can hold a key (review 18 A-02)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	secrets := append(credentials(headers), o.Key)
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	if o.CallTimeout() > 0 {
		copy := *client
		copy.Timeout = o.CallTimeout()
		client = &copy
	}
	started := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return Result{}, errors.New(scrub(err.Error(), secrets...))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out struct {
		Error   string `json:"error"`
		Message struct {
			Content  string `json:"content"`
			Thinking string `json:"thinking"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("ollama %d: %s", res.StatusCode, truncate(scrub(string(raw), secrets...), 8000))
	}
	if out.Error != "" {
		return Result{}, fmt.Errorf("ollama %d: %s", res.StatusCode, truncate(scrub(out.Error, secrets...), 8000))
	}
	if res.StatusCode >= 300 {
		return Result{}, fmt.Errorf("ollama %d: %s", res.StatusCode, truncate(scrub(string(raw), secrets...), 8000))
	}
	return Result{Content: out.Message.Content, Thinking: out.Message.Thinking, PromptTokens: out.PromptEvalCount, OutputTokens: out.EvalCount, Seconds: time.Since(started).Seconds()}, nil
}
