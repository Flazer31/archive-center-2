package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

const (
	// worldlineRecent is how many of the newest turns the map always shows.
	worldlineRecent = 20
	// worldlineOpenMax is the most turns one opened range shows (the screen
	// opens a fold this many turns at a time).
	worldlineOpenMax = 50
	// previewRunes is the length of a turn's texts on the map.
	previewRunes = 80
)

// wlNode is a turn on the map, or a folded range of turns (Fold).
type wlNode struct {
	Turn      int        `json:"turn,omitempty"`
	User      string     `json:"user,omitempty"`
	Reply     string     `json:"reply,omitempty"`
	Inherited bool       `json:"inherited,omitempty"` // the parent chat's turn, copied
	Off       bool       `json:"off,omitempty"`       // turned off in the host
	Versions  int        `json:"versions,omitempty"`  // other versions (rerolls, edits)
	Memories  int        `json:"memories,omitempty"`
	Branches  []wlBranch `json:"branches,omitempty"` // chats branched after this turn
	Fold      *[2]int    `json:"fold,omitempty"`     // first and last folded turn
}

type wlBranch struct {
	Chat  string `json:"chat"`
	Turns int    `json:"turns"`
}

type wlVersion struct {
	Kind string `json:"kind"` // reroll | edit
	Text string `json:"text"`
}

// worldlineHandler is the worldline (step 9): a chat's turns as a map
// growing from turn 1, with the chats branched from it and the chat it
// branched from.
//   - GET /worldline?chat_id=&open=a-b : the map. The first turn, the newest
//     worldlineRecent turns, the turns where a branch splits and one opened
//     range (its newest worldlineOpenMax turns) are turns; the rest is folded
//     into ranges, so a long chat stays a short map however far it is opened
//     (user decision 10-28, for the browser's memory).
//   - GET /worldline/turn?chat_id=&turn= : one turn's texts and its other
//     versions.
//
// Turns are numbered as the memory screen numbers them (memory.Own). What
// the answer shows of the chat is read under the session's lock, so one
// answer is one observation of it.
func worldlineHandler(pipe *pipeline.Pipeline, db *store.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			settingsError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		q := r.URL.Query()
		chat := q.Get("chat_id")
		if chat == "" {
			settingsError(w, http.StatusBadRequest, "chat_required")
			return
		}
		key := "chat:" + chat
		detail := strings.HasSuffix(r.URL.Path, "/turn")
		var children []store.Branch
		var err error
		if !detail {
			if children, err = db.BranchesOf(key); err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
		}
		n, _ := strconv.Atoi(q.Get("turn"))
		var own []memory.ScopedTurn
		var parent [2]string
		var nodes []wlNode // turns shown, and folds
		var start []wlBranch
		var one map[string]any // the turn asked for
		err = pipe.WithSession(key, func(s *lifecycle.Session) {
			turns := s.ActiveTurns()
			own, parent = memory.Own(s), s.BranchOf
			if detail {
				if n >= 1 && n <= len(turns) {
					user, reply, others := turnTexts(turns[n-1])
					one = map[string]any{"chat": chat, "turn": n, "user": user, "reply": reply,
						"inherited": own[n-1].Inherited, "off": turns[n-1].Off, "versions": others}
				}
				return
			}
			shown := map[int]bool{1: true}
			for i := max(1, len(turns)-worldlineRecent+1); i <= len(turns); i++ {
				shown[i] = true
			}
			branches := map[int][]wlBranch{}
			for _, c := range children {
				b := wlBranch{Chat: strings.TrimPrefix(c.Key, "chat:"), Turns: c.Turns}
				at := forkPlace(s, c.Message)
				if at == 0 { // branched before the first turn (the greeting), or where the chat does not show
					start = append(start, b)
					continue
				}
				branches[at] = append(branches[at], b)
				shown[at] = true
			}
			for i, t := range own { // where this chat leaves its parent
				if t.Inherited && (i+1 == len(own) || !own[i+1].Inherited) {
					shown[i+1], shown[i+2] = true, true
				}
			}
			if a, b, ok := strings.Cut(q.Get("open"), "-"); ok {
				lo, err1 := strconv.Atoi(a)
				hi, err2 := strconv.Atoi(b)
				if err1 == nil && err2 == nil {
					for i := max(lo, hi-worldlineOpenMax+1); i <= hi; i++ {
						shown[i] = true
					}
				}
			}
			fold := 0 // first turn of the fold being gathered
			for i := 1; i <= len(turns); i++ {
				if !shown[i] {
					if fold == 0 {
						fold = i
					}
					continue
				}
				if fold > 0 {
					nodes = append(nodes, wlNode{Fold: &[2]int{fold, i - 1}})
					fold = 0
				}
				user, reply, others := turnTexts(turns[i-1])
				nodes = append(nodes, wlNode{Turn: i, User: preview(user), Reply: preview(reply), Inherited: own[i-1].Inherited,
					Off: turns[i-1].Off, Versions: len(others), Branches: branches[i]})
			}
		})
		if err != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		}
		if detail {
			if one == nil {
				settingsError(w, http.StatusNotFound, "turn_not_found")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(one)
			return
		}

		count, err := db.MemoryCounts(own)
		if err != nil {
			storeFailed(w, http.StatusInternalServerError, err)
			return
		}
		for i := range nodes {
			nodes[i].Memories = count[nodes[i].Turn]
		}
		var from map[string]any
		if parent[0] != "" {
			at := 0
			if err := pipe.WithSession(parent[0], func(p *lifecycle.Session) { at = forkPlace(p, parent[1]) }); err != nil {
				storeFailed(w, http.StatusInternalServerError, err)
				return
			}
			from = map[string]any{"chat": strings.TrimPrefix(parent[0], "chat:"), "turn": at}
		}
		if nodes == nil {
			nodes = []wlNode{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"chat": chat, "turns": len(own), "parent": from,
			"start_branches": start, "nodes": nodes})
	}
}

// forkPlace is the place, among a chat's active turns, of the turn holding
// a row: a branch made at that row splits after it. A row of a turn deleted
// since is placed at the active turn before it; the greeting, or a row the
// chat does not hold, is 0.
func forkPlace(s *lifecycle.Session, row string) int {
	place := 0
	for _, t := range s.Turns {
		if !t.Greeting && !t.Inactive {
			place++
		}
		for _, m := range t.Members {
			if m.Key == row {
				return place
			}
		}
		for _, r := range t.Responses {
			if r.Key == row {
				return place
			}
		}
	}
	return 0
}

// turnTexts are a turn's input and current reply as the host holds them,
// and the reply's other versions: the replies a reroll replaced and the
// earlier texts of a reply (an edit, or a reroll in the same row), each
// text once.
func turnTexts(t *lifecycle.Turn) (user, reply string, others []wlVersion) {
	var in, out []string
	for _, m := range t.Members {
		if m.State == lifecycle.Active {
			in = append(in, m.Data)
		}
	}
	seen := map[string]bool{}
	for _, r := range t.Responses {
		if r.State == lifecycle.Active && len(r.Revisions) > 0 {
			cur := r.Revisions[len(r.Revisions)-1].Data
			out = append(out, cur)
			seen[cur] = true
		}
	}
	for _, r := range t.Responses {
		if r.State == lifecycle.Deleted {
			continue
		}
		n := len(r.Revisions)
		for j, rev := range r.Revisions {
			if (r.State == lifecycle.Active && j == n-1) || seen[rev.Data] {
				continue
			}
			seen[rev.Data] = true
			kind := "edit"
			if r.State == lifecycle.Replaced || (rev.GenerationID != "" && rev.GenerationID != r.Revisions[n-1].GenerationID) {
				kind = "reroll"
			}
			others = append(others, wlVersion{Kind: kind, Text: rev.Data})
		}
	}
	if others == nil {
		others = []wlVersion{}
	}
	return strings.Join(in, "\n"), strings.Join(out, "\n"), others
}

func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > previewRunes {
		return string(r[:previewRunes]) + "…"
	}
	return s
}
