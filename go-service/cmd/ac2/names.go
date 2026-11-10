package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// Preview has no session, generation or reindex dependency. Registration is
// under observe.Server's existing requireToken boundary, like /names/merge.
func namesPreviewHandler(db *store.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			settingsError(w, 405, "method_not_allowed")
			return
		}
		var req struct {
			ChatID string `json:"chat_id"`
			Keep   string `json:"keep"`
			Merge  string `json:"merge"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			settingsError(w, 400, "bad_request")
			return
		}
		if req.ChatID == "" {
			settingsError(w, 400, "chat_required")
			return
		}
		out, err := db.PreviewMergeNames("chat:"+req.ChatID, req.Keep, req.Merge)
		if err != nil {
			if errors.Is(err, store.ErrNameNotFound) {
				settingsError(w, 404, "name_not_found")
			} else {
				settingsError(w, 500, "store_failed")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}
}

func namesHandler(pipe *pipeline.Pipeline, db *store.DB, worker *generate.Worker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChatID  string `json:"chat_id"`
			ID      string `json:"id"`
			Name    string `json:"name"`
			Keep    string `json:"keep"`
			Merge   string `json:"merge"`
			MergeID int64  `json:"merge_id"`
		}
		get := r.Method == http.MethodGet && r.URL.Path == "/names"
		if get {
			req.ChatID = r.URL.Query().Get("chat_id")
		} else if r.Method == http.MethodPost {
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				settingsError(w, 400, "bad_request")
				return
			}
		} else {
			settingsError(w, 405, "method_not_allowed")
			return
		}
		if req.ChatID == "" {
			settingsError(w, 400, "chat_required")
			return
		}
		key := "chat:" + req.ChatID
		var names []store.ListedName
		var changed []string
		var actionErr error
		var err error
		if r.URL.Path == "/names/unmerge" {
			// Operation ownership is sufficient; do not hydrate the whole
			// session/name list to find this recorded operation again.
			// It runs between worker runs, as copy and move do: an index run
			// committing doc_hash on the same memories made every attempt fail
			// with 1020 (upgrade test 11-06).
			undo := func() error { changed, actionErr = db.UnmergeNames(key, req.MergeID); return nil }
			if worker != nil {
				err = worker.WithChatOperation(undo)
			} else {
				err = undo()
			}
		} else {
			run := func() error {
				return pipe.WithSession(key, func(s *lifecycle.Session) {
					names, actionErr = db.ChatNames(key, memory.Own(s))
					if actionErr != nil || get {
						return
					}
					ids := []string{req.ID}
					merge := strings.HasSuffix(r.URL.Path, "/merge")
					if merge {
						ids = []string{req.Keep, req.Merge}
					}
					for _, id := range ids {
						found := false
						for _, n := range names {
							if n.ID == id {
								found = true
								if n.Inherited {
									actionErr = store.ErrInheritedOnly
									return
								}
							}
						}
						if !found {
							actionErr = store.ErrNameNotFound
							return
						}
					}
					if merge {
						changed, actionErr = db.MergeNames(key, req.Keep, req.Merge)
					} else {
						changed, actionErr = db.EditName(key, req.ID, req.Name)
					}
				})
			}
			// A merge rewrites the same memory rows an index run is updating,
			// so it also runs between worker runs, like unmerge (review 03).
			if worker != nil && strings.HasSuffix(r.URL.Path, "/merge") {
				err = worker.WithChatOperation(run)
			} else {
				err = run()
			}
		}
		if err == nil {
			err = actionErr
		}
		if err != nil {
			code, status := "store_failed", 500
			switch {
			case errors.Is(err, store.ErrMergeNoRecord):
				code, status = "no_record", 404
			case errors.Is(err, store.ErrNameNotFound):
				code, status = err.Error(), 404
			case errors.Is(err, store.ErrEmptyName):
				code, status = err.Error(), 400
			case errors.Is(err, store.ErrInheritedOnly):
				code, status = err.Error(), 403
			}
			settingsError(w, status, code)
			return
		}
		if !get && worker != nil {
			for _, session := range changed {
				worker.Reindex(session)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if get {
			merges, err := db.NameMerges(key)
			if err != nil {
				storeFailed(w, 500, err)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"chat": req.ChatID, "names": names, "merges": merges})
		} else {
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}
}
