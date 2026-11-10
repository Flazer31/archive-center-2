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

// attachedChat is one stored chat of an attached past, in story order.
type attachedChat struct {
	ChatID   string `json:"chat_id"`
	Turns    int    `json:"turns"`
	Memories int    `json:"memories"`
}

// attachResult is a chat's attached past: the stored chats it reads, in
// order, and how many turns and memories in all.
type attachResult struct {
	Chats    []attachedChat `json:"chats"`
	Turns    int            `json:"turns"`
	Memories int            `json:"memories"`
	DryRun   bool           `json:"dry_run,omitempty"`
}

// attachHandler attaches stored chats as a chat's earlier story (C22 AC1's
// archived-session attach, user decision 11-06 "연결 기록 추가"; C25 AC1's
// stitch: several chats in the order chosen, the chat itself last):
//   - GET /chats/attach?chat_id= : the attached past (chats empty: none)
//   - POST /chats/attach {"chat_id","chats":[...],"dry_run"} : attach the
//     chats' read scopes as they are now, in that order (their later turns
//     are not read); replaces an earlier attachment
//   - POST /chats/attach {"chat_id","detach":true} : detach
//
// The past is read before the chat's own turns by injection and generation;
// the chat's host observation, turn identity and its own memory are not
// changed, and no memory is copied.
func attachHandler(pipe *pipeline.Pipeline, db *store.DB, worker *generate.Worker) http.HandlerFunc {
	count := func(s *lifecycle.Session) (attachResult, error) {
		out := attachResult{Chats: []attachedChat{}}
		// the past in runs of turns from one chat, in order
		for i := 0; i < len(s.Past); {
			j := i
			for j < len(s.Past) && s.Past[j].From == s.Past[i].From {
				j++
			}
			part := memory.On(memory.Past(&lifecycle.Session{Past: s.Past[i:j]}))
			n, err := db.ScopeMemoryCount(part)
			if err != nil {
				return out, err
			}
			out.Chats = append(out.Chats, attachedChat{ChatID: strings.TrimPrefix(s.Past[i].From, "chat:"), Turns: len(part), Memories: n})
			out.Turns += len(part)
			out.Memories += n
			i = j
		}
		return out, nil
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			chat := r.URL.Query().Get("chat_id")
			if chat == "" {
				settingsError(w, http.StatusBadRequest, "chat_required")
				return
			}
			var out attachResult
			var err error
			if perr := pipe.WithSession("chat:"+chat, func(s *lifecycle.Session) { out, err = count(s) }); perr != nil || err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
			return
		}
		if r.Method != http.MethodPost {
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		var req struct {
			ChatID string   `json:"chat_id"`
			Chats  []string `json:"chats"`
			Detach bool     `json:"detach"`
			Dry    bool     `json:"dry_run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatID == "" || (!req.Detach && len(req.Chats) == 0) {
			settingsError(w, http.StatusBadRequest, "bad_request")
			return
		}
		// the order chosen, each chat once
		var chats []string
		seen := map[string]bool{}
		for _, c := range req.Chats {
			if c == "" || c == req.ChatID {
				settingsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			if !seen[c] {
				seen[c] = true
				chats = append(chats, c)
			}
		}
		if req.Detach {
			chats = nil
		}
		var out attachResult
		var opErr error
		missing := false
		locked := func() error {
			return pipe.WithSession("chat:"+req.ChatID, func(s *lifecycle.Session) {
				var past []lifecycle.PastTurn
				for _, c := range chats {
					src, ok, err := db.LoadSession("chat:" + c)
					if err != nil {
						opErr = err
						return
					}
					if !ok {
						missing = true
						return
					}
					past = append(past, memory.PastOf(src)...)
				}
				past = memory.UniquePast(past)
				next := *s
				next.Past = past
				if out, opErr = count(&next); opErr != nil || req.Dry {
					out.DryRun = req.Dry
					return
				}
				if opErr = db.SetPast(s.Key, past); opErr != nil {
					return
				}
				s.Past = past
				if worker != nil {
					worker.Notify(s.Key, memory.WithPast(s))
				}
			})
		}
		var err error
		if worker != nil {
			err = worker.WithChatOperation(locked)
		} else {
			err = locked()
		}
		switch {
		case missing:
			settingsError(w, http.StatusNotFound, "chat_not_found")
		case err != nil || opErr != nil:
			storeFailed(w, http.StatusInternalServerError, err)
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
		}
	}
}
