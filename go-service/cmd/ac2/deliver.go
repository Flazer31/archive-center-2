package main

import (
	"archivecenter2/goservice/internal/body"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// injectHandler answers the memory a request gets: POST {"chat_id",
// "input", "payload": [{"len","h"}]} -> the memory block and what was
// chosen. Without a query vector (no key, or the provider failed) the order
// is by recency and "warning" says why.
// query gives the current query vector provider (nil = none); the settings
// screen may change it between requests.
// st, when set, keeps the result for the status screen.
func injectHandler(server *observe.Server, pipe *pipeline.Pipeline, db *store.DB, query func() *provider.Embedder, st *status) http.HandlerFunc {
	transportBuilder := &memoryTransportBuilder{}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		started := time.Now()
		// the whole answer shares one deadline (review 20 B-03): the plugin
		// waits 100 s, so the query, a preprocessing mode and narrative
		// support all end by narrDeadline from the start; what has not
		// finished by then is left out and the memory goes as chosen so far
		within, cancel := context.WithDeadline(r.Context(), started.Add(narrDeadline))
		defer cancel()
		var req struct {
			ChatID            string                  `json:"chat_id"`
			Input             string                  `json:"input"`
			Payload           []store.TextPrint       `json:"payload"`
			Observation       *observe.Batch          `json:"observation"`
			PlacementMessages []memory.PayloadMessage `json:"placement_messages"`
			Type              string                  `json:"type"` // the host's request type
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatID == "" {
			http.Error(w, "chat_id required", http.StatusBadRequest)
			return
		}
		// this request's observation is applied first (it also goes through
		// /observe; applying it again is safe), so the choice reads the chat
		// as the request leaves it
		applyWarning := ""
		resend := false
		if b := req.Observation; b != nil && len(b.Events) > 0 {
			var err error
			if b.Contract != observe.ContractVersion {
				applyWarning = "observation_contract"
			} else if _, resend, _, err = server.Apply(*b); err != nil {
				applyWarning = "observation_not_applied"
				log.Printf("memory delivery: apply observation: %v", err)
			}
		}
		// as AC1: only the main model's request gets memory; auxiliary
		// requests (otherAx, submodel, translate, ...) go on unchanged and
		// leave the chat's last injection as it was
		if req.Type != "" && req.Type != "model" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"skipped": "request_type", "observation": applyWarning, "resend_text": resend})
			return
		}
		// the chat as last observed, read under the observation lock
		var scope []memory.ScopedTurn
		var locator *store.DeliveryLocator
		if err := pipe.WithSession("chat:"+req.ChatID, func(s *lifecycle.Session) {
			scope = memory.ScopeOf(s)
			turns := s.ActiveTurns()
			if len(turns) > 0 {
				last := turns[len(turns)-1]
				locator = &store.DeliveryLocator{TurnID: last.ID, Turn: len(turns), SourceKey: memory.SourceOf(s.Key, last).Key()}
			}
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		chars, secret, err := db.MemoryBudgets()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// the input is the host's own row of this turn: a preset can put more
		// user-role prompt parts after it, so the request's last user message
		// is not always what the user wrote (2026-10-29: a 545-char preset part
		// was searched instead of the 28-char input)
		input := req.Input
		if t := currentInput(scope); t != "" {
			input = t
		}
		dr := store.DeliveryRequest{Input: input, Payload: req.Payload, Chars: chars, Secret: secret}
		warning := ""
		var queryErr error
		embedMS := int64(0)
		q := query()
		switch {
		case q == nil:
			warning = "query_vector_off"
		case strings.TrimSpace(input) == "":
			warning = "no_input_text"
		default:
			t := time.Now()
			v, recent, err, recentErr := queryVectors(within, q, input, previousTurnText(scope))
			embedMS = time.Since(t).Milliseconds()
			if err != nil {
				warning, queryErr = "query_vector_failed", err
				log.Printf("memory delivery: query vector: %v", err)
			} else {
				dr.Query, dr.Model = v, q.VectorModel()
				dr.RecentQuery = recent
				if recentErr != nil {
					warning = "recent_query_vector_failed"
					log.Printf("memory delivery: recent turn vector: %v", recentErr)
				}
			}
		}
		// body tracking (1.0 E09/E10): its own section and budget; a failed
		// read is logged and the memory is delivered without it
		if lines, err := bodyLines(db, "chat:"+req.ChatID, scope); err != nil {
			log.Printf("memory delivery: body state: %v", err)
		} else {
			dr.Body = lines
		}
		d, err := db.Deliver(scope, dr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// a preprocessing mode (E01~E03) chooses among the ranked records;
		// any failure keeps this basic delivery
		var prep *store.PreprocessRecord
		if st != nil {
			p, problem := st.mods.prepFor()
			switch {
			case p != nil:
				d, prep = preprocess(within, p, db, scope, dr, d, input)
			case problem != "":
				prep = &store.PreprocessRecord{Ran: "go", Fallback: problem}
			}
		}
		// narrative support (E03, step 4): a guide beside the memory and the
		// input improved for the main model only; any failure sends without
		var guide narrGuide
		var rewrite *inputRewrite
		var narr *store.NarrativeRecord
		if st != nil {
			n, problem := st.mods.narrativeFor()
			switch {
			case n != nil:
				guide, rewrite, narr = narrative(within, n, d, scope, input, started)
			case problem != "":
				narr = &store.NarrativeRecord{Guide: problem}
			}
		}
		if d.RecentQueryError != nil {
			warning = "recent_query_vector_failed"
		}
		// what a preprocessing mode or narrative support could not do goes to
		// the log with its whole error (credentials masked)
		if prep != nil && (prep.Fallback != "" || prep.Error != "") {
			log.Printf("memory delivery: preprocess %s ran %s fallback %q: %s", prep.Mode, prep.Ran, prep.Fallback, diagnostics.ErrorSummary(prep.Error))
		}
		if narr != nil && (narr.GuideError != "" || narr.InputError != "" || (narr.Guide != "" && narr.Guide != "made" && narr.Guide != "nothing_kept" && narr.Guide != "nothing_to_guide")) {
			log.Printf("memory delivery: narrative guide %q input %q: %s %s", narr.Guide, narr.InputResult, diagnostics.ErrorSummary(narr.GuideError), diagnostics.ErrorSummary(narr.InputError))
		}
		mode, marker, err := db.MemoryPlacement()
		if err != nil {
			storeFailed(w, 500, err)
			return
		}
		transportMode, transportErr := db.MemoryTransport()
		transport := transportBuilder.selected(transportMode, d, transportErr)
		total := time.Since(started).Milliseconds()
		placement := memory.ResolvePlacement(req.PlacementMessages, mode, marker)
		d.Preprocess = prep
		d.Narrative = narr
		var in *injection
		if st != nil {
			in = st.injected(req.ChatID, d, warning, chars, secret, total, queryErr, dr.Query != nil, scope...)
		}
		// the delivery is kept in the database too, with every candidate
		// considered, so it can be checked after a restart (10-29); a failed
		// write is logged and never fails the delivery
		sum := store.DeliverySummary{Locator: locator, Input: input, Window: d.Window, WindowFrom: d.WindowFrom, Ranked: d.Ranked, RecentQueryUsed: &d.RecentQueryUsed, Candidates: d.Candidates,
			Chars: d.Chars, CharsMax: chars, SecretChars: d.SecretChars, SecretMax: secret, IDs: d.IDs, SecretIDs: []string{}, Warning: warning, QueryError: rawDetail(queryErr), TotalMS: total, Preprocess: prep, Narrative: narr}
		for _, id := range d.IDs {
			if d.SecretIDs[id] {
				sum.SecretIDs = append(sum.SecretIDs, id)
			}
		}
		if in != nil {
			sum.Memories = in.Memories
		}
		// one line per memory request in the log (user 11-08: the log must
		// show what happened)
		log.Printf("memory delivery: chat %s: %d memories, %d/%d chars, secret %d/%d, ranked %s, warning %q, %d ms", req.ChatID, len(d.IDs), d.Chars, chars, d.SecretChars, secret, d.Ranked, warning, total)
		if queryErr != nil {
			log.Printf("memory delivery: chat %s: query: %s", req.ChatID, diagnostics.ErrorSummary(queryErr.Error()))
		}
		deliveryID, traceErr := db.SaveDeliveryTrace("chat:"+req.ChatID, time.Now(), sum, d.Block, d.Trace)
		if traceErr != nil {
			log.Printf("memory delivery: keep trace: %v", traceErr)
		}
		if in != nil {
			// Publish a copy: a concurrent management reader may hold in.
			updated := *in
			updated.DeliveryID = deliveryID
			st.mu.Lock()
			if st.last[req.ChatID] == in {
				st.last[req.ChatID] = &updated
			}
			st.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			store.Delivery
			DeliveryID  int64                   `json:"delivery_id,omitempty"`
			Warning     string                  `json:"warning,omitempty"`
			Observation string                  `json:"observation,omitempty"`
			ResendText  bool                    `json:"resend_text,omitempty"` // as /observe: texts sent as fingerprints only are not known
			EmbedMS     int64                   `json:"embed_ms"`
			TotalMS     int64                   `json:"total_ms"`
			Placement   memory.Placement        `json:"placement"`
			Transport   *memoryTransport        `json:"transport,omitempty"`
			Anchor      string                  `json:"anchor"` // the marker the plugin looks for in the next request
			Preprocess  *store.PreprocessRecord `json:"preprocess,omitempty"`
			Guide       *narrGuide              `json:"guide,omitempty"`
			Input       *inputRewrite           `json:"input_rewrite,omitempty"`
		}{d, deliveryID, warning, applyWarning, resend, embedMS, total, placement, transport, marker, prep, guideOrNil(guide), rewrite})
	}
}

// currentInput is the text of the host's user rows of the turn the request
// answers (the last turn, with no reply yet); "" when there is none or the
// texts were not captured.
func currentInput(scope []memory.ScopedTurn) string {
	if len(scope) == 0 || len(scope[len(scope)-1].Source.Responses) > 0 {
		return ""
	}
	var parts []string
	for _, m := range scope[len(scope)-1].Source.Members {
		if strings.TrimSpace(m.Data) != "" {
			parts = append(parts, m.Data)
		}
	}
	return strings.Join(parts, "\n")
}

// The request's scope already resolves branch ownership and visible revisions.
// Read just its last completed turn, before the current unanswered input.
func previousTurnText(scope []memory.ScopedTurn) string {
	for i := len(scope) - 1; i >= 0; i-- {
		if len(scope[i].Source.Responses) > 0 {
			return memory.TurnText(scope[i].Source)
		}
	}
	return ""
}

// Queries remain separate texts (separate singleton groups for contextual
// embedding). If the optional turn fails, retain a valid input vector. A
// whole-batch failure gets one input-only call through the established path.
func queryVectors(ctx context.Context, v *provider.Embedder, input, recent string) ([]float32, []float32, error, error) {
	if recent == "" {
		q, err := queryVector(ctx, v, input)
		return q, nil, err, nil
	}
	// Voyage alone keeps its model name unprefixed and batches in Embed.
	// Other providers retain their existing one-text request path.
	if v.VectorModel() != v.Model() {
		q, err := queryVector(ctx, v, input)
		if err != nil {
			return nil, nil, err, nil
		}
		r, recentErr := queryVector(ctx, v, recent)
		return q, r, nil, recentErr
	}
	batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var r provider.Embedding
	var err error
	if v.Contextual() {
		var groups provider.GroupEmbedding
		groups, err = v.EmbedGroups(batchCtx, [][]string{{input}, {recent}}, "query")
		for _, group := range groups.Vectors {
			r.Vectors = append(r.Vectors, group[0])
		}
	} else {
		r, err = v.Embed(batchCtx, []string{input, recent}, "query")
	}
	cancel()
	if len(r.Vectors) > 0 && len(r.Vectors[0]) > 0 {
		if err == nil && len(r.Vectors) > 1 && len(r.Vectors[1]) > 0 {
			return r.Vectors[0], r.Vectors[1], nil, nil
		}
		if err == nil {
			err = errors.New("embedding: no recent turn vector")
		}
		return r.Vectors[0], nil, nil, err
	}
	if err == nil {
		err = errors.New("embedding: no input vector in batch")
	}
	q, inputErr := queryVector(ctx, v, input)
	return q, nil, inputErr, err
}

func queryVector(ctx context.Context, v *provider.Embedder, input string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if v.Contextual() {
		r, err := v.EmbedGroups(ctx, [][]string{{input}}, "query")
		if err != nil {
			return nil, err
		}
		return r.Vectors[0][0], nil
	}
	r, err := v.Embed(ctx, []string{input}, "query")
	if err != nil {
		return nil, err
	}
	return r.Vectors[0], nil
}

// storeFailed answers "store_failed" and puts the database error itself in
// the log (user 11-08: the log must show why), credentials masked.
func storeFailed(w http.ResponseWriter, status int, err error) {
	if err != nil {
		log.Printf("store_failed: %s", diagnostics.ErrorSummary(err.Error()))
	}
	settingsError(w, status, "store_failed")
}

// settingsError answers a settings request with a stable code the settings
// screen shows.
func settingsError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"status": "error", "code": code})
}

// settingsHandler reads (GET) or sets (POST {"memory_chars","secret_chars"})
// the memory ceilings shown on the settings screen.
func settingsHandler(db *store.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
		case http.MethodPost:
			var req struct {
				MemoryChars   *int    `json:"memory_chars"`
				SecretChars   *int    `json:"secret_chars"`
				Placement     *string `json:"placement"`
				Anchor        *string `json:"anchor"`
				TransportMode *string `json:"transport_mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				settingsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			chars, secret, err := db.MemoryBudgets()
			var mode, marker string
			if err == nil {
				mode, marker, err = db.MemoryPlacement()
			}
			if err == nil {
				if req.Placement != nil {
					mode = *req.Placement
				}
				if req.Anchor != nil {
					marker = *req.Anchor
				}
				if req.MemoryChars != nil {
					chars = *req.MemoryChars
				}
				if req.SecretChars != nil {
					secret = *req.SecretChars
				}
				if req.TransportMode == nil {
					err = db.SetMemorySettings(chars, secret, mode, marker)
				} else {
					err = db.SetMemorySettings(chars, secret, mode, marker, *req.TransportMode)
				}
			}
			if errors.Is(err, store.ErrNegativeBudget) {
				settingsError(w, http.StatusBadRequest, "invalid_budget")
				return
			}
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
		default:
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		chars, secret, err := db.MemoryBudgets()
		if err != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		mode, marker, err := db.MemoryPlacement()
		if err != nil {
			storeFailed(w, 500, err)
			return
		}
		transportMode, err := db.MemoryTransport()
		if err != nil {
			storeFailed(w, 500, err)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"transport_mode": transportMode, "transport_modes": []string{"text", "google_pdf", "llm_gateway_pdf", "provider_manager_pdf"}, "memory_chars": chars, "secret_chars": secret, "placement": mode, "anchor": marker, "placements": memory.PlacementModes})
	}
}

// bodyLines are the body section of a chat's delivery: every tracked
// person's body on the story's latest day, with names for ids.
func bodyLines(db *store.DB, chatKey string, scope []memory.ScopedTurn) ([]string, error) {
	states, err := db.BodyStates(chatKey, scope)
	if err != nil || len(states) == 0 {
		return nil, err
	}
	var refs []string
	for _, st := range states {
		refs = append(refs, st.Who)
		if p := st.Pregnancy; p != nil {
			refs = append(refs, p.Candidates...)
		}
	}
	names, err := db.NamesByID(refs)
	if err != nil {
		return nil, err
	}
	return body.Lines(states, func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}), nil
}

// guideOrNil is the guide as sent, each part a titled message.
func guideOrNil(g narrGuide) *narrGuide {
	if g.AfterMemory == "" && g.BeforeReply == "" {
		return nil
	}
	return &narrGuide{AfterMemory: store.GuideMessage(g.AfterMemory), BeforeReply: store.GuideMessage(g.BeforeReply)}
}
