package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// E12 HypaMemory import (AC1's /import/hypamemory: when the
// user presses the button and the chat has HypaMemory summaries, every
// summary is imported, duplicates or not, and made again into AC2 memory).
//
//	POST /memory/hypa {"chat_id","chat_name","summaries":[{"text",
//	  "important","tags","category"}],"dry_run"}
//
// The summaries become a stored chat of their own ("hypa:<chat_id>"), one
// turn per summary in the order given: the summary is the turn's reply, its
// marks the turn's input. Its memories are made by the cold start (one
// critic call per summary not made yet), and it is attached before the
// chat's own turns as its earlier story (C22), ahead of any chat attached
// already. No new generation or delivery path: what a turn of the chat
// is made into and how its memory is delivered stay as they are.
//
// A summary's turn is named by its text (and how many times that text came
// before), so importing again keeps the memories of the summaries that did
// not change and makes only the new ones; a summary no longer there leaves
// with its turn. dry_run counts without writing.
const hypaMax = 500

type hypaSummary struct {
	Text      string   `json:"text"`
	Important bool     `json:"important"`
	Tags      []string `json:"tags"`
	Category  string   `json:"category"`
}

// hypaChat is the stored chat that holds a chat's imported summaries.
func hypaChat(chat string) string { return "hypa:" + chat }

// hypaRows are the summaries as the rows of their chat (observation.v1
// stored_chat rows): per summary a user row with its marks and a reply row
// with its text, their ids from the text.
// It also gives each summary's reply row id and user-row marks, in order.
func hypaRows(sums []hypaSummary) ([]map[string]any, []string, []string) {
	var rows []map[string]any
	var ids, marksOf []string
	seen := map[string]int{}
	for _, s := range sums {
		h := sha256.Sum256([]byte(s.Text))
		key := hex.EncodeToString(h[:8])
		seen[key]++
		id := fmt.Sprintf("hypa-%s-%d", key, seen[key])
		marks := []string{"[HypaMemory 요약]"}
		if s.Important {
			marks = append(marks, "중요")
		}
		if c := strings.TrimSpace(s.Category); c != "" {
			marks = append(marks, "분류: "+c)
		}
		var tags []string
		for _, t := range s.Tags {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
		if len(tags) > 0 {
			marks = append(marks, "태그: "+strings.Join(tags, ", "))
		}
		n := len(rows)
		ids, marksOf = append(ids, id+"-a"), append(marksOf, strings.Join(marks, " · "))
		rows = append(rows,
			map[string]any{"i": n, "role": "user", "chat_id": id + "-u", "data": map[string]any{"text": strings.Join(marks, " · ")}},
			map[string]any{"i": n + 1, "role": "char", "chat_id": id + "-a", "generation_id": id, "data": map[string]any{"text": s.Text}})
	}
	return rows, ids, marksOf
}

func hypaHandler(pipe *pipeline.Pipeline, worker *generate.Worker, db *store.DB, server *observe.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		var req struct {
			ChatID    string        `json:"chat_id"`
			ChatName  string        `json:"chat_name"`
			Summaries []hypaSummary `json:"summaries"`
			Dry       bool          `json:"dry_run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatID == "" || strings.HasPrefix(req.ChatID, "hypa:") {
			settingsError(w, http.StatusBadRequest, "bad_request")
			return
		}
		var sums []hypaSummary
		for _, s := range req.Summaries {
			if s.Text = strings.TrimSpace(s.Text); s.Text != "" {
				sums = append(sums, s)
			}
		}
		switch {
		case len(sums) == 0:
			settingsError(w, http.StatusBadRequest, "hypa_empty")
			return
		case len(sums) > hypaMax:
			settingsError(w, http.StatusBadRequest, "hypa_too_many")
			return
		case server == nil || worker == nil:
			settingsError(w, http.StatusServiceUnavailable, "store_failed")
			return
		}
		key := "chat:" + hypaChat(req.ChatID)
		rows, ids, marks := hypaRows(sums)
		// the summaries a call is made for: those whose turn has no memory
		// made from the same rows as now (its reply row and its marks: the
		// job is named by the turn's texts)
		made := map[string]string{} // reply row key -> marks of a made turn
		if src, ok, err := db.LoadSession(key); err != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		} else if ok {
			keys, err := db.DoneKeys(key)
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			for _, t := range memory.Own(src) {
				if keys[t.Source.Key()] && len(t.Source.Members) > 0 {
					for _, res := range t.Source.Responses {
						made[res.Key] = t.Source.Members[0].Data
					}
				}
			}
		}
		fresh := 0
		for i, id := range ids {
			if m, ok := made["c:"+id]; !ok || m != marks[i] {
				fresh++
			}
		}
		answer := map[string]any{"chat": req.ChatID, "hypa_chat": hypaChat(req.ChatID), "summaries": len(sums), "to_make": fresh, "dry_run": req.Dry}
		if req.Dry {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(answer)
			return
		}
		// the summaries' chat as one load observation, stored and applied
		// like the plugin's
		name := "HypaMemory 요약"
		if n := strings.TrimSpace(req.ChatName); n != "" {
			name += " · " + n
		}
		ev, _ := json.Marshal(map[string]any{"boot_id": "hypa-import:" + req.ChatID, "seq": time.Now().UnixNano(), "hook": "load",
			"stored_chat": map[string]any{"chat_id": hypaChat(req.ChatID), "name": name, "rows": rows}})
		if _, _, _, err := server.Apply(observe.Batch{Contract: observe.ContractVersion, PluginVersion: "hypa-import", Events: []json.RawMessage{ev}}); err != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		}
		var asked []int
		var berr error
		if err := pipe.WithSession(key, func(s *lifecycle.Session) {
			asked, berr = worker.Backfill(key, memory.WithPast(s), 0, false)
		}); err != nil || berr != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		}
		// attached first: the summaries are the oldest story; a chat
		// attached already stays after them, an earlier import is replaced
		var attachErr error
		locked := func() error {
			return pipe.WithSession("chat:"+req.ChatID, func(s *lifecycle.Session) {
				src, ok, err := db.LoadSession(key)
				if err != nil || !ok {
					attachErr = fmt.Errorf("hypa chat not stored: %v", err)
					return
				}
				// in the order the summaries came now: a new turn of the
				// stored chat is added after its others, so its own order
				// is the order first imported
				place := map[string]int{}
				for i, id := range ids {
					place["c:"+id] = i
				}
				past := memory.PastOf(src)
				at := func(p lifecycle.PastTurn) int {
					var src memory.Source
					if json.Unmarshal([]byte(p.Source), &src) == nil {
						for _, r := range src.Responses {
							if i, ok := place[r.Key]; ok {
								return i
							}
						}
					}
					return len(ids)
				}
				sort.SliceStable(past, func(a, b int) bool { return at(past[a]) < at(past[b]) })
				for _, p := range s.Past {
					if p.From != key {
						past = append(past, p)
					}
				}
				past = memory.UniquePast(past)
				if attachErr = db.SetPast(s.Key, past); attachErr != nil {
					return
				}
				s.Past = past
				worker.Notify(s.Key, memory.WithPast(s))
			})
		}
		err := worker.WithChatOperation(locked)
		if err != nil || attachErr != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		}
		answer["turns"] = asked
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(answer)
	}
}
