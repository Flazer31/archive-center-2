package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
)

// decisionHandler is the decision-model connection (1.0 E02, user 11-07:
// Jev and Ollama's Clef, Ollama in the cloud):
//   - GET /settings/decision : the connection (key set and its last 4 only)
//     and the services offered
//   - POST /settings/decision {"provider","url","model","key","clear_key"} :
//     saves it (a key belongs to its provider: another provider starts
//     without one; "provider": "" turns it off). Saving calls nothing.
//   - POST /settings/decision/test : asks the saved connection one question
//     (one paid call) and tells whether it answered
//
// The connection is not used by delivery yet (preprocessing modes, plan
// step 3).
func decisionHandler(db *store.DB, box *secret.Box) http.HandlerFunc {
	var save sync.Mutex
	view := func(s store.DecisionSettings) map[string]any {
		url, model := s.URL, s.Model
		if p, ok := provider.FindDecision(s.Provider); ok {
			if url == "" {
				url = p.DefaultURL
			}
			if model == "" {
				model = p.DefaultModel
			}
		}
		// the local Ollama is not offered (user 11-08 "전처리에서 ollama를 직접
		// PC에서 쓰는건 없어도 괜찮을 거 같고"); a connection already saved
		// with it stays shown, so a save never drops it unseen
		offered := []provider.DecisionProvider{}
		for _, p := range provider.DecisionProviders {
			if p.ID != "ollama" || s.Provider == "ollama" {
				offered = append(offered, p)
			}
		}
		return map[string]any{"provider": s.Provider, "url": url, "model": model, "key_set": s.Key != "", "key_last4": last4(s.Key), "providers": offered}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		test := strings.HasSuffix(r.URL.Path, "/test")
		switch {
		case r.Method == http.MethodGet && !test:
			s, err := db.DecisionSettings(box)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(view(s))
		case r.Method == http.MethodPost && test:
			s, err := db.DecisionSettings(box)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			if s.Provider == "" {
				settingsError(w, http.StatusBadRequest, "decision_off")
				return
			}
			d, err := provider.NewDecider(provider.DecisionConfig{Provider: s.Provider, URL: s.URL, Model: s.Model, Key: s.Key})
			if err != nil {
				code := "bad_request"
				switch {
				case errors.Is(err, provider.ErrDecisionNoKey):
					code = "key_required"
				case errors.Is(err, provider.ErrDecisionUnknown):
					code = "unknown_provider"
				}
				settingsError(w, http.StatusBadRequest, code)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
			defer cancel()
			start := time.Now()
			a, err := d.Ask(ctx, "연결 시험: 리나가 약국에 들어왔다.", []provider.Question{{ID: "test", Text: "리나가 약국에 있는가?", Yes: "있다", No: "없다"}})
			out := map[string]any{"ok": err == nil && len(a.Yes) == 1, "ms": time.Since(start).Milliseconds(), "model": a.Model, "input_tokens": a.InputTokens}
			switch {
			case err != nil:
				out["error"] = err.Error() // keys are masked by the provider package
			case len(a.Yes) == 0:
				out["error"] = "no_answer" // a reply without the test question's answer (review 16 B-01)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost:
			var req struct {
				Provider string `json:"provider"`
				URL      string `json:"url"`
				Model    string `json:"model"`
				Key      string `json:"key"`
				ClearKey bool   `json:"clear_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				settingsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			if req.Provider != "" {
				if _, ok := provider.FindDecision(req.Provider); !ok {
					settingsError(w, http.StatusBadRequest, "unknown_provider")
					return
				}
			}
			save.Lock()
			defer save.Unlock()
			s, err := db.DecisionSettings(box)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			if req.Provider != s.Provider {
				s.Key = ""
			}
			s.Provider, s.URL, s.Model = req.Provider, strings.TrimSpace(req.URL), strings.TrimSpace(req.Model)
			if req.ClearKey {
				s.Key = ""
			}
			if k := strings.TrimSpace(req.Key); k != "" {
				s.Key = k
			}
			if s.Provider != "" {
				if _, err := provider.NewDecider(provider.DecisionConfig{Provider: s.Provider, URL: s.URL, Model: s.Model, Key: s.Key}); errors.Is(err, provider.ErrDecisionNoKey) {
					settingsError(w, http.StatusBadRequest, "key_required")
					return
				}
			}
			if err := db.SetDecisionSettings(box, s); err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(view(s))
		default:
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		}
	}
}
