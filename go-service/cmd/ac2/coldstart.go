package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// The cold start (AC1's session normalize): a chat's earlier turns without
// memories are made on the backend, in the background.
//
//	POST /memory/backfill {"chat_id", "max_turns" (0 = all), "order":
//	  "oldest" | "recent"} asks for the turns and answers them.
//	GET  /memory/backfill?chat_id= answers the run's progress: each asked
//	  turn made, waiting (or retrying) or failed (its calls used up).
//	POST /memory/backfill/cancel {"chat_id"} drops the turns not made yet;
//	  a turn being made now finishes.
//
// The turns of a run are kept in memory; after a restart the turns still
// asked for (stored) are the run.
type coldRun struct {
	turns   []int
	started time.Time
}

func backfillHandler(pipe *pipeline.Pipeline, worker *generate.Worker, db *store.DB) http.HandlerFunc {
	h, _ := backfillHandlers(pipe, worker, db, nil)
	return h
}

// The run's request may carry the plugin's fresh snapshot of the chat with
// its texts ("observation", as a memory request does): it is applied first,
// so a chat the backend has no record of (after a database reset, or never
// observed) is read from the host as it is now.
func backfillHandlers(pipe *pipeline.Pipeline, worker *generate.Worker, db *store.DB, server *observe.Server) (http.HandlerFunc, func()) {
	var mu sync.Mutex
	runs := map[string]*coldRun{}
	return func(w http.ResponseWriter, r *http.Request) {
		cancel := strings.HasSuffix(r.URL.Path, "/cancel")
		var req struct {
			ChatID      string         `json:"chat_id"`
			MaxTurns    int            `json:"max_turns"`
			Order       string         `json:"order"`
			Observation *observe.Batch `json:"observation"`
		}
		switch {
		case r.Method == http.MethodGet && !cancel:
			req.ChatID = r.URL.Query().Get("chat_id")
		case r.Method == http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				req.ChatID = ""
			}
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
			return
		}
		if req.ChatID == "" {
			http.Error(w, "chat_id required", http.StatusBadRequest)
			return
		}
		key := "chat:" + req.ChatID
		answer := func(v map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v)
		}
		switch {
		case cancel:
			n, err := db.UngrantAll(key)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			answer(map[string]any{"chat": key, "cancelled": n})
		case r.Method == http.MethodPost:
			resend := false
			if b := req.Observation; server != nil && b != nil && len(b.Events) > 0 {
				if b.Contract != observe.ContractVersion {
					http.Error(w, "observation contract", http.StatusBadRequest)
					return
				}
				var err error
				if _, resend, _, err = server.Apply(*b); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			var asked []int
			var berr error
			// under the observation lock: the request reads the chat as last
			// observed, and its notice cannot replace a newer one
			err := pipe.WithSession(key, func(s *lifecycle.Session) {
				asked, berr = worker.Backfill(key, memory.WithPast(s), req.MaxTurns, req.Order == "recent")
			})
			if err == nil {
				err = berr
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(asked) > 0 {
				mu.Lock()
				runs[key] = &coldRun{turns: asked, started: time.Now()}
				mu.Unlock()
			}
			// resend_text: texts came as fingerprints only and are not known
			// here; the plugin sends them again and asks once more
			answer(map[string]any{"chat": key, "turns": asked, "resend_text": resend})
		default:
			mu.Lock()
			run := runs[key]
			mu.Unlock()
			v, err := coldProgress(db, worker, key, run)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			answer(v)
		}
	}, func() { mu.Lock(); runs = map[string]*coldRun{}; mu.Unlock() }

}

// coldProgress counts a run's turns by their latest job: made, failed with
// its calls used up, or still to be made (asked, or failed and retrying).
func coldProgress(db *store.DB, worker *generate.Worker, key string, run *coldRun) (map[string]any, error) {
	grants, err := db.Grants(key)
	if err != nil {
		return nil, err
	}
	jobs, err := db.TurnJobs(key)
	if err != nil {
		return nil, err
	}
	var turns []int
	var started any
	if run != nil {
		turns, started = run.turns, run.started.UTC().Format(time.RFC3339)
	} else {
		for t := range grants {
			turns = append(turns, t)
		}
		sort.Ints(turns)
	}
	retries, _ := worker.Diagnostics()["generation_retries"].(int)
	done, failed, waiting, cancelled := 0, 0, 0, 0
	failures := []map[string]any{}
	for _, t := range turns {
		j, ok := jobs[t]
		switch {
		case ok && j.State == "done":
			done++
		case ok && j.State == "failed" && j.Attempts >= 1+retries:
			failed++
			e := ""
			if len(j.Notes) > 0 {
				e = j.Notes[len(j.Notes)-1]
			}
			failures = append(failures, map[string]any{"turn": t, "error": e})
		case grants[t] || (ok && j.State != "done"):
			waiting++ // asked, or failed and called again
		default:
			cancelled++ // dropped by a cancel before it was made
		}
	}
	cs := worker.Chat(key)
	return map[string]any{"chat": key, "total": len(turns), "done": done, "failed": failed, "waiting": waiting, "cancelled": cancelled,
		"asked": len(grants), "running": cs.Running || cs.Waiting, "started_at": started, "failures": failures}, nil
}
