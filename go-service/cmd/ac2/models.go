package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
)

// embedSuggestions are the models the settings screen suggests per
// embedding provider (any model may be typed, as in AC1).
var embedSuggestions = map[string][]string{
	"voyageai": {"voyage-context-4", "voyage-4-large"},
	"openai":   {"text-embedding-3-small", "text-embedding-3-large"},
	"gemini":   {"gemini-embedding-001"},
	"vertex":   {"gemini-embedding-001", "text-embedding-005", "text-multilingual-embedding-002"},
	"ollama":   {"nomic-embed-text", "mxbai-embed-large", "bge-m3", "embeddinggemma", "qwen3-embedding"},
}

// models holds the connections the settings screen sets and puts them to
// use: the critic in the generation worker (from its next job), the
// embedding in delivery and indexing (from the next request). Until a
// setting is saved, the -gen-* flags and VOYAGE_API_KEY serve. Saving never
// calls a provider.
type models struct {
	db     *store.DB
	box    *secret.Box
	worker *generate.Worker

	flagCritic  provider.Config // from -gen-*; Provider "" = none
	flagRetries int             // -gen-retries
	flagEmbed   string          // -embed-model (a Voyage model)
	envKey      string          // VOYAGE_API_KEY (Voyage only)

	save     sync.Mutex // one change at a time: read, store, put to use
	mu       sync.RWMutex
	saved    store.ModelSettings
	embedder *provider.Embedder
	problem  string // why the critic cannot run ("" = it can, or none is set)
	running  string // the critic connection in use (signature; "" = none yet)
	vectors  string // the embedding connection in use (signature; "" = none)
}

// signature names a connection by its provider, model and key (the key by a
// hash): what counts as another critic.
func signature(provider, model, key string) string {
	sum := sha256.Sum256([]byte(key))
	return strings.Join([]string{provider, model, hex.EncodeToString(sum[:8])}, "|")
}

// critic is the configuration in effect: the saved one ("off" when the
// screen saved none), else the flags.
func (m *models) critic(s store.ModelSettings) (provider.Config, string) {
	// a redirect is not followed: the key and the story go only to the
	// address set
	if s.CriticSet {
		if s.Provider == "" {
			return provider.Config{}, "off"
		}
		return provider.Config{Provider: s.Provider, Model: s.Model, URL: s.URL, Key: s.Key, Reasoning: s.Reasoning, Options: s.Options, NoRedirect: true}, "saved"
	}
	if m.flagCritic.Provider != "" {
		c := m.flagCritic
		c.NoRedirect = true
		return c, "flags"
	}
	return provider.Config{}, "none"
}

// embedding is the embedding connection in effect and where its key comes
// from: the saved provider (Voyage until one is saved, with the
// -embed-model flag), its saved model or the provider's default, and the
// saved key, else VOYAGE_API_KEY for Voyage.
func (m *models) embedding(s store.ModelSettings) (provider.EmbedConfig, string) {
	c := provider.EmbedConfig{Provider: s.EmbedProvider, Model: s.EmbedModel, URL: s.EmbedURL, Key: s.EmbedKey}
	if c.Provider == "" {
		c.Provider = "voyageai"
	}
	if c.Model == "" {
		if c.Provider == "voyageai" && m.flagEmbed != "" {
			c.Model = m.flagEmbed
		} else if p, ok := provider.FindEmbed(c.Provider); ok {
			c.Model = p.DefaultModel
		}
	}
	switch {
	case c.Key != "":
		return c, "saved"
	case c.Provider == "voyageai" && m.envKey != "":
		c.Key = m.envKey
		return c, "env"
	}
	return c, "none"
}

// apply puts settings to use: at a start (saved false) as they are; for a
// save on the settings screen (saved true) also, with another critic, the
// turns the last one could not make get their calls again, and with
// another embedding connection the missing vectors of every chat are
// filled.
func (m *models) apply(s store.ModelSettings, saved bool) {
	cfg, source := m.critic(s)
	var chat provider.Chatter
	problem := ""
	if cfg.Provider != "" {
		c, err := provider.New(cfg)
		if err != nil {
			problem = err.Error()
			log.Printf("critic: %s (%s)", problem, cfg.Provider)
		} else {
			chat = c
		}
	}
	ec, _ := m.embedding(s)
	v, err := provider.NewEmbedder(ec) // nil (no vectors) while it lacks something, e.g. a key
	if err != nil {
		v = nil
	}
	m.mu.Lock()
	m.saved, m.embedder, m.problem = s, v, problem
	prev, prevVectors := m.running, m.vectors
	if chat != nil {
		m.running = signature(cfg.Provider, cfg.Model, cfg.Key)
	}
	m.vectors = ""
	if v != nil {
		m.vectors = signature(ec.Provider+"|"+ec.URL, ec.Model, ec.Key)
	}
	changed := saved && chat != nil && prev != m.running
	refill := saved && v != nil && prevVectors != m.vectors
	m.mu.Unlock()
	if m.worker != nil {
		retries := s.Retries
		if source == "flags" {
			retries = m.flagRetries
		}
		if chat == nil {
			m.worker.Configure(nil, "", retries)
		} else {
			m.worker.Configure(chat, cfg.Model, retries)
		}
		m.worker.SetKnownMax(s.KnownChars)
		switch {
		case changed:
			m.worker.Rearm() // also runs every chat again, filling vectors
		case refill:
			if err := m.worker.Resume(); err != nil {
				log.Printf("embedding changed: resume: %v", err)
			}
		}
	}
}

// query is the embedding connection now, for delivery's query vector and
// indexing (nil = none).
func (m *models) query() *provider.Embedder {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.embedder
}

func last4(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 4 {
		return ""
	}
	return k[len(k)-4:]
}

// view is what the settings screen shows: values, which key is set (its end
// only), and the choices, all decided here.
func (m *models) view() map[string]any {
	m.mu.RLock()
	s, problem, emb := m.saved, m.problem, m.embedder
	m.mu.RUnlock()
	cfg, source := m.critic(s)
	ec, esource := m.embedding(s)
	vectorModel := ""
	if emb != nil {
		vectorModel = emb.VectorModel()
	}
	retries := s.Retries
	if source == "flags" {
		retries = m.flagRetries // as apply gives the worker
	}
	return map[string]any{
		"critic": map[string]any{
			"timeout_ms": cfg.TimeoutMS, "temperature": cfg.Temperature, "max_tokens": cfg.MaxTokens,
			"extra_headers_json": cfg.ExtraHeadersJSON, "extra_body_json": cfg.ExtraBodyJSON,
			"service_tier": cfg.ServiceTier, "claude_prompt_cache": cfg.ClaudePromptCache,
			"provider": cfg.Provider, "model": cfg.Model, "url": cfg.URL, "reasoning": cfg.Reasoning, "retries": retries, "known_chars": s.KnownChars,
			"key_set": cfg.Key != "", "key_last4": last4(cfg.Key), "source": source, "problem": problem,
		},
		"embedding": map[string]any{
			"provider": ec.Provider, "model": ec.Model, "url": ec.URL, "key_set": ec.Key != "", "key_last4": last4(ec.Key), "key_source": esource,
			"ready": emb != nil, "vector_model": vectorModel, "suggestions": embedSuggestions,
		},
		"providers":              provider.Providers,
		"embed_providers":        provider.EmbedProviders,
		"service_tier_providers": tierProviders(),
	}
}

func modelsError(w http.ResponseWriter, status int, code string) {
	settingsError(w, status, code)
}

// handler answers GET /settings/models (the view) and POST with the changes:
// {"critic": {"provider","model","url","reasoning","retries","key","clear_key"},
// "embedding": {"model","key","clear_key"}}. A key is replaced only when a new
// one is sent; "provider": "" turns generation off.
func (m *models) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
		case http.MethodPost:
			var req struct {
				Critic *struct {
					provider.Options
					Provider   string `json:"provider"`
					Model      string `json:"model"`
					URL        string `json:"url"`
					Reasoning  string `json:"reasoning"`
					Retries    *int   `json:"retries"`
					KnownChars *int   `json:"known_chars"`
					Key        string `json:"key"`
					ClearKey   bool   `json:"clear_key"`
				} `json:"critic"`
				Embedding *struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
					URL      string `json:"url"`
					Key      string `json:"key"`
					ClearKey bool   `json:"clear_key"`
				} `json:"embedding"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				modelsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			m.save.Lock()
			defer m.save.Unlock()
			m.mu.RLock()
			s := m.saved
			m.mu.RUnlock()
			if c := req.Critic; c != nil {
				// a key belongs to its provider: another provider starts
				// without one
				if c.Provider != s.Provider {
					s.Key = ""
				}
				s.CriticSet = true
				s.Provider, s.Model, s.URL, s.Reasoning = c.Provider, strings.TrimSpace(c.Model), strings.TrimSpace(c.URL), c.Reasoning
				if c.Retries != nil {
					s.Retries = *c.Retries
				}
				if c.KnownChars != nil {
					s.KnownChars = *c.KnownChars
				}
				s.Options = c.Options
				if err := s.Options.Check(); err != nil {
					modelsError(w, http.StatusBadRequest, err.Error())
					return
				}
				if c.ClearKey {
					s.Key = ""
				}
				if strings.TrimSpace(c.Key) != "" {
					s.Key = strings.TrimSpace(c.Key)
				}
				if s.Provider != "" {
					cfg, _ := m.critic(s)
					if err := cfg.Check(); err != nil {
						modelsError(w, http.StatusBadRequest, err.Error())
						return
					}
				}
			}
			if e := req.Embedding; e != nil {
				cur, _ := m.embedding(s)
				if e.Provider == "" {
					e.Provider = cur.Provider // no provider sent: the one in effect
				}
				p, ok := provider.FindEmbed(e.Provider)
				if !ok {
					modelsError(w, http.StatusBadRequest, "unknown_provider")
					return
				}
				if p.NeedsURL && strings.TrimSpace(e.URL) == "" {
					modelsError(w, http.StatusBadRequest, "url_required")
					return
				}
				// a key belongs to its provider: another provider starts
				// without one
				if e.Provider != cur.Provider {
					s.EmbedKey = ""
				}
				s.EmbedProvider, s.EmbedModel, s.EmbedURL = e.Provider, strings.TrimSpace(e.Model), strings.TrimSpace(e.URL)
				if e.ClearKey {
					s.EmbedKey = ""
				}
				if strings.TrimSpace(e.Key) != "" {
					s.EmbedKey = strings.TrimSpace(e.Key)
				}
			}
			if err := m.db.SetModelSettings(m.box, s); err != nil {
				if errors.Is(err, store.ErrBadRetries) {
					modelsError(w, http.StatusBadRequest, "bad_retries")
					return
				}
				if errors.Is(err, store.ErrBadKnownChars) {
					modelsError(w, http.StatusBadRequest, "bad_known_chars")
					return
				}
				modelsError(w, http.StatusInternalServerError, "store_failed")
				return
			}
			m.apply(s, true)
		default:
			modelsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.view())
	}
}
