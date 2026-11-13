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

// The names the settings screen shows.
var guideLevelLabels = []map[string]any{
	{"id": store.GuideOff, "label": "끔"},
	{"id": store.GuideWeak, "label": "약하게", "detail": "이어지는 점만 참고로"},
	{"id": store.GuideMid, "label": "중간", "detail": "+ 인물별 반응 후보"},
	{"id": store.GuideStrong, "label": "강하게", "detail": "+ 이번 응답 전개 계획(따름)"},
}

var inputModeLabels = []map[string]any{
	{"id": store.InputOff, "label": "끔"},
	{"id": store.InputClarify, "label": "뜻만 분명하게", "detail": "뜻만 다듬기"},
	{"id": store.InputBlend, "label": "기억 녹이기", "detail": "이어지는 기억을 짧은 구절로 섞기"},
}

// narrativeHandler answers GET /settings/narrative (the view) and POST
// {"level","chars","input","ai_own","ai":{...,"key","clear_key"}}. The
// publisher uses the critic's connection unless ai_own; a key belongs to its
// provider. Saving calls nothing.
func narrativeHandler(m *models) http.HandlerFunc {
	var save sync.Mutex
	view := func(s store.NarrativeSettings) map[string]any {
		// what the saved choice lacks, as a turn would find it (1.1)
		problem := ""
		if n, code := m.narrativeFor(); code != "" {
			problem = code
		} else if n != nil {
			problem = n.problem
		}
		return map[string]any{
			"problem": problem,
			"level":   s.Level, "levels": guideLevelLabels, "chars": s.Chars, "input": s.Input, "inputs": inputModeLabels, "ai_own": s.AIOwn,
			"ai": map[string]any{
				"provider": s.AI.Provider, "model": s.AI.Model, "url": s.AI.URL, "reasoning": s.AI.Reasoning,
				"timeout_ms": s.AI.TimeoutMS, "temperature": s.AI.Temperature, "max_tokens": s.AI.MaxTokens,
				"extra_headers_json": s.AI.ExtraHeadersJSON, "extra_body_json": s.AI.ExtraBodyJSON, "service_tier": s.AI.ServiceTier, "claude_prompt_cache": s.AI.ClaudePromptCache,
				"key_set": s.AI.Key != "", "key_last4": last4(s.AI.Key),
			},
			"providers": provider.Providers,
			// the providers that take a service tier, as on the critic's view
			"service_tier_providers": tierProviders(),
			"deadline_s":             int(narrTimeout / time.Second), // one publisher call's bound (narrative() cuts it at narrTimeout)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s, err := m.db.NarrativeSettings(m.box)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(view(s))
		case http.MethodPost:
			var req struct {
				Level *string `json:"level"`
				Chars *int    `json:"chars"`
				Input *string `json:"input"`
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
			s, err := m.db.NarrativeSettings(m.box)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			if req.Level != nil {
				s.Level = *req.Level
			}
			if req.Chars != nil {
				s.Chars = *req.Chars
			}
			if req.Input != nil {
				s.Input = *req.Input
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
			if s.AIOwn && s.AI.Provider != "" {
				if err := s.AI.Check(); err != nil {
					settingsError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			if err := m.db.SetNarrativeSettings(m.box, s); err != nil {
				switch {
				case errors.Is(err, store.ErrNarrativeChoice):
					settingsError(w, http.StatusBadRequest, "unknown_choice")
				case errors.Is(err, store.ErrNegativeBudget):
					settingsError(w, http.StatusBadRequest, "invalid_budget")
				default:
					settingsError(w, http.StatusBadRequest, err.Error())
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(view(s))
		default:
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		}
	}
}
