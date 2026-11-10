package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// bodyPerson is a tracked person on the body screen.
type bodyPerson struct {
	Who   string       `json:"who"`
	Name  string       `json:"name"`
	Spec  body.Spec    `json:"spec"`
	State bodyStateOut `json:"state"`
	Facts []bodyFact   `json:"facts"`
}

type bodyStateOut struct {
	Now       string          `json:"now,omitempty"`
	Cycle     *body.Cycle     `json:"cycle,omitempty"`
	CycleRef  string          `json:"cycle_ref,omitempty"`
	RefKind   string          `json:"ref_kind,omitempty"`
	Pregnancy *body.Pregnancy `json:"pregnancy,omitempty"`
	Recovery  bool            `json:"recovery,omitempty"`
	Lines     []string        `json:"lines"` // as delivered
}

type bodyFact struct {
	ID      int64          `json:"id"`
	Kind    string         `json:"kind"`
	Day     string         `json:"day,omitempty"`
	Scope   string         `json:"scope,omitempty"`
	User    bool           `json:"user"` // recorded on this screen
	Q       string         `json:"q,omitempty"`
	Partner []string       `json:"partners,omitempty"`
	Result  *body.Exposure `json:"result,omitempty"`
}

// bodyHandler is the body screen (1.0 E09/E10, AC1's 주기·임신 panel):
//   - GET /body?chat_id= : the switches and every tracked person's state,
//     settings and facts
//   - POST /body/settings {"cycle_tracking_enabled","automatic_pregnancy_enabled"}
//   - POST /body/person {"chat_id","who","spec"} : a person's model settings
//   - POST /body/record {"chat_id","who","kind","day"} : a fact the user
//     records (AC1 fact correction); day is a date or "Day N"
//   - POST /body/record/delete {"chat_id","id"} : removes a recorded fact
//     (AC1 undo); facts read from turns change with their turns
func bodyHandler(pipe *pipeline.Pipeline, db *store.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reply := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v)
		}
		if r.Method == http.MethodGet {
			chat := r.URL.Query().Get("chat_id")
			if chat == "" {
				settingsError(w, 400, "chat_required")
				return
			}
			on, err := db.BodyTracking()
			if err != nil {
				storeFailed(w, 500, err)
				return
			}
			var scope []memory.ScopedTurn
			if err := pipe.WithSession("chat:"+chat, func(s *lifecycle.Session) { scope = memory.ScopeOf(s) }); err != nil {
				storeFailed(w, 500, err)
				return
			}
			states, err := db.BodyStates("chat:"+chat, scope)
			if err != nil {
				storeFailed(w, 500, err)
				return
			}
			var refs []string
			for _, st := range states {
				refs = append(refs, st.Who)
				if p := st.Pregnancy; p != nil {
					refs = append(refs, p.Candidates...)
				}
				for _, f := range st.Facts {
					refs = append(refs, f.Partners...)
				}
			}
			names, err := db.NamesByID(refs)
			if err != nil {
				storeFailed(w, 500, err)
				return
			}
			name := func(id string) string {
				if n, ok := names[id]; ok {
					return n
				}
				return id
			}
			people := []bodyPerson{}
			for _, st := range states {
				p, err := db.BodyPerson("chat:"+chat, st.Who)
				if err != nil {
					storeFailed(w, 500, err)
					return
				}
				out := bodyPerson{Who: st.Who, Name: name(st.Who), Spec: p.Spec, Facts: []bodyFact{}, State: bodyStateOut{Now: st.Now.Text(), Cycle: st.Cycle,
					CycleRef: st.CycleRef.Text(), RefKind: st.RefKind, Pregnancy: st.Pregnancy, Recovery: st.Recovery, Lines: body.Lines([]body.State{st}, name)}}
				for _, f := range st.Facts {
					partners := []string{}
					for _, id := range f.Partners {
						partners = append(partners, name(id))
					}
					out.Facts = append(out.Facts, bodyFact{ID: f.ID, Kind: f.Kind, Day: f.Day.Text(), Scope: f.Scope, User: f.User, Q: f.Q, Partner: partners, Result: f.Result})
				}
				people = append(people, out)
			}
			reply(map[string]any{"cycle_tracking_enabled": on.Cycle, "automatic_pregnancy_enabled": on.Pregnancy, "budget": body.Budget, "people": people})
			return
		}
		if r.Method != http.MethodPost {
			settingsError(w, 405, "method_not_allowed")
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/body") {
		case "/settings":
			var t body.Toggles
			if json.NewDecoder(r.Body).Decode(&t) != nil {
				settingsError(w, 400, "bad_request")
				return
			}
			if db.SetBodyTracking(t) != nil {
				settingsError(w, 500, "store_failed")
				return
			}
			reply(t)
		case "/person":
			var req struct {
				ChatID string    `json:"chat_id"`
				Who    string    `json:"who"`
				Spec   body.Spec `json:"spec"`
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.ChatID == "" || req.Who == "" {
				settingsError(w, 400, "bad_request")
				return
			}
			if !req.Spec.Valid() {
				settingsError(w, 400, "invalid_body_settings")
				return
			}
			if db.SetBodyPerson("chat:"+req.ChatID, body.Person{Who: req.Who, Spec: req.Spec}) != nil {
				settingsError(w, 500, "store_failed")
				return
			}
			reply(map[string]any{"status": "ok"})
		case "/record":
			var req struct {
				ChatID string `json:"chat_id"`
				Who    string `json:"who"`
				Kind   string `json:"kind"`
				Day    string `json:"day"`
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.ChatID == "" || req.Who == "" {
				settingsError(w, 400, "bad_request")
				return
			}
			id, err := db.RecordBodyFact("chat:"+req.ChatID, req.Who, req.Kind, req.Day)
			if err != nil {
				if err == store.ErrBodyFact {
					settingsError(w, 400, "invalid_body_fact")
					return
				}
				settingsError(w, 500, "store_failed")
				return
			}
			reply(map[string]any{"id": id})
		case "/record/delete":
			var req struct {
				ChatID string `json:"chat_id"`
				ID     int64  `json:"id"`
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.ChatID == "" || req.ID == 0 {
				settingsError(w, 400, "bad_request")
				return
			}
			n, err := db.DeleteBodyFact("chat:"+req.ChatID, req.ID)
			if err != nil {
				storeFailed(w, 500, err)
				return
			}
			if n == 0 {
				settingsError(w, 404, "body_fact_not_found")
				return
			}
			reply(map[string]any{"status": "ok"})
		default:
			settingsError(w, 404, "not_found")
		}
	}
}
