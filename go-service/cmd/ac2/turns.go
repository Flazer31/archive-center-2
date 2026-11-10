package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

type chatTurn struct {
	Turn      int    `json:"turn"`
	User      string `json:"user"`
	Reply     string `json:"reply"`
	Memories  int    `json:"memories"`
	State     string `json:"state"` // made | waiting | failed | none
	Inherited bool   `json:"inherited"`
	Off       bool   `json:"off"`
}

func turnsHandler(pipe *pipeline.Pipeline, db *store.DB, worker *generate.Worker, st *status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChatID string `json:"chat_id"`
			Turn   int    `json:"turn"`
		}
		get := r.Method == http.MethodGet && r.URL.Path == "/chats/turns"
		if get {
			req.ChatID = r.URL.Query().Get("chat_id")
		} else if r.Method == http.MethodPost {
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Turn < 1 {
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
		var result any
		var actionErr error
		err := pipe.WithSession(key, func(s *lifecycle.Session) {
			scope := memory.Own(s)
			if get {
				offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
				if limit <= 0 || limit > memoriesPageMax {
					limit = memoriesPageMax
				}
				offset = min(max(offset, 0), len(scope))
				end := min(offset+limit, len(scope))
				counts, e := db.MemoryCounts(scope[offset:end])
				if e != nil {
					actionErr = e
					return
				}
				grants, e := db.Grants(key)
				if e != nil {
					actionErr = e
					return
				}
				rows := []chatTurn{}
				turns := s.ActiveTurns()
				for i := offset; i < end; i++ {
					t := scope[i]
					state, e := db.SourceJobState(t.Source)
					if e != nil {
						actionErr = e
						return
					}
					inherited := t.Inherited && state == "done"
					if t.Inherited && !inherited {
						state, e = db.SourceJobState(t.OwnSource())
					}
					if e != nil {
						actionErr = e
						return
					}
					label := "none"
					switch state {
					case "done":
						label = "made"
					case "failed", "unreadable":
						label = "failed"
					case "requested", "received":
						label = "waiting"
					default:
						if t.Confirmed || grants[t.OwnSource().Turn] {
							label = "waiting"
						}
					}
					if t.Off {
						label = "none"
					}
					u, a, _ := turnTexts(turns[i])
					rows = append(rows, chatTurn{Turn: i + 1, User: u, Reply: a, Memories: counts[i-offset+1], State: label, Inherited: inherited, Off: t.Off})
				}
				result = map[string]any{"chat": req.ChatID, "total": len(scope), "offset": offset, "turns": rows}
				return
			}
			if req.Turn > len(scope) {
				actionErr = errors.New("turn_not_found")
				return
			}
			t := scope[req.Turn-1]
			// Use the same inherited-memory ownership as the memory screen.
			if t.Inherited {
				state, e := db.SourceJobState(t.Source)
				if e != nil {
					actionErr = e
					return
				}
				if state == "done" {
					actionErr = store.ErrInheritedOnly
					return
				}
			}
			if strings.HasSuffix(r.URL.Path, "/remake") {
				actionErr = db.RemakeGeneration(t.OwnSource())
				if actionErr == nil && worker != nil {
					worker.Notify(key, memory.WithPast(s))
				}
			} else {
				one := append([]memory.ScopedTurn(nil), scope...)
				for i := range one {
					one[i].Off = one[i].Off || i+1 != req.Turn
				}
				items, e := db.ChatMemories(key, one)
				if e != nil {
					actionErr = e
					return
				}
				ids := []string{}
				for _, m := range items {
					ids = append(ids, m.ID)
				}
				actionErr = db.DeleteMemories(key, ids)
				if actionErr == nil && worker != nil {
					worker.Reindex(key)
				}
				// a cleared turn is made again, as any turn a deletion empties
				if actionErr == nil && !t.Off {
					if actionErr = db.RemakeGeneration(t.OwnSource()); actionErr == nil && worker != nil {
						worker.Notify(key, memory.WithPast(s))
					}
				}
			}
			result = map[string]any{"status": "ok"}
		})
		if err == nil {
			err = actionErr
		}
		if err != nil {
			code, status := "store_failed", 500
			if errors.Is(err, store.ErrInheritedOnly) {
				code, status = err.Error(), 403
			} else if err.Error() == "turn_not_found" {
				code, status = err.Error(), 404
			}
			settingsError(w, status, code)
			return
		}
		if !get && st != nil {
			st.add(event{Kind: "turn_memory_changed", Chat: req.ChatID, Turn: req.Turn, Detail: r.URL.Path})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

// remakeTurns makes the given turns (1-based, the chat's own) again, as
// /memory/turn/remake does; a turn turned off or out of range is skipped.
// It returns the turns remade.
func remakeTurns(pipe *pipeline.Pipeline, db *store.DB, worker *generate.Worker, chat string, turns []int) ([]int, error) {
	done := []int{}
	if len(turns) == 0 {
		return done, nil
	}
	key := "chat:" + chat
	var err error
	if e := pipe.WithSession(key, func(s *lifecycle.Session) {
		scope := memory.Own(s)
		for _, n := range turns {
			if n < 1 || n > len(scope) || scope[n-1].Off {
				continue
			}
			if err = db.RemakeGeneration(scope[n-1].OwnSource()); err != nil {
				return
			}
			done = append(done, n)
		}
		if len(done) > 0 && worker != nil {
			worker.Notify(key, memory.WithPast(s))
		}
	}); e != nil {
		return nil, e
	}
	return done, err
}

// remakeFailedHandler makes again every turn of a chat whose memory could
// not be made (C30, AC1's missing-turn rescan): the turns the turns list
// shows as failed (a failed or unreadable job, retries used up included),
// through the same remake as one turn. A turn turned off or read from a
// parent's memory is not one of them.
//   - POST /memory/turn/remake-failed {"chat_id","dry_run"} -> {"turns":[...]}
func remakeFailedHandler(pipe *pipeline.Pipeline, db *store.DB, worker *generate.Worker, st *status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			settingsError(w, 405, "method_not_allowed")
			return
		}
		var req struct {
			ChatID string `json:"chat_id"`
			Dry    bool   `json:"dry_run"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.ChatID == "" {
			settingsError(w, 400, "bad_request")
			return
		}
		failed := []int{}
		var readErr error
		turns := failed
		// the list and the remake run as one chat operation of the worker
		// (review 12 R10): a retry the worker saves meanwhile is not undone
		op := func() error {
			if err := pipe.WithSession("chat:"+req.ChatID, func(s *lifecycle.Session) {
				for i, t := range memory.Own(s) {
					if t.Off {
						continue
					}
					src := t.Source
					if t.Inherited {
						state, e := db.SourceJobState(src)
						if e != nil {
							readErr = e
							return
						}
						if state == "done" {
							continue // the parent's memory
						}
						src = t.OwnSource()
					}
					state, e := db.SourceJobState(src)
					if e != nil {
						readErr = e
						return
					}
					if state == "failed" || state == "unreadable" {
						failed = append(failed, i+1)
					}
				}
			}); err != nil {
				return err
			}
			if readErr != nil {
				return readErr
			}
			turns = failed
			if !req.Dry && len(failed) > 0 {
				var err error
				if turns, err = remakeTurns(pipe, db, worker, req.ChatID, failed); err != nil {
					return err
				}
			}
			return nil
		}
		var err error
		if worker != nil {
			err = worker.WithChatOperation(op)
		} else {
			err = op()
		}
		if err != nil {
			storeFailed(w, 500, err)
			return
		}
		if !req.Dry && len(turns) > 0 && st != nil {
			st.add(event{Kind: "turn_memory_changed", Chat: req.ChatID, Detail: r.URL.Path})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"turns": turns, "dry_run": req.Dry})
	}
}
