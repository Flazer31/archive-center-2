package main

import (
	"encoding/json"
	"net/http"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
)

func promptHandler(db *store.DB) http.HandlerFunc {
	return promptEditor(db.CriticPrompt, db.SetCriticPrompt, memory.Instructions)
}

// publisherPromptHandler edits the narrative guide's rules (user 11-09); the
// level's items and the output shape stay the code's, so a level still asks
// for its own items.
func publisherPromptHandler(db *store.DB) http.HandlerFunc {
	return promptEditor(db.PublisherPrompt, db.SetPublisherPrompt, narrGuideHead)
}

// promptEditor is GET (the text in use, custom or the default) and POST
// {"content"} / {"reset": true} for one stored instruction override.
func promptEditor(get func() (string, error), set func(string) error, def string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
		case http.MethodPost:
			var req struct {
				Content string `json:"content"`
				Reset   bool   `json:"reset"`
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				settingsError(w, 400, "bad_request")
				return
			}
			if req.Reset {
				req.Content = ""
			}
			if set(req.Content) != nil {
				settingsError(w, 500, "store_failed")
				return
			}
		default:
			settingsError(w, 405, "method_not_allowed")
			return
		}
		text, err := get()
		if err != nil {
			storeFailed(w, 500, err)
			return
		}
		custom := text != ""
		if !custom {
			text = def
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"content": text, "custom": custom})
	}
}
