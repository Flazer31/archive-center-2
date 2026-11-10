package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func u5ptr[T any](v T) *T { return &v }

// Exercise the actual public constructor and HTTP transports, with a local
// server returning each provider's envelope. No real provider is called.
func TestU5AdvancedRequests(t *testing.T) {
	for _, tc := range []struct{ name, provider, model, path, nested, tokenKey, answer string }{
		{"chat", "custom", "plain", "/v1", "", "max_tokens", `{"choices":[{"message":{"content":"ok"}}]}`},
		{"responses", "custom", "gpt-5", "/responses", "", "max_output_tokens", `{"output":[{"content":[{"type":"output_text","text":"ok"}]}]}`},
		{"claude", "claude", "claude-4-6", "", "", "max_tokens", `{"content":[{"type":"text","text":"ok"}]}`},
		{"gemini", "gemini", "gemini-3", "", "generationConfig", "maxOutputTokens", `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`},
		{"ollama", "ollama", "plain", "", "options", "num_predict", `{"message":{"content":"ok"}}`},
		{"ollama_chat", "ollama", "plain", "/v1", "", "max_tokens", `{"choices":[{"message":{"content":"ok"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			var header http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = nil
				json.NewDecoder(r.Body).Decode(&got)
				header = r.Header.Clone()
				w.Write([]byte(tc.answer))
			}))
			defer srv.Close()
			cfg := Config{Provider: tc.provider, Model: tc.model, URL: srv.URL + tc.path, Key: "synthetic-only", Options: Options{
				Temperature: u5ptr(0.0), MaxTokens: u5ptr(1234), ExtraHeadersJSON: `{"X-U5":"sent","X-Count":7,"Authorization":"blocked"}`,
				ExtraBodyJSON: `{"top_p":0.8,"model":"blocked","messages":[],"contents":[]}`, ServiceTier: "standard", ClaudePromptCache: "ephemeral_1h",
			}}
			call := func(c Config) {
				p, err := New(c)
				if err != nil {
					t.Fatal(err)
				}
				if r, err := p.Chat(context.Background(), "instruction and source"); err != nil || r.Content != "ok" {
					t.Fatalf("call: %v %v", r, err)
				}
			}
			call(cfg)
			values := got
			if tc.nested != "" {
				values = got[tc.nested].(map[string]any)
			}
			if values["temperature"] != float64(0) || values[tc.tokenKey] != float64(1234) || got["top_p"] != 0.8 || header.Get("X-U5") != "sent" || header.Get("X-Count") != "7" {
				t.Fatalf("overrides did not reach %s", tc.name)
			}
			if got["model"] == "blocked" || header.Get("Authorization") == "blocked" {
				t.Fatal("protected AC1 field overwritten")
			}
			if SupportsServiceTier(tc.provider) {
				if got["service_tier"] != "default" {
					t.Fatal("standard tier mapping")
				}
			} else if got["service_tier"] != nil {
				t.Fatal("tier on another transport")
			}
			if tc.provider == "claude" {
				cache, _ := got["cache_control"].(map[string]any)
				if cache["ttl"] != "1h" {
					t.Fatal("Claude cache")
				}
			} else if got["cache_control"] != nil {
				t.Fatal("cache on another provider")
			}
			// Removing all overrides recovers exactly the pre-U5 request body.
			cfg.Options = Options{}
			call(cfg)
			baseline := got
			if header.Get("X-U5") != "" || got["top_p"] != nil || got["service_tier"] != nil || got["cache_control"] != nil {
				t.Fatal("cleared setting persisted")
			}
			cfg.Options = Options{ExtraHeadersJSON: "  ", ExtraBodyJSON: "{}"}
			call(cfg)
			if !reflect.DeepEqual(got, baseline) {
				t.Fatal("empty overrides changed default request")
			}
		})
	}
}

func TestU5OptionsReasoningAndMerge(t *testing.T) {
	for _, tc := range []struct {
		provider, model, path, reasoning, key string
		temp                                  bool
	}{
		{"openai", "gpt-5", "/v1", "high", "max_completion_tokens", false},
		{"openai", "o3", "/v1", "high", "max_completion_tokens", false},
		{"openrouter", "gpt-5", "/v1", "high", "max_tokens", false},
		{"custom", "gpt-5", "/responses", "high", "max_output_tokens", false},
		{"claude", "claude-sonnet-4-6", "", "high", "max_tokens", false},
		{"claude", "claude-sonnet-4-5", "", "high", "max_tokens", false},
	} {
		t.Run(tc.provider+tc.path+tc.model, func(t *testing.T) {
			var body map[string]any
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&body)
				w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"output":[],"content":[]}`))
			}))
			defer s.Close()
			c, err := New(Config{Provider: tc.provider, Model: tc.model, URL: s.URL + tc.path, Key: "fake", Reasoning: tc.reasoning, Options: Options{MaxTokens: u5ptr(48000), Temperature: u5ptr(0.4)}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Chat(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			if body[tc.key] != float64(48000) || body["temperature"] != nil {
				t.Fatal("AC1 reasoning sampling/token mapping")
			}
		})
	}
	var body map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer s.Close()
	c, _ := New(Config{Provider: "gemini", Model: "gemini-3", URL: s.URL, Key: "fake", Reasoning: "high", Options: Options{Temperature: u5ptr(0.2), ExtraBodyJSON: `{"generationConfig":{"topP":0.7}}`}})
	c.Chat(context.Background(), "test")
	g := body["generationConfig"].(map[string]any)
	if g["temperature"] != 0.2 || g["topP"] != 0.7 || g["thinkingConfig"] == nil {
		t.Fatal("nested merge lost existing fields")
	}
}

func TestU5TimeoutAndJSON(t *testing.T) {
	for _, raw := range []string{"{", "[]", "null", `{} {}`} {
		for _, headers := range []bool{true, false} {
			o := Options{}
			code := "bad_extra_body_json"
			if headers {
				o.ExtraHeadersJSON = raw
				code = "bad_extra_headers_json"
			} else {
				o.ExtraBodyJSON = raw
			}
			if err := o.Check(); err == nil || err.Error() != code {
				t.Fatalf("%s was accepted", raw)
			}
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(150 * time.Millisecond):
		}
	}))
	defer s.Close()
	for _, p := range []string{"custom", "ollama"} {
		c, err := New(Config{Provider: p, Model: "test", URL: s.URL, Options: Options{TimeoutMS: u5ptr(int64(40))}})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = c.Chat(context.Background(), "test")
		if err == nil || !strings.Contains(err.Error(), "deadline") || time.Since(start) > time.Second {
			t.Fatal("configured timeout not used")
		}
	}
}

func TestU5OptionChoicesAndNumbers(t *testing.T) {
	var body map[string]any
	var header string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = nil
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		decoder.Decode(&body)
		header = r.Header.Get("X-Number")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer s.Close()
	for _, id := range []string{"openai", "llmgateway", "vercel", "neuralwatt", "custom"} {
		for _, tier := range []string{"standard", "flex", "priority"} {
			c, err := New(Config{Provider: id, Model: "test", URL: s.URL, Key: "synthetic", Options: Options{ServiceTier: tier}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Chat(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			want := tier
			if tier == "standard" {
				want = "default"
			}
			if body["service_tier"] != want {
				t.Fatalf("%s tier %s", id, tier)
			}
		}
	}
	for _, mode := range []string{"off", "ephemeral_5m", "ephemeral_1h"} {
		c, _ := New(Config{Provider: "claude", Model: "claude-sonnet-4-6", URL: s.URL, Key: "synthetic", Options: Options{ClaudePromptCache: mode}})
		if _, err := c.Chat(context.Background(), "test"); err != nil {
			t.Fatal(err)
		}
		if mode == "off" {
			if body["cache_control"] != nil {
				t.Fatal("cache off")
			}
			continue
		}
		cache, _ := body["cache_control"].(map[string]any)
		if cache["type"] != "ephemeral" {
			t.Fatal("cache type")
		}
		if mode == "ephemeral_1h" && cache["ttl"] != "1h" {
			t.Fatal("1h cache")
		}
		if mode == "ephemeral_5m" && cache["ttl"] != nil {
			t.Fatal("5m cache")
		}
	}
	c, _ := New(Config{Provider: "custom", Model: "test", URL: s.URL, Options: Options{ExtraHeadersJSON: `{"X-Number":9007199254740993}`, ExtraBodyJSON: `{"seed":9007199254740993}`}})
	if _, err := c.Chat(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	seed, _ := body["seed"].(json.Number)
	if header != "9007199254740993" || seed.String() != header {
		t.Fatal("extra JSON number lost precision")
	}
}

func TestU5TimeoutOverridesTransportDefault(t *testing.T) {
	old := httpClient
	httpClient = &http.Client{Timeout: 2 * time.Millisecond}
	defer func() { httpClient = old }()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer s.Close()
	c, err := New(Config{Provider: "custom", Model: "test", URL: s.URL, Options: Options{TimeoutMS: u5ptr(int64(500))}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), "test"); err != nil {
		t.Fatalf("old client timeout overrode saved timeout: %v", err)
	}
	c, _ = New(Config{Provider: "custom", Model: "test", URL: s.URL})
	if _, err := c.Chat(context.Background(), "test"); err == nil {
		t.Fatal("unset timeout changed transport default")
	}
}
