package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archivecenter2/goservice/internal/store"
)

// GET /chats: the chat list (user decisions 10-28), most recent first. Each
// chat: its host names (empty until the plugin sends them), turn, memory
// and name counts, its last observation and, for a branch, the chat and
// turn it leaves from.
func chatsHandler(db *store.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		list, err := db.ChatList()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		chats := make([]map[string]any, 0, len(list))
		for _, c := range list {
			total := 0
			for _, n := range c.Memories {
				total += n
			}
			v := map[string]any{"chat_id": strings.TrimPrefix(c.Key, "chat:"), "char_name": c.CharName, "chat_name": c.ChatName,
				"turns": c.Turns, "memories": map[string]int{"event": c.Memories["event"], "fact": c.Memories["fact"], "view": c.Memories["view"], "total": total},
				"names": c.Names, "last_at": nil, "branch_of": nil}
			if !c.LastAt.IsZero() {
				v["last_at"] = c.LastAt.UTC().Format(time.RFC3339)
			}
			if c.ParentKey != "" {
				v["branch_of"] = map[string]any{"chat_id": strings.TrimPrefix(c.ParentKey, "chat:"), "turn": c.ParentTurn}
			}
			chats = append(chats, v)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"chats": chats})
	}
}

func chatCompareHandler(db *store.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			settingsError(w, 405, "method_not_allowed")
			return
		}
		q := r.URL.Query()
		left, right := q.Get("left_chat_id"), q.Get("right_chat_id")
		if left == "" || right == "" {
			settingsError(w, 400, "chat_required")
			return
		}
		var pages [2]store.ComparePage
		for i, prefix := range []string{"left_", "right_"} {
			pages[i].Limit = 10
			for _, field := range []string{"turn_after", "memory_after", "limit"} {
				raw := q.Get(prefix + field)
				if raw == "" {
					continue
				}
				n, err := strconv.ParseInt(raw, 10, 64)
				if err != nil || n < 0 || (field == "limit" && (n < 1 || n > 100)) || int64(int(n)) != n {
					settingsError(w, 400, "bad_page")
					return
				}
				switch field {
				case "turn_after":
					pages[i].TurnAfter = int(n)
				case "memory_after":
					pages[i].MemoryAfter = n
				case "limit":
					pages[i].Limit = int(n)
				}
			}
		}
		v, err := db.CompareChats(r.Context(), "chat:"+left, "chat:"+right, pages)
		if err != nil {
			chatReadError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"left": v[0], "right": v[1]})
	}
}
func chatReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrChatNotFound) {
		settingsError(w, 404, "chat_not_found")
		return
	}
	settingsError(w, 500, "store_failed")
}
