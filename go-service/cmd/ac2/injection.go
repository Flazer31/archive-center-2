package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/store"
)

type injectionGeneration struct {
	hudStage
	Turn   int        `json:"turn"`
	NextAt *time.Time `json:"next_at"`
}

// injectionHandler serves the user's management view. Detailed memory is
// explicitly projected here, not on the general status/HUD paths.
func (s *status) injectionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		chat := r.URL.Query().Get("chat_id")
		s.mu.Lock()
		in := s.last[chat]
		s.mu.Unlock()
		// after a restart the last delivery is read from the database
		if in == nil && chat != "" {
			index, err := s.db.DeliveryTraceIndex("chat:"+chat, 1)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			if len(index) == 1 {
				page, err := s.db.DeliveryTraceRows("chat:"+chat, index[0].ID, 0, 1)
				if err != nil {
					storeFailed(w, http.StatusInternalServerError, err)
					return
				}
				if page != nil {
					t := page.Summary
					in = &injection{DeliveryID: page.ID, At: page.At, Chat: chat, Items: len(t.IDs), Chars: t.Chars, CharsMax: t.CharsMax, SecretChars: t.SecretChars, SecretMax: t.SecretMax,
						Ranked: t.Ranked, RecentQueryUsed: t.RecentQueryUsed, Warning: t.Warning, TotalMS: t.TotalMS, Block: page.Block, Memories: t.Memories}
					if in.Memories == nil {
						in.Memories = []store.InjectedMemory{}
					}
					if p := t.Preprocess; p != nil {
						v := *p
						v.Error = diagnostics.ErrorSummary(v.Error)
						in.Preprocess = &v
					}
					in.Narrative, in.NarrativeDetail = narrativeViews(t.Narrative)
				}
			}
		}
		var gen *injectionGeneration
		if s.hud != nil {
			v, err := s.hud.view(chat)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			turns := v["turns"].([]hudTurn)
			if len(turns) > 1 {
				prev := turns[1]
				for _, stage := range prev.Stages {
					if stage.Key == "generation" {
						gen = &injectionGeneration{hudStage: stage, Turn: prev.Turn, NextAt: prev.NextAt}
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if in == nil {
			json.NewEncoder(w).Encode(map[string]any{"chat": chat, "none": true, "generation": gen})
			return
		}
		json.NewEncoder(w).Encode(struct {
			*injection
			DeliveryID int64                  `json:"delivery_id,omitempty"`
			None       bool                   `json:"none"`
			Block      string                 `json:"block"`
			Memories   []store.InjectedMemory `json:"memories"`
			ReadError  string                 `json:"read_error,omitempty"`
			Generation *injectionGeneration   `json:"generation"`
			// the guide and the input before and after (management detail)
			Narrative *store.NarrativeRecord `json:"narrative,omitempty"`
		}{in, in.DeliveryID, false, in.Block, in.Memories, in.ReadError, gen, in.NarrativeDetail})
	}
}

// traceHandler serves a chat's latest deliveries with every candidate
// considered (GET /injection/trace?chat_id=&limit=, newest first, at most
// the kept ones): the record for checking what a request saw and chose.
func (s *status) traceHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		chat := r.URL.Query().Get("chat_id")
		if chat == "" {
			settingsError(w, http.StatusBadRequest, "chat_required")
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 1
		}
		w.Header().Set("Content-Type", "application/json")
		if rawID := r.URL.Query().Get("delivery_id"); rawID != "" {
			id, err := strconv.ParseInt(rawID, 10, 64)
			if err != nil || id <= 0 {
				settingsError(w, http.StatusBadRequest, "invalid_delivery_id")
				return
			}
			cursor, err := tracePageNumber(r, "row_cursor", 0)
			if err != nil {
				settingsError(w, http.StatusBadRequest, "invalid_row_cursor")
				return
			}
			rowLimit, err := tracePageNumber(r, "row_limit", store.DeliveryTracePageMax)
			if err != nil || rowLimit == 0 {
				settingsError(w, http.StatusBadRequest, "invalid_row_limit")
				return
			}
			page, err := s.db.DeliveryTraceRows("chat:"+chat, id, cursor, rowLimit)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			if page == nil {
				settingsError(w, http.StatusNotFound, "delivery_not_found")
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"chat": chat, "delivery": page})
			return
		}
		if r.URL.Query().Get("view") == "index" {
			index, err := s.db.DeliveryTraceIndex("chat:"+chat, limit)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"chat": chat, "deliveries": index})
			return
		}
		traces, err := s.db.DeliveryTraces("chat:"+chat, limit)
		if err != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"chat": chat, "deliveries": traces})
	}
}

func tracePageNumber(r *http.Request, key string, fallback int) (int, error) {
	if raw := r.URL.Query().Get(key); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, err
		}
		if n < 0 {
			return 0, strconv.ErrRange
		}
		return n, nil
	}
	return fallback, nil
}
