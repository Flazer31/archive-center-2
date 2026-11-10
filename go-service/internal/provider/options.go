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

// Options are optional critic request overrides. Nil numbers keep the old
// transport defaults; temperature zero is an explicit sampling value.
type Options struct {
	TimeoutMS         *int64   `json:"timeout_ms"`
	Temperature       *float64 `json:"temperature"`
	MaxTokens         *int     `json:"max_tokens"`
	ExtraHeadersJSON  string   `json:"extra_headers_json"`
	ExtraBodyJSON     string   `json:"extra_body_json"`
	ServiceTier       string   `json:"service_tier"`
	ClaudePromptCache string   `json:"claude_prompt_cache"`
}

func objectJSON(raw, code string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var obj map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if !json.Valid([]byte(raw)) || decoder.Decode(&obj) != nil || obj == nil {
		return nil, errors.New(code)
	}
	return obj, nil
}

// Check validates the two JSON object editors before anything is saved.
func (o Options) Check() error {
	if _, err := objectJSON(o.ExtraHeadersJSON, "bad_extra_headers_json"); err != nil {
		return err
	}
	_, err := objectJSON(o.ExtraBodyJSON, "bad_extra_body_json")
	return err
}

func (o Options) CallTimeout() time.Duration {
	if o.TimeoutMS != nil && *o.TimeoutMS > 0 {
		return time.Duration(*o.TimeoutMS) * time.Millisecond
	}
	return 0
}

// SupportsServiceTier follows AC1's OpenAI-compatible tier providers. Gemini's
// native serviceTier is outside U5's OpenAI-compatible-only UI choice.
func SupportsServiceTier(id string) bool {
	switch id {
	case "openai", "llmgateway", "vercel", "neuralwatt", "custom":
		return true
	}
	return false
}

type timedChat struct {
	Chatter
	Options
}

func (c timedChat) Chat(ctx context.Context, prompt string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.CallTimeout())
	defer cancel()
	return c.Chatter.Chat(ctx, prompt)
}

// mergeBody follows AC1's recursive extra body merge and protected root keys.
func mergeBody(dst, src map[string]any, root bool) {
	for k, v := range src {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if root {
			switch strings.ToLower(k) {
			case "model", "messages", "contents", "system", "systeminstruction", "api_key", "apikey", "provider", "endpoint", "stream":
				continue
			}
		}
		a, aok := dst[k].(map[string]any)
		b, bok := v.(map[string]any)
		if aok && bok {
			mergeBody(a, b, false)
		} else {
			dst[k] = v
		}
	}
}

// requestJSON applies overrides at the transport boundary, including Ollama.
// Typed sampling is applied before extra body, as in AC1.
func requestJSON(ctx context.Context, c Config, transport, endpoint string, headers map[string]string, body map[string]any, keys ...string) (int, []byte, error) {
	if err := applyOptions(c, transport, headers, body); err != nil {
		return 0, nil, err
	}
	client := httpClient
	if c.CallTimeout() > 0 || c.NoRedirect {
		copy := *httpClient
		if c.CallTimeout() > 0 {
			copy.Timeout = c.CallTimeout()
		}
		if c.NoRedirect {
			copy.CheckRedirect = noRedirect
		}
		client = &copy
	}
	return doJSONClient(ctx, client, endpoint, headers, body, keys...)
}

func applyOptions(c Config, transport string, headers map[string]string, body map[string]any) error {
	if transport == "gemini" || transport == "ollama" {
		name := "generationConfig"
		if transport == "ollama" {
			name = "options"
		}
		if c.Temperature != nil || c.MaxTokens != nil {
			cfg, _ := body[name].(map[string]any)
			if cfg == nil {
				cfg = map[string]any{}
				body[name] = cfg
			}
			if c.Temperature != nil {
				cfg["temperature"] = *c.Temperature
			}
			if c.MaxTokens != nil {
				k := "maxOutputTokens"
				if transport == "ollama" {
					k = "num_predict"
				}
				cfg[k] = *c.MaxTokens
			}
		}
	} else {
		if c.Temperature != nil {
			body["temperature"] = *c.Temperature
		}
		if c.MaxTokens != nil {
			key := "max_tokens"
			if transport == "responses" {
				key = "max_output_tokens"
			}
			if transport == "chat" && openAIReasoningModel(c.Model) && c.Reasoning != "" && c.Provider != "ollama" && c.Provider != "openrouter" && c.Provider != "vercel" && c.Provider != "llmgateway" && c.Provider != "neuralwatt" {
				key = "max_completion_tokens"
				delete(body, "max_tokens")
			}
			body[key] = *c.MaxTokens
		}
		// AC1 omits temperature with Claude thinking and GPT reasoning.
		if transport == "claude" && body["thinking"] != nil || (transport == "chat" || transport == "responses") && openAIReasoningModel(c.Model) && c.Reasoning != "" && c.Provider != "ollama" {
			delete(body, "temperature")
		}
	}
	extra, err := objectJSON(c.ExtraBodyJSON, "bad_extra_body_json")
	if err != nil {
		return err
	}
	mergeBody(body, extra, true)
	h, err := objectJSON(c.ExtraHeadersJSON, "bad_extra_headers_json")
	if err != nil {
		return err
	}
	for k, v := range h {
		k = strings.TrimSpace(k)
		switch strings.ToLower(k) {
		case "", "authorization", "content-type", "accept", "x-goog-api-key", "x-api-key", "anthropic-version", "host", "content-length":
			continue
		}
		switch v.(type) {
		case string, json.Number, float64, bool:
			if s := fmt.Sprint(v); strings.TrimSpace(s) != "" {
				for name := range headers {
					if strings.EqualFold(name, k) {
						delete(headers, name)
					}
				}
				headers[http.CanonicalHeaderKey(k)] = s
			}
		}
	}
	if SupportsServiceTier(c.Provider) && c.ServiceTier != "" {
		tier := c.ServiceTier
		if tier == "standard" || tier == "auto" {
			tier = "default"
		}
		body["service_tier"] = tier
	}
	if c.Provider == "claude" {
		switch c.ClaudePromptCache {
		case "ephemeral_5m":
			body["cache_control"] = map[string]any{"type": "ephemeral"}
		case "ephemeral_1h":
			body["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
		}
	}
	return nil
}

// AC1 proxyOpenAIReasoningModel: these models take a completion-token ceiling
// with reasoning, rather than max_tokens on ordinary chat completions.
func openAIReasoningModel(model string) bool {
	return regexp.MustCompile(`(^|/)(?:gpt[-_]?5(?:$|[-_.:])|o[134](?:$|[-_:]))`).MatchString(strings.ToLower(strings.TrimSpace(model)))
}
