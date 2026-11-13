package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// prepModeLabels are the names the settings screen shows, with the
// measured recommendation.
var prepModeLabels = []map[string]any{
	{"id": store.PreprocessAIJev, "label": "AI + 판정 모델", "recommended": false},
	{"id": store.PreprocessAI, "label": "AI", "recommended": false},
	{"id": store.PreprocessJev, "label": "판정 모델", "recommended": true},
	{"id": store.PreprocessOff, "label": "사용 안 함", "recommended": false},
}

// preprocessHandler answers GET /settings/preprocess (the view) and POST
// {"mode","ai_own","ai":{"provider","model","url","reasoning",options...,
// "key","clear_key"}}. The AI uses the critic's connection unless ai_own;
// a key belongs to its provider (another provider starts without one).
// Saving calls nothing.
func preprocessHandler(m *models) http.HandlerFunc {
	var save sync.Mutex
	view := func(s store.PreprocessSettings) map[string]any {
		// what the saved mode lacks, as a turn would find it (1.1: the
		// screen warns instead of the mode silently not running): every
		// part that lacks it (AI + decision model may lack both), problem
		// the first
		problems := []string{}
		if p, code := m.prepFor(); code != "" {
			problems = append(problems, code)
		} else if p != nil {
			for _, x := range []string{p.decisionProblem, p.aiProblem} {
				if x != "" {
					problems = append(problems, x)
				}
			}
		}
		problem := ""
		if len(problems) > 0 {
			problem = problems[0]
		}
		return map[string]any{
			"problem": problem, "problems": problems,
			"mode": s.Mode, "modes": prepModeLabels, "ai_own": s.AIOwn,
			"ai": map[string]any{
				"provider": s.AI.Provider, "model": s.AI.Model, "url": s.AI.URL, "reasoning": s.AI.Reasoning,
				"timeout_ms": s.AI.TimeoutMS, "temperature": s.AI.Temperature, "max_tokens": s.AI.MaxTokens,
				"extra_headers_json": s.AI.ExtraHeadersJSON, "extra_body_json": s.AI.ExtraBodyJSON, "service_tier": s.AI.ServiceTier, "claude_prompt_cache": s.AI.ClaudePromptCache,
				"key_set": s.AI.Key != "", "key_last4": last4(s.AI.Key),
			},
			"providers": provider.Providers,
			// the providers that take a service tier, as on the critic's view
			"service_tier_providers": tierProviders(),
			"deadline_s":             int(prepTimeout / time.Second), // the whole call's bound
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s, err := m.db.PreprocessSettings(m.box)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(view(s))
		case http.MethodPost:
			var req struct {
				Mode  *string `json:"mode"`
				AIOwn *bool   `json:"ai_own"`
				AI    *struct {
					provider.Options
					Provider  string `json:"provider"`
					Model     string `json:"model"`
					URL       string `json:"url"`
					Reasoning string `json:"reasoning"`
					Key       string `json:"key"`
					ClearKey  bool   `json:"clear_key"`
				} `json:"ai"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				settingsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			save.Lock()
			defer save.Unlock()
			s, err := m.db.PreprocessSettings(m.box)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			if req.Mode != nil {
				s.Mode = *req.Mode
			}
			if req.AIOwn != nil {
				s.AIOwn = *req.AIOwn
			}
			if a := req.AI; a != nil {
				if a.Provider != s.AI.Provider {
					s.AI.Key = ""
				}
				s.AI.Provider, s.AI.Model, s.AI.URL, s.AI.Reasoning, s.AI.Options = a.Provider, strings.TrimSpace(a.Model), strings.TrimSpace(a.URL), a.Reasoning, a.Options
				if a.ClearKey {
					s.AI.Key = ""
				}
				if k := strings.TrimSpace(a.Key); k != "" {
					s.AI.Key = k
				}
			}
			// the AI's own connection, when it is used, must be one that can
			// be made (as the critic's on its screen)
			if s.AIOwn && s.AI.Provider != "" {
				if err := s.AI.Check(); err != nil {
					settingsError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			if err := m.db.SetPreprocessSettings(m.box, s); err != nil {
				if errors.Is(err, store.ErrPreprocessMode) {
					settingsError(w, http.StatusBadRequest, "unknown_mode")
					return
				}
				settingsError(w, http.StatusBadRequest, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(view(s))
		default:
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		}
	}
}
