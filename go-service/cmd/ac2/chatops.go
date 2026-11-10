package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

func chatOpsHandler(pipe *pipeline.Pipeline, db *store.DB, worker *generate.Worker, st *status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		var req struct {
			ChatID string `json:"chat_id"`
			From   string `json:"from"`
			To     string `json:"to"`
			Dry    bool   `json:"dry_run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			settingsError(w, http.StatusBadRequest, "bad_request")
			return
		}
		op := strings.TrimPrefix(r.URL.Path, "/chats/")
		if op == "delete" {
			req.From = req.ChatID
		}
		if req.From == "" || (op != "delete" && (req.To == "" || req.From == req.To)) {
			settingsError(w, http.StatusBadRequest, "bad_request")
			return
		}
		var result store.ChatOpResult
		var opErr error
		locked := func() error {
			return pipe.WithSession("chat:"+req.From, func(_ *lifecycle.Session) {
				result, opErr = db.ChatOperation(op, "chat:"+req.From, "chat:"+req.To, req.Dry)
				if opErr != nil || req.Dry {
					return
				}
				for _, key := range result.Changed {
					pipe.ForgetLocked(key)
					if worker != nil {
						sources := append([]string(nil), result.ForgottenSources...)
						if s, ok, err := db.LoadSession(key); err == nil && ok {
							for _, turn := range memory.Own(s) {
								sources = append(sources, turn.OwnSource().Key())
							}
						}
						worker.Forget(key, sources)
						if key != "chat:"+req.From || op == "copy" {
							if s, ok, err := db.LoadSession(key); err == nil && ok {
								worker.Notify(key, memory.WithPast(s))
							}
						}
					}
				}
				if op != "copy" && st != nil {
					st.forgetChat(req.From)
				}
			})
		}
		var err error
		if worker != nil {
			err = worker.WithChatOperation(locked)
		} else {
			err = locked()
		}
		if err != nil || opErr != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func (s *status) forgetChat(chat string) {
	if s.hud != nil {
		s.hud.forget("chat:" + chat)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.last, chat)
	kept := s.events[:0]
	for _, e := range s.events {
		if e.Chat != chat {
			kept = append(kept, e)
		}
	}
	s.events = kept
}
