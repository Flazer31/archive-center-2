package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

type hudStage struct {
	Key       string     `json:"key"`
	State     string     `json:"state"`
	At        *time.Time `json:"at"`
	ElapsedMS *int64     `json:"elapsed_ms"`
	Detail    string     `json:"detail,omitempty"`
	Count     int        `json:"count"`
}

type hudTurn struct {
	Turn         int        `json:"turn"`
	Stages       []hudStage `json:"stages"`
	CurrentStage hudStage   `json:"current_stage"`
	Injection    *injection `json:"injection"`
	IDs          []string   `json:"ids"`
	Retry        bool       `json:"retry"`
	Off          bool       `json:"off"`
	NextAt       *time.Time `json:"next_at"`
	// the generation's calls (user 11-08: a retry is told on the HUD too):
	// Attempt is the call being made or the last one made, of Attempts
	// allowed; Retried says this turn is being tried again (by the user or
	// after a failed call)
	Attempt  int  `json:"attempt"`
	Attempts int  `json:"attempts"`
	Retried  bool `json:"retried"`
}

// hudFailure is an earlier turn whose generation failed and is not done,
// outside the latest two turns (review 19 B-04): told with its retry.
type hudFailure struct {
	Turn     int        `json:"turn"`
	State    string     `json:"state"` // failed | retry_waiting | waiting (asked again)
	Detail   string     `json:"detail,omitempty"`
	Attempt  int        `json:"attempt"`
	Attempts int        `json:"attempts"`
	NextAt   *time.Time `json:"next_at"`
	Retry    bool       `json:"retry"`
}

type hudClock struct {
	source                                   string
	observed, response, confirmed            *time.Time
	observedMS                               *int64
	generation, generated, indexing, indexed *time.Time
	state, vector, reason                    string
	genErr                                   string // a failure the worker heard that the store may not hold
	injection                                *injection
	ids                                      []string
	retry                                    bool
}

type hudChat struct {
	at    time.Time
	turns map[int]*hudClock
}

// hud projects existing lifecycle/jobs/status. Only measured times and the
// per-turn injection are transient: two turns per chat, at most the status
// tracker's 50 chats. Unknown times stay null after a restart.
type hud struct {
	db     *store.DB
	pipe   *pipeline.Pipeline
	worker *generate.Worker
	st     *status
	mu     sync.Mutex
	chats  map[string]*hudChat
	now    func() time.Time
}

func newHUD(db *store.DB, pipe *pipeline.Pipeline, worker *generate.Worker, st *status) *hud {
	h := &hud{db: db, pipe: pipe, worker: worker, st: st, chats: map[string]*hudChat{}, now: time.Now}
	pipe.Observed = h.observed
	st.hud = h
	worker.Progress = h.progress
	return h
}

func (h *hud) chat(key string) *hudChat { // h.mu held
	c := h.chats[key]
	if c == nil {
		if len(h.chats) >= injectionsKept {
			var oldest string
			for k, v := range h.chats {
				if oldest == "" || v.at.Before(h.chats[oldest].at) {
					oldest = k
				}
			}
			delete(h.chats, oldest)
		}
		c = &hudChat{turns: map[int]*hudClock{}}
		h.chats[key] = c
	}
	c.at = h.now()
	return c
}

func recentTurns(s *lifecycle.Session) []*lifecycle.Turn {
	out := []*lifecycle.Turn{}
	for i := len(s.Turns) - 1; i >= 0 && len(out) < 2; i-- {
		if !s.Turns[i].Inactive {
			out = append(out, s.Turns[i])
		}
	}
	return out
}

func (h *hud) observed(s *lifecycle.Session, events []lifecycle.Event, started time.Time, elapsedMS int64) {
	at := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.chat(s.Key)
	kept := map[int]*hudClock{}
	for _, t := range recentTurns(s) {
		src := memory.SourceOf(s.Key, t)
		v := c.turns[t.ID]
		if v == nil {
			v = &hudClock{}
		}
		if v.observed == nil {
			v.observed, v.observedMS = &started, &elapsedMS
		}
		if v.source != src.Key() {
			v.source = src.Key()
			v.generation, v.generated, v.indexing, v.indexed = nil, nil, nil, nil
			v.state, v.vector, v.reason, v.genErr = "", "", "", ""
			v.retry = false
			v.confirmed = nil
			if !src.Readable() {
				v.response = nil
			}
		}
		for _, e := range events {
			if e.TurnID != t.ID {
				continue
			}
			switch e.Kind {
			case "response_added", "response_revised", "response_new_generation", "response_replaced":
				if src.Readable() {
					v.response = &at
				}
			case "turn_confirmed":
				v.confirmed = &at
			}
		}
		kept[t.ID] = v
	}
	// an earlier turn asked again, or running, keeps its clock until it ends
	// (review 19 R2-B01)
	for id, v := range c.turns {
		if kept[id] == nil && v.held() {
			kept[id] = v
		}
	}
	c.turns = kept
}

func (v *hudClock) held() bool { return v.retry || v.state == "running" }

func (h *hud) injected(chat string, in *injection, ids []string) {
	// HUD keeps scalar diagnostics only; the status tracker owns the one
	// detailed snapshot per chat, so old turns cannot retain full blocks.
	summary := *in
	summary.Block, summary.Memories, summary.ReadError = "", nil, ""
	// The call is outside the status lock; pipeline -> HUD is the only
	// lock order, also used by observation and view.
	_ = h.pipe.WithSession("chat:"+chat, func(s *lifecycle.Session) {
		recent := recentTurns(s)
		if len(recent) == 0 {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		c := h.chat(s.Key)
		v := c.turns[recent[0].ID]
		if v == nil {
			v = &hudClock{source: memory.SourceOf(s.Key, recent[0]).Key()}
			c.turns[recent[0].ID] = v
		}
		v.injection = &summary
		v.ids = append([]string{}, ids...)
	})
}

func (h *hud) progress(p generate.Progress) {
	at := h.now()
	sources := []store.HUDJobSource{{Turn: p.Turn, Source: p.Source}}
	if p.Kind == "indexing" || p.Kind == "index_finished" {
		var err error
		sources, err = h.db.HUDJobSources(p.Jobs)
		if err != nil {
			return
		} // missing diagnostics never changes worker behavior
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.chat(p.Session)
	for _, src := range sources {
		v := c.turns[src.Turn]
		if v == nil {
			free := 0
			oldest := src.Turn
			for id, known := range c.turns {
				if known.held() {
					continue
				}
				free++
				if id < oldest {
					oldest = id
				}
			}
			if free >= 2 {
				if oldest == src.Turn {
					continue
				}
				delete(c.turns, oldest)
			}
			v = &hudClock{source: src.Source}
			c.turns[src.Turn] = v
		}
		// Late measurements describe the old revision only. They do not
		// overwrite the displayed timing of its replacement.
		if v.source != src.Source {
			continue
		}
		switch p.Kind {
		case "generating":
			// a manual retry stays told while its call runs (review 19 B-03)
			v.generation, v.generated, v.state, v.genErr = &at, nil, "running", ""
		case "generation_finished":
			v.generated, v.state, v.retry, v.genErr = &at, "done", false, ""
			if p.Err != nil {
				v.state, v.genErr = "failed", detail(p.Err)
			}
		case "indexing":
			v.indexing, v.indexed, v.vector = &at, nil, "running"
		case "index_finished":
			v.indexed, v.vector = &at, "done"
			if p.Err != nil {
				v.vector, v.reason = "failed", detail(p.Err)
			}
		}
	}
}

func hudDuration(start, end *time.Time) *int64 {
	if start == nil || end == nil {
		return nil
	}
	n := max(int64(0), end.Sub(*start).Milliseconds())
	return &n
}

// generationState tells one turn's generation from its stored job and what
// the worker reported, the same for the latest turns and the earlier
// failures (review 19 R2-B01): state, the error summary and whether the
// retry button is offered.
func (h *hud) generationState(off bool, job store.HUDJob, v hudClock, w generate.ChatState) (string, string, bool) {
	failed := job.State == "failed" || job.State == "unreadable"
	switch {
	case off:
		return "off", "", false
	case job.State == "done":
		return "done", "", false
	case v.state == "running" && w.Running:
		return "running", "", false
	case v.retry: // asked again by the user, not yet running (review 19 B-03)
		return "waiting", "", false
	case failed && job.Attempts > 0:
		if job.Attempts <= h.worker.RetryLimit() && job.NextAt != nil {
			return "retry_waiting", diagnostics.ErrorSummary(job.Reason), true
		}
		return "failed", diagnostics.ErrorSummary(job.Reason), true
	case v.state == "failed":
		// the call or its saving failed and the store does not hold that
		// failure (review 19 B-01, R2-B02): told as the worker heard it
		if w.RetryWaiting {
			return "retry_waiting", v.genErr, true
		}
		return "failed", v.genErr, true
	case failed: // rearmed, its call not started yet
		return "waiting", "", false
	}
	return "waiting", "", false
}

func (h *hud) view(chat string) (map[string]any, error) {
	settings, err := h.db.HUDSettings()
	if err != nil {
		return nil, err
	}
	out := []hudTurn{}
	failures := []hudFailure{}
	active := false
	critic := h.st.mods.view()["critic"].(map[string]any)
	criticReady := critic["provider"] != "" && critic["problem"] == ""
	workerState := h.worker.Chat("chat:" + chat)
	var readErr error
	err = h.pipe.WithSession("chat:"+chat, func(s *lifecycle.Session) {
		for _, t := range recentTurns(s) {
			own := memory.SourceOf(s.Key, t)
			src := own
			one := *s
			one.Turns = []*lifecycle.Turn{t}
			if scope := memory.Own(&one); len(scope) > 0 {
				var e error
				src, e = h.db.HUDSource(scope[0])
				if e != nil {
					readErr = e
					return
				}
			}
			job, e := h.db.HUDJob(src)
			if e != nil {
				readErr = e
				return
			}
			var v hudClock
			h.mu.Lock()
			if c := h.chats[s.Key]; c != nil {
				if known := c.turns[t.ID]; known != nil {
					v = *known
				}
			}
			h.mu.Unlock()
			if v.source != "" && v.source != own.Key() {
				v = hudClock{}
			}
			observed := hudStage{Key: "observed", State: "done", At: v.observed, ElapsedMS: v.observedMS}
			in := hudStage{Key: "injected", State: "waiting"}
			if v.injection != nil {
				in.State, in.At, in.ElapsedMS, in.Count, in.Detail = "done", &v.injection.At, &v.injection.TotalMS, v.injection.Items, v.injection.Warning
			}
			reply := hudStage{Key: "response_saved", State: "waiting", At: v.response}
			if v.injection != nil && !src.Readable() {
				now := h.now()
				reply.ElapsedMS = hudDuration(&v.injection.At, &now) // waiting for the reply so far
			}
			if src.Readable() {
				reply.State = "done"
				if v.injection != nil {
					reply.ElapsedMS = hudDuration(&v.injection.At, v.response)
				}
			}
			confirmed := hudStage{Key: "confirmed", State: "waiting", At: v.confirmed}
			if t.Confirmed {
				confirmed.State = "done"
				confirmed.ElapsedMS = hudDuration(v.response, v.confirmed)
			}
			gen := hudStage{Key: "generation", At: v.generation, Count: job.Made}
			var retry bool
			gen.State, gen.Detail, retry = h.generationState(t.Off, job, v, workerState)
			if gen.State == "done" && v.generated == nil {
				v.generated = job.DoneAt
			}
			if gen.At == nil {
				gen.At = job.CreatedAt
			}
			gen.ElapsedMS = hudDuration(v.generation, v.generated)
			if gen.State == "running" {
				now := h.now()
				gen.ElapsedMS = hudDuration(v.generation, &now)
			}
			vec := hudStage{Key: "vectors", State: "waiting", At: v.indexing, ElapsedMS: hudDuration(v.indexing, v.indexed)}
			if v.vector != "" {
				vec.State = v.vector
				if vec.State == "failed" {
					vec.Detail = v.reason
					if workerState.RetryWaiting {
						vec.State = "retry_waiting"
					}
				}
			}
			if q := h.st.mods.query(); q == nil {
				vec.State = "off"
			} else if job.State == "done" && job.Made == 0 && v.vector == "" {
				vec.State = "skipped" // the turn made no memory: nothing to index
			} else if job.State == "done" && v.vector == "" {
				ready, total, e := h.db.HUDVectors(src, q.VectorModel())
				if e != nil {
					readErr = e
					return
				}
				vec.Count = ready
				if ready == total {
					vec.State = "done"
				}
			}
			// a confirmed turn's generation is followed until it ends (done,
			// failed with its calls used up, or off), from the stored job: the
			// worker's queue state can be empty between being notified and
			// running (11-08 HUD rebuild)
			generationMoving := gen.State == "running" || gen.State == "retry_waiting" || gen.State == "waiting"
			if criticReady && t.Confirmed && !t.Off && generationMoving {
				active = true
			}
			if vec.State == "running" || vec.State == "retry_waiting" {
				active = true
			}
			stages := []hudStage{observed, in, reply, confirmed, gen, vec}
			current := observed
			for _, stage := range stages {
				if stage.State != "waiting" && stage.State != "off" {
					current = stage
				}
			}
			if t.Off {
				current = gen
			}
			nextAt := job.NextAt
			if gen.State != "retry_waiting" {
				nextAt = nil
			}
			// the store counts a call as it starts (review 19 B-02)
			attempt := job.Attempts
			out = append(out, hudTurn{Turn: t.ID, Stages: stages, CurrentStage: current, Injection: v.injection, IDs: v.ids, Retry: retry, Off: t.Off, NextAt: nextAt,
				Attempt: attempt, Attempts: 1 + h.worker.RetryLimit(), Retried: v.retry || (gen.State == "running" && job.Attempts > 1)})
		}
		shown := map[int]bool{}
		for _, t := range out {
			shown[t.Turn] = true
		}
		failed, e := h.db.HUDFailedJobs(s.Key)
		if e != nil {
			readErr = e
			return
		}
		for _, j := range failed {
			if shown[j.Turn] {
				continue
			}
			for _, t := range s.Turns {
				if t.ID != j.Turn || t.Inactive || t.Off || memory.SourceOf(s.Key, t).Key() != j.Source {
					continue
				}
				var v hudClock
				h.mu.Lock()
				if c := h.chats[s.Key]; c != nil && c.turns[t.ID] != nil && c.turns[t.ID].source == j.Source {
					v = *c.turns[t.ID]
				}
				h.mu.Unlock()
				f := hudFailure{Turn: t.ID, Attempt: j.Attempts, Attempts: 1 + h.worker.RetryLimit()}
				f.State, f.Detail, f.Retry = h.generationState(false, store.HUDJob{State: "failed", Attempts: j.Attempts, NextAt: j.NextAt, Reason: j.Reason}, v, workerState)
				if f.State == "retry_waiting" {
					f.NextAt = j.NextAt
				}
				if f.State != "failed" && criticReady {
					active = true
				}
				failures = append(failures, f)
				shown[t.ID] = true
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	notices, err := h.db.HUDNotices("chat:" + chat)
	if err != nil {
		return nil, err
	}
	return map[string]any{"chat": chat, "settings": settings, "turns": out, "failures": failures, "notices": notices, "active": active}, nil
}

func (h *hud) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var answer any
		var err error
		switch {
		case r.URL.Path == "/settings/hud":
			if r.Method != http.MethodGet && r.Method != http.MethodPost {
				settingsError(w, 405, "method_not_allowed")
				return
			}
			v, e := h.db.HUDSettings()
			err = e
			if err == nil && r.Method == http.MethodPost {
				if json.NewDecoder(r.Body).Decode(&v) != nil {
					settingsError(w, 400, "bad_request")
					return
				}
				err = h.db.SetHUDSettings(v)
			}
			answer = v
		case r.URL.Path == "/hud/retry" && r.Method == http.MethodPost:
			var req struct {
				ChatID string `json:"chat_id"`
				Turn   int    `json:"turn"`
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.ChatID == "" {
				settingsError(w, 400, "bad_request")
				return
			}
			err = h.pipe.WithSession("chat:"+req.ChatID, func(s *lifecycle.Session) {
				for _, t := range s.Turns {
					if t.ID != req.Turn {
						continue
					}
					src := memory.SourceOf(s.Key, t)
					h.worker.RetryTurn(s.Key, t.ID, func() error { return h.db.RearmHUDTurn(src) })
					h.mu.Lock()
					c := h.chat(s.Key)
					v := c.turns[t.ID]
					if v == nil {
						v = &hudClock{source: src.Key()}
						c.turns[t.ID] = v
					}
					v.retry = true
					h.mu.Unlock()
					break
				}
			})
			if err == nil {
				answer, err = h.view(req.ChatID)
			}
		case r.URL.Path == "/hud" && r.Method == http.MethodGet:
			answer, err = h.view(strings.TrimSpace(r.URL.Query().Get("chat_id")))
		default:
			settingsError(w, 405, "method_not_allowed")
			return
		}
		if err != nil {
			storeFailed(w, 500, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(answer)
	}
}

// forget drops a chat's HUD clocks (the chat deleted or moved away); clear
// drops them all (the database reset).
func (h *hud) forget(key string) {
	h.mu.Lock()
	delete(h.chats, key)
	h.mu.Unlock()
}

func (h *hud) clear() {
	h.mu.Lock()
	h.chats = map[string]*hudChat{}
	h.mu.Unlock()
}
