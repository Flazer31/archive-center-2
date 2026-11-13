package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/store"
)

// activityKept is how many recent events the status screen lists (10-27);
// they are kept in memory only and start empty after a restart.
const activityKept = 20

// event is one line of the recent activity list.
type event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"` // injected, made, failed, given_up, index_failed, query_failed, rearmed
	Chat   string    `json:"chat,omitempty"`
	Turn   int       `json:"turn,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// injection is what the last memory request got.
type injection struct {
	DeliveryID      int64     `json:"-"` // management detail only
	At              time.Time `json:"at"`
	Chat            string    `json:"chat"`
	Items           int       `json:"items"`
	Chars           int       `json:"chars"`
	CharsMax        int       `json:"chars_max"`
	SecretChars     int       `json:"secret_chars"`
	SecretMax       int       `json:"secret_max"`
	Ranked          string    `json:"ranked"`
	RecentQueryUsed *bool     `json:"recent_query_used,omitempty"`
	Warning         string    `json:"warning,omitempty"`
	TotalMS         int64     `json:"total_ms"`
	// Stages split TotalMS by step (1.1 A1); none for traces before 1.1
	Stages []store.DeliveryStage `json:"stages,omitempty"`
	// Prefix: how much of the request equals the previous one from the
	// start (1.1 A2); nil when there was none to compare
	Prefix *store.RequestPrefix `json:"prefix,omitempty"`
	// Preprocess is what a preprocessing mode did (nil when off)
	Preprocess *store.PreprocessRecord `json:"preprocess,omitempty"`
	// Narrative is what narrative support did (nil when off); the texts
	// (guide, input before and after) only in NarrativeDetail
	Narrative       *store.NarrativeRecord `json:"narrative,omitempty"`
	NarrativeDetail *store.NarrativeRecord `json:"-"`
	// Detailed contents belong only to /injection, never /status or /hud.
	Block     string                 `json:"-"`
	Memories  []store.InjectedMemory `json:"-"`
	ReadError string                 `json:"-"`
}

// vectorPath is the last outcome of one kind of vector call: indexing the
// memories, or the query of a memory request. Each is cleared only by its
// own success.
type vectorPath struct {
	Failing bool
	Detail  string
	At      time.Time
}

// injectionsKept is how many chats keep their last injection for the
// status screen (the oldest is dropped); memory only.
const injectionsKept = 50

// status gathers what the status screen and the chat chip show (step 9-3):
// for the current chat (its generation and last injection) and for the
// backend (connections, vectors, recent activity). Go decides every state;
// the plugin only shows it.
type status struct {
	db     *store.DB
	mods   *models
	worker *generate.Worker
	now    func() time.Time

	mu     sync.Mutex
	events []event // oldest first
	last   map[string]*injection
	index  vectorPath
	query  vectorPath
	hud    *hud
}

func newStatus(db *store.DB, mods *models, worker *generate.Worker) *status {
	return &status{db: db, mods: mods, worker: worker, now: time.Now, last: map[string]*injection{}}
}

// detail is an error as the general status and HUD show it: a category,
// never the text itself (a provider error can echo the input or a protected
// name back). The raw text stays in the management records that keep it.
func detail(err error) string {
	if err == nil {
		return ""
	}
	return diagnostics.ErrorSummary(err.Error())
}

// rawDetail keeps the shortened text for the token-protected management
// trace, which already holds the request's own input.
func rawDetail(err error) string {
	if err == nil {
		return ""
	}
	return shorten(err.Error())
}

func shorten(text string) string {
	return diagnostics.Redact(text) // whole, credentials masked
}

func (s *status) add(e event) {
	e.At = s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	if len(s.events) > activityKept {
		s.events = s.events[len(s.events)-activityKept:]
	}
}

// workerEvent is the generation worker's Event.
func (s *status) workerEvent(kind, session string, turn int, err error) {
	chat := strings.TrimPrefix(session, "chat:")
	// every generation and index outcome goes to the log too, with its whole
	// error (credentials masked), so a failure can be read there
	if kind != "indexed" {
		if err != nil {
			log.Printf("worker: %s %s turn %d: %s", kind, session, turn, diagnostics.ErrorSummary(err.Error()))
		} else {
			log.Printf("worker: %s %s turn %d", kind, session, turn)
		}
	}
	switch kind {
	case "indexed":
		s.mu.Lock()
		s.index = vectorPath{At: s.now()}
		s.mu.Unlock()
		return
	case "index_failed":
		s.mu.Lock()
		s.index = vectorPath{Failing: true, Detail: detail(err), At: s.now()}
		s.mu.Unlock()
	case "rearmed":
		s.add(event{Kind: kind, Detail: strconv.Itoa(turn)})
		return
	}
	s.add(event{Kind: kind, Chat: chat, Turn: turn, Detail: detail(err)})
}

// injected records a memory request's result.
func (s *status) injected(chat string, d store.Delivery, warning string, charsMax, secretMax int, ms int64, queryErr error, queried bool, scope ...memory.ScopedTurn) *injection {
	in := &injection{At: s.now(), Chat: chat, Items: len(d.IDs), Chars: d.Chars, CharsMax: charsMax, SecretChars: d.SecretChars, SecretMax: secretMax, Ranked: d.Ranked, Warning: warning, TotalMS: ms}
	in.RecentQueryUsed = &d.RecentQueryUsed
	if d.Preprocess != nil {
		// the provider's error text may quote the request: status and HUD
		// carry its summary only
		p := *d.Preprocess
		p.Error = diagnostics.ErrorSummary(p.Error)
		in.Preprocess = &p
	}
	in.Narrative, in.NarrativeDetail = narrativeViews(d.Narrative)
	in.Block = d.Block
	in.Memories = []store.InjectedMemory{}
	if len(d.IDs) > 0 {
		var err error
		in.Memories, err = s.db.InjectedMemories("chat:"+chat, d.IDs, scope, d.SecretIDs)
		if err != nil {
			// Diagnostic read failure must not reject a completed delivery.
			in.ReadError = "store_failed"
		}
	}
	if s.hud != nil {
		s.hud.injected(chat, in, d.IDs)
	}
	s.mu.Lock()
	s.last[chat] = in
	if len(s.last) > injectionsKept {
		oldest := ""
		for k, v := range s.last {
			if oldest == "" || v.At.Before(s.last[oldest].At) {
				oldest = k
			}
		}
		delete(s.last, oldest)
	}
	switch {
	case queryErr != nil:
		s.query = vectorPath{Failing: true, Detail: detail(queryErr), At: in.At}
	case queried:
		s.query = vectorPath{At: in.At}
	}
	s.mu.Unlock()
	if queryErr != nil {
		s.add(event{Kind: "query_failed", Chat: chat, Detail: detail(queryErr)})
	}
	s.add(event{Kind: "injected", Chat: chat, Detail: warning})
	return in
}

func diagInt(d map[string]any, k string) int {
	n, _ := d[k].(int)
	return n
}

// view is the status the screen shows, for chat (its key without "chat:";
// "" = the backend as a whole). chip is the one state the chat chip shows,
// the first that holds of: embedding_failed, retrying, generating,
// given_up, no_critic, done.
func (s *status) view(chat string) map[string]any {
	d := s.worker.Diagnostics()
	mv := s.mods.view()
	critic := mv["critic"].(map[string]any)
	emb := mv["embedding"].(map[string]any)
	model, _ := emb["vector_model"].(string) // "" = no embedding connection
	keySet, _ := emb["key_set"].(bool)
	s.mu.Lock()
	index, query := s.index, s.query
	var last *injection
	if chat != "" {
		last = s.last[chat]
	}
	list := make([]event, 0, len(s.events))
	for i := len(s.events) - 1; i >= 0; i-- {
		list = append(list, s.events[i])
	}
	s.mu.Unlock()

	vstate := "unknown" // no vector call since the start
	switch {
	case model == "":
		vstate = "off"
	case index.Failing || query.Failing:
		vstate = "failing"
	case !index.At.IsZero() || !query.At.IsZero():
		vstate = "ok"
	}
	waitingVectors := 0
	if model != "" {
		if n, err := s.db.VectorsWaiting(model); err == nil {
			waitingVectors = n
		}
	}
	// the chat's own generation, or the whole backend's without a chat
	var cs generate.ChatState
	if chat != "" {
		cs = s.worker.Chat("chat:" + chat)
	} else {
		cs = generate.ChatState{
			Waiting:      diagInt(d, "generation_waiting_chats") > 0,
			Running:      d["generation_running"] == true,
			RetryWaiting: diagInt(d, "generation_retry_waiting_chats") > 0,
			GivenUp:      diagInt(d, "generation_given_up_turns"),
		}
	}
	noCritic := critic["provider"] == "" || critic["problem"] != ""
	chip := "done"
	switch {
	case vstate == "failing":
		chip = "embedding_failed"
	case cs.RetryWaiting:
		chip = "retrying"
	case (cs.Waiting || cs.Running) && !noCritic:
		chip = "generating"
	case cs.GivenUp > 0:
		chip = "given_up"
	case noCritic:
		chip = "no_critic"
	}
	vector := map[string]any{"model": model, "key_set": keySet, "state": vstate, "waiting_vectors": waitingVectors, "backlog_chats": diagInt(d, "index_backlog_chats"),
		"index_failing": index.Failing, "query_failing": query.Failing}
	vector["failures"] = diagInt(d, "index_failures")
	vector["running_chats"] = diagInt(d, "index_running_chats")
	vector["running_sessions"] = d["index_running_sessions"]
	vector["retry_waiting_chats"] = diagInt(d, "index_retry_waiting_chats")
	if index.Failing {
		vector["index_detail"], vector["index_at"] = index.Detail, index.At
	}
	if query.Failing {
		vector["query_detail"], vector["query_at"] = query.Detail, query.At
	}
	lastErr, _ := d["generation_last_error"].(string)
	return map[string]any{
		"chip":    chip,
		"chat":    chat,
		"version": observe.ServiceVersion,
		"generation": map[string]any{
			"critic": map[string]any{"provider": critic["provider"], "model": critic["model"], "source": critic["source"], "problem": critic["problem"]},
			"chat":   cs,
			// the whole backend
			"waiting_chats":       diagInt(d, "generation_waiting_chats"),
			"running":             d["generation_running"] == true,
			"running_chats":       diagInt(d, "generation_running_chats"),
			"running_sessions":    d["generation_running_sessions"],
			"retry_waiting_chats": diagInt(d, "generation_retry_waiting_chats"),
			"given_up_turns":      diagInt(d, "generation_given_up_turns"),
			"made":                diagInt(d, "generation_runs"),
			"failures":            diagInt(d, "generation_failures"),
			"last_error":          diagnostics.ErrorSummary(lastErr),
		},
		"injection": last,
		"embedding": vector,
		"activity":  list,
	}
}

// handler answers GET /status?chat_id=<the chat in view>.
func (s *status) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.view(r.URL.Query().Get("chat_id")))
	}
}

// narrativeViews are the status/HUD summary of a narrative record (no texts,
// errors summarized) and the management detail (/injection).
func narrativeViews(r *store.NarrativeRecord) (*store.NarrativeRecord, *store.NarrativeRecord) {
	if r == nil {
		return nil, nil
	}
	v := *r
	v.GuideText, v.InputBefore, v.InputAfter = "", "", ""
	v.GuideError, v.InputError = diagnostics.ErrorSummary(v.GuideError), diagnostics.ErrorSummary(v.InputError)
	detail := *r
	detail.GuideError, detail.InputError = v.GuideError, v.InputError
	return &v, &detail
}
