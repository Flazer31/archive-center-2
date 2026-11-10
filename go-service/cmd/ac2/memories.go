package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// memoriesPageMax is the most memories one list answer carries.
const memoriesPageMax = 100

// factModes is the generation prompt's fact mode list, in its order: the
// memory screen's fact groups follow it, any other mode after them.
var factModes = []string{"상태", "성향", "말투", "습관", "관계", "경계", "세계 규칙", "의무", "질문·위험", "공동 목표"}

func factModeRank(mode string) string {
	for i, m := range factModes {
		if m == mode {
			return strconv.Itoa(10 + i)
		}
	}
	return "99" + mode
}

// memoriesHandler is the memory screen (step 9-4):
//   - GET /memory/list?chat_id=&q=&turn=&kind=&offset=&limit= : the chat's
//     memories, the latest turn first; q keeps those whose body, names or
//     slot hold it, turn those of one turn (the worldline's number), kind
//     one kind (event, fact, view), grouped as the screen shows it, or
//     person: the current facts and views by whom they are about (C14);
//     kinds counts each kind among those q keeps (person: the current facts
//     and views), replaced the facts and views a later record replaced
//     (person also gives people, everyone with a count, and with person=<name>
//     lists that one person only, "" being about nobody, with parts: their
//     groups and counts)
//   - POST /memory/edit {"chat_id","id","body","imp"} : body and importance
//     of a memory of the chat's own turns (absent = kept)
//   - POST /memory/delete {"chat_id","id"} or {"chat_id","ids": [...]} : deletes the selection
//
// A branch's memories read from its parent are listed, marked inherited,
// and refused for a change (inherited_read_only; user decision 10-28).
// st, when set, lists the changes in the recent activity.
func memoriesHandler(pipe *pipeline.Pipeline, db *store.DB, worker *generate.Worker, st *status) http.HandlerFunc {
	// list reads the chat's memories; turn > 0 reads that turn's only (the
	// other turns are skipped as a turn turned off is, keeping the numbers)
	list := func(chat string, turn int) ([]store.ListedMemory, error) {
		var scope, story []memory.ScopedTurn
		if err := pipe.WithSession("chat:"+chat, func(s *lifecycle.Session) { scope, story = memory.Own(s), memory.WithPast(s) }); err != nil {
			return nil, err
		}
		if turn > 0 {
			for i := range scope {
				scope[i].Off = scope[i].Off || i+1 != turn
			}
		}
		// dates as injection reads them: over the whole story, not the turns shown
		return db.ChatMemoriesIn("chat:"+chat, scope, story)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/detail"):
			q := r.URL.Query()
			if q.Get("chat_id") == "" {
				settingsError(w, http.StatusBadRequest, "chat_required")
				return
			}
			turn, err := strconv.Atoi(q.Get("turn_id"))
			if err != nil || turn <= 0 || q.Get("id") == "" {
				settingsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			limit, _ := strconv.Atoi(q.Get("limit"))
			if limit <= 0 || limit > memoriesPageMax {
				limit = memoriesPageMax
			}
			detail, err := db.MemoryDetail("chat:"+q.Get("chat_id"), turn, q.Get("id"), q.Get("cursor"), limit)
			switch {
			case errors.Is(err, store.ErrMemoryNotFound):
				settingsError(w, http.StatusNotFound, "memory_not_found")
			case errors.Is(err, store.ErrDetailCursor):
				settingsError(w, http.StatusBadRequest, "bad_cursor")
			case err != nil:
				storeFailed(w, http.StatusInternalServerError, err)
			default:
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(detail)
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/list"):
			q := r.URL.Query()
			chat := q.Get("chat_id")
			if chat == "" {
				settingsError(w, http.StatusBadRequest, "chat_required")
				return
			}
			turn, _ := strconv.Atoi(q.Get("turn"))
			all, err := list(chat, max(turn, 0))
			if err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			find := strings.ToLower(strings.TrimSpace(q.Get("q")))
			kind := q.Get("kind")
			var kept []store.ListedMemory
			kinds := map[string]int{"event": 0, "fact": 0, "view": 0}
			people, replaced := 0, 0
			// events that change a value, facts and views replaced
			var history []store.ListedMemory
			for _, m := range all { // in the order made; sorted below
				// what the screen shows of it: its body, names and slot, and
				// the names and values its effects change (review 11-09 A1)
				shown := m.Body + "\n" + m.About + "\n" + m.Owner + "\n" + m.Slot
				for _, e := range m.Effects {
					shown += "\n" + e.About + "\n" + e.Slot + "\n" + e.Before + "\n" + e.Value
				}
				if find == "" || strings.Contains(strings.ToLower(shown), find) {
					kinds[m.Kind]++
					if len(m.Effects) > 0 || ((m.Kind == "fact" || m.Kind == "view") && m.Replaced) {
						history = append(history, m)
					}
					// C14 by person: the current facts and views only; a value
					// a later record replaced is counted, not shown
					inPerson := m.Kind == "fact" || m.Kind == "view"
					if inPerson && m.Replaced {
						replaced++
					} else if inPerson {
						people++
					}
					if kind == "" || m.Kind == kind || (kind == "person" && inPerson && !m.Replaced) {
						kept = append(kept, m)
					}
				}
			}
			// The latest turn first: a remake inserts new ids for an older
			// turn, so turn order, rather than insertion order, is the memory
			// screen's order; a turn's memories keep the order made. One
			// kind is grouped as the screen shows it (user 10-28: by item
			// like AC1): facts by mode, views by whose they are.
			// all kinds: a turn's events, then facts, then views
			group := func(m store.ListedMemory) string { return "" }
			rank := map[string]int{"event": 1, "fact": 2, "view": 3}
			switch kind {
			case "fact":
				group = func(m store.ListedMemory) string { return factModeRank(m.Mode) }
			case "view":
				group = func(m store.ListedMemory) string { return m.Owner }
			case "person":
				// a person's facts by mode (any other mode after them), then
				// their views; what is about nobody (world rules and the like) last
				group = func(m store.ListedMemory) string {
					who, part := m.Person+"\x01"+m.PersonRef, "1"+factModeRank(m.Mode)
					if m.Kind == "view" {
						part = "2"
					}
					if m.PersonRef == "" {
						who = "￿"
					}
					return who + "\x00" + part
				}
			}
			sort.SliceStable(kept, func(i, j int) bool {
				if gi, gj := group(kept[i]), group(kept[j]); gi != gj {
					return gi < gj
				}
				if kept[i].Turn != kept[j].Turn {
					return kept[i].Turn > kept[j].Turn
				}
				return kind == "" && rank[kept[i].Kind] < rank[kept[j].Kind]
			})
			// by name, one name at a time (user 11-09: a tab per name, its
			// parts folded): people lists every name in the order above, by
			// its stored reference (two names may read the same), with how
			// many each has (ref "" is about nobody); person=<ref> keeps one,
			// and parts counts that name's parts (a fact's mode, or 주관 기억)
			var whoList []map[string]any
			var parts []map[string]any
			var changes []map[string]any
			if kind == "person" {
				// a name's changed values (user 11-09: a thing like a holy sword
				// keeps its record and its values carry on): each effect of an
				// event about it (before -> value), and each fact or view of it a
				// later record replaced, oldest first
				type change struct {
					ref, name string
					row       map[string]any
				}
				var changed []change
				sort.SliceStable(history, func(i, j int) bool { return history[i].Turn < history[j].Turn })
				for _, m := range history {
					if m.Kind == "event" {
						for i, e := range m.Effects {
							ref := ""
							if i < len(m.EffectRefs) {
								ref = m.EffectRefs[i]
							}
							if _, isName := m.NameTypes[ref]; !isName {
								continue // an effect on a record (an obligation closed)
							}
							changed = append(changed, change{ref, e.About, map[string]any{"turn": m.Turn, "date": m.Date, "id": m.ID,
								"slot": e.Slot, "before": e.Before, "value": e.Value, "delta": e.Delta, "status": e.Status, "event": m.Body}})
						}
					} else if m.PersonRef != "" {
						changed = append(changed, change{m.PersonRef, m.Person, map[string]any{"turn": m.Turn, "date": m.Date, "id": m.ID,
							"slot": m.Slot, "value": m.Body, "replaced": true}})
					}
				}
				types := map[string]string{}
				for _, m := range all {
					for ref, t := range m.NameTypes {
						if types[ref] == "" {
							types[ref] = t
						}
					}
				}
				count, changeCount := map[string]int{}, map[string]int{}
				for _, c := range changed {
					changeCount[c.ref]++
				}
				for _, m := range kept {
					if _, ok := count[m.PersonRef]; !ok {
						whoList = append(whoList, map[string]any{"name": m.Person, "ref": m.PersonRef})
					}
					count[m.PersonRef]++
				}
				// a name with changes only has a tab too, before nobody's
				for _, c := range changed {
					if _, ok := count[c.ref]; !ok {
						count[c.ref] = 0
						at := len(whoList)
						if at > 0 && whoList[at-1]["ref"] == "" {
							at--
						}
						whoList = slices.Insert(whoList, at, map[string]any{"name": c.name, "ref": c.ref})
					}
				}
				for _, p := range whoList {
					ref := p["ref"].(string)
					p["count"], p["changes"], p["type"] = count[ref], changeCount[ref], types[ref]
				}
				if q.Has("person") {
					who := q.Get("person")
					changes = []map[string]any{}
					for _, c := range changed {
						if c.ref == who {
							changes = append(changes, c.row)
						}
					}
					one := []store.ListedMemory{}
					partOf := map[string]map[string]any{}
					for _, m := range kept {
						if m.PersonRef != who {
							continue
						}
						one = append(one, m)
						part := m.Mode
						if m.Kind == "view" {
							part = "주관 기억"
						} else if part == "" {
							part = "그 밖의 사실"
						}
						if partOf[part] == nil {
							partOf[part] = map[string]any{"name": part, "count": 0}
							parts = append(parts, partOf[part])
						}
						partOf[part]["count"] = partOf[part]["count"].(int) + 1
					}
					kept = one
				}
			}
			offset, _ := strconv.Atoi(q.Get("offset"))
			limit, _ := strconv.Atoi(q.Get("limit"))
			if limit <= 0 || limit > memoriesPageMax {
				limit = memoriesPageMax
			}
			offset = min(max(offset, 0), len(kept))
			page := kept[offset:min(offset+limit, len(kept))]
			if page == nil {
				page = []store.ListedMemory{}
			}
			w.Header().Set("Content-Type", "application/json")
			kinds["person"] = people
			out := map[string]any{"chat": chat, "total": len(kept), "offset": offset, "kinds": kinds, "replaced": replaced, "memories": page}
			if kind == "person" {
				if whoList == nil {
					whoList = []map[string]any{}
				}
				out["people"] = whoList
				if parts != nil {
					out["parts"] = parts
				}
				if changes != nil {
					out["changes"] = changes
				}
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/hold"):
			// C18: {"chat_id","id","hold": "pin" | "mute" | ""}; a memory of
			// the chat's own turns, like an edit
			var req struct {
				ChatID string `json:"chat_id"`
				ID     string `json:"id"`
				Hold   string `json:"hold"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatID == "" || req.ID == "" {
				settingsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			loc, err := db.MemoryEditLocator("chat:"+req.ChatID, req.ID)
			changed := false
			if err == nil {
				changed, err = db.SetMemoryHold("chat:"+req.ChatID, req.ID, req.Hold)
			}
			switch {
			case errors.Is(err, store.ErrMemoryNotFound):
				settingsError(w, http.StatusNotFound, err.Error())
				return
			case errors.Is(err, store.ErrInheritedOnly):
				settingsError(w, http.StatusForbidden, err.Error())
				return
			case errors.Is(err, store.ErrBadHold):
				settingsError(w, http.StatusBadRequest, err.Error())
				return
			case err != nil:
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			if st != nil && changed {
				st.add(event{Kind: "memory_held", Chat: req.ChatID, Turn: loc.Turn, Detail: req.ID + " " + req.Hold})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "hold": req.Hold, "changed": changed})
		case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/edit") || strings.HasSuffix(r.URL.Path, "/delete")):
			var req struct {
				ChatID string            `json:"chat_id"`
				ID     string            `json:"id"`
				IDs    []string          `json:"ids"`
				Body   *string           `json:"body"`
				Imp    *float64          `json:"imp"`
				Patch  store.MemoryPatch `json:"patch"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatID == "" {
				settingsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			edit := strings.HasSuffix(r.URL.Path, "/edit")
			ids := []string{}
			seen := map[string]bool{}
			for _, id := range append([]string{req.ID}, req.IDs...) {
				if id != "" && !seen[id] {
					ids, seen[id] = append(ids, id), true
				}
			}
			if len(ids) == 0 || (edit && req.ID == "") {
				settingsError(w, http.StatusBadRequest, "bad_request")
				return
			}
			if edit {
				ids = []string{req.ID}
			}
			var all []store.ListedMemory
			var err error
			if edit {
				var loc store.DetailLocator
				loc, err = db.MemoryEditLocator("chat:"+req.ChatID, req.ID)
				if err == nil || errors.Is(err, store.ErrInheritedOnly) {
					all = []store.ListedMemory{{ID: req.ID, Turn: loc.Turn, Inherited: loc.Inherited}}
				}
			} else {
				all, err = list(req.ChatID, 0)
			}
			if err != nil && !errors.Is(err, store.ErrInheritedOnly) && !errors.Is(err, store.ErrMemoryNotFound) {
				storeFailed(w, 500, err)
				return
			}
			byID := map[string]store.ListedMemory{}
			for _, m := range all {
				byID[m.ID] = m
			}
			for _, id := range ids {
				m, ok := byID[id]
				if !ok {
					err = store.ErrMemoryNotFound
					break
				}
				if m.Inherited {
					err = store.ErrInheritedOnly
					break
				}
			}
			docChanged := !edit
			if err == nil {
				if edit {
					docChanged, err = db.EditMemoryPatch("chat:"+req.ChatID, req.ID, req.Body, req.Imp, req.Patch)
				} else {
					err = db.DeleteMemories("chat:"+req.ChatID, ids)
				}
			}
			switch {
			case errors.Is(err, store.ErrMemoryNotFound):
				settingsError(w, http.StatusNotFound, err.Error())
				return
			case errors.Is(err, store.ErrInheritedOnly):
				settingsError(w, http.StatusForbidden, err.Error())
				return
			case errors.Is(err, store.ErrEmptyBody), errors.Is(err, store.ErrBadImportance), errors.Is(err, store.ErrBadMemoryPatch):
				settingsError(w, http.StatusBadRequest, err.Error())
				return
			case err != nil:
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			kind := "memory_deleted"
			if edit {
				kind = "memory_edited"
			}
			if worker != nil && docChanged {
				worker.Reindex("chat:" + req.ChatID) // the turn's vectors are made again
			}
			if st != nil {
				for _, id := range ids {
					st.add(event{Kind: kind, Chat: req.ChatID, Turn: byID[id].Turn, Detail: id})
				}
			}
			// a turn left with none of its own memories is made again
			// (user 10-29: a deletion is seen and the turn regenerated)
			remade := []int{}
			if !edit {
				gone := map[string]bool{}
				for _, id := range ids {
					gone[id] = true
				}
				left := map[int]bool{}
				for _, m := range all {
					if !m.Inherited && !gone[m.ID] {
						left[m.Turn] = true
					}
				}
				var emptied []int
				for _, id := range ids {
					if t := byID[id].Turn; !left[t] && !slices.Contains(emptied, t) {
						emptied = append(emptied, t)
					}
				}
				if remade, err = remakeTurns(pipe, db, worker, req.ChatID, emptied); err != nil {
					storeFailed(w, http.StatusInternalServerError, err)
					return
				}
				if st != nil {
					for _, t := range remade {
						st.add(event{Kind: "turn_memory_changed", Chat: req.ChatID, Turn: t, Detail: "/memory/turn/remake"})
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "remade": remade})
		default:
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		}
	}
}
