package main

import (
	"encoding/json"
	"net/http"

	"archivecenter2/goservice/internal/embed"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

type maintenance struct {
	db        *store.DB
	mods      *models
	worker    *generate.Worker
	pipe      *pipeline.Pipeline
	server    *observe.Server
	status    *status
	clearCold func()
}

// reindex previews the exact current document targets with GET; POST asks the
// existing worker/backlog to index them. Neither path generates memories.
func (m *maintenance) reindex(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChatID string `json:"chat_id"`
		Mode   string `json:"mode"`
	}
	switch r.Method {
	case http.MethodGet:
		req.ChatID, req.Mode = r.URL.Query().Get("chat_id"), r.URL.Query().Get("mode")
	case http.MethodPost:
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			settingsError(w, 400, "bad_request")
			return
		}
	default:
		settingsError(w, 405, "method_not_allowed")
		return
	}
	if req.Mode != "missing" && req.Mode != "all" {
		settingsError(w, 400, "bad_reindex_mode")
		return
	}
	// Serialize settings changes with target calculation and invalidation.
	m.mods.save.Lock()
	defer m.mods.save.Unlock()
	v := m.mods.query()
	if v == nil {
		settingsError(w, 400, "embedding_off")
		return
	}
	count, chats := 0, 0
	err := m.worker.Maintain(func() error {
		keys := []string{"chat:" + req.ChatID}
		if req.ChatID == "" {
			var err error
			keys, err = m.db.SessionKeys()
			if err != nil {
				return err
			}
		}
		for _, key := range keys {
			s, found, err := m.db.LoadSession(key)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			var sources []memory.Source
			for _, t := range memory.On(memory.Own(s)) {
				sources = append(sources, t.Source)
				if t.Inherited {
					sources = append(sources, t.OwnSource())
				}
			}
			jobs, err := m.db.CurrentJobs(sources)
			if err != nil {
				return err
			}
			docs, err := embed.Documents(m.db, v.VectorModel(), jobs)
			if err != nil {
				return err
			}
			hashes := make([]string, 0, len(docs))
			for _, hash := range docs {
				hashes = append(hashes, hash)
			}
			have, err := m.db.HaveEmbeddings(v.VectorModel(), hashes)
			if err != nil {
				return err
			}
			n := 0
			for _, hash := range docs {
				if req.Mode == "all" || !have[hash] {
					n++
				}
			}
			count += n
			if n == 0 {
				continue
			}
			chats++
			if r.Method == http.MethodPost {
				// Commit document links before queueing so existing status counts
				// include records which have never had an indexing attempt.
				if err := m.db.SetDocHashes(docs); err != nil {
					return err
				}
				if req.Mode == "all" {
					if err := m.db.ForgetVectors(v.VectorModel(), hashes); err != nil {
						return err
					}
				}
				m.worker.Reindex(key)
			}
		}
		return nil
	})
	if err != nil {
		storeFailed(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"count": count, "chats": chats, "model": v.VectorModel(), "mode": req.Mode, "queued": r.Method == http.MethodPost})
}

func (m *maintenance) reset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		settingsError(w, 405, "method_not_allowed")
		return
	}
	var req struct {
		Phrase string `json:"phrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		settingsError(w, 400, "bad_request")
		return
	}
	if req.Phrase != "전체 DB 초기화" {
		settingsError(w, 400, "reset_phrase_mismatch")
		return
	}
	m.mods.save.Lock()
	defer m.mods.save.Unlock()
	err := m.server.Reset(func() error {
		return m.worker.Reset(func() error {
			if err := m.pipe.Reset(func() error { return m.db.ResetData(r.Context()) }); err != nil {
				return err
			}
			m.worker.Clear()
			m.status.mu.Lock()
			m.status.events, m.status.last = nil, map[string]*injection{}
			m.status.index, m.status.query = vectorPath{}, vectorPath{}
			hud := m.status.hud
			m.status.mu.Unlock()
			if hud != nil {
				hud.clear()
			}
			if m.clearCold != nil {
				m.clearCold()
			}
			return nil
		})
	})
	if err != nil {
		storeFailed(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "settings_preserved": true})
}
