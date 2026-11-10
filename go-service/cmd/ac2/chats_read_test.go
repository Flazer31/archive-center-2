package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/store"
)

func TestC26ChatReadHTTP(t *testing.T) {
	db, _, srcs := u3Fixture(t)
	u3Save(t, db, srcs[0], `{"entities":[{"name":"동명"}],"memories":[{"kind":"fact","body":"비밀 원문","hide_from":["동명"]},{"kind":"view","body":"개인 생각"}]}`)
	if err := db.SetChatLabel("chat:EMPTY", "", "empty"); err != nil {
		t.Fatal(err)
	}
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.Routes = map[string]http.HandlerFunc{"/chats/compare": chatCompareHandler(db)}
	var err error
	srv.Token, _, err = observe.LoadToken(filepath.Join(t.TempDir(), "synthetic-access-token"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := srv.Token.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/chats/compare?left_chat_id=U3&right_chat_id=EMPTY&left_limit=1"} {
		for _, auth := range []string{"", "wrong", token} {
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set(observe.TokenHeader, auth)
			w := httptest.NewRecorder()
			srv.Public().ServeHTTP(w, r)
			if auth != token {
				if w.Code != 401 || strings.Contains(w.Body.String(), "비밀") {
					t.Fatal("authentication failed", w.Code, w.Body.String())
				}
				continue
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var out map[string]any
			if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			left, right := out["left"].(map[string]any), out["right"].(map[string]any)
			if left["memories"].(map[string]any)["total"] != float64(2) || right["turns"] != float64(0) || left["next_memory_after"] == nil {
				t.Fatal(out)
			}
		}
	}
	for _, c := range []struct {
		method, path, code string
		status             int
	}{
		{"POST", "/chats/compare", "method_not_allowed", 405},
		{"GET", "/chats/compare?left_chat_id=U3", "chat_required", 400},
		{"GET", "/chats/compare?left_chat_id=U3&right_chat_id=EMPTY&left_limit=101", "bad_page", 400},
		{"GET", "/chats/compare?left_chat_id=U3&right_chat_id=EMPTY&right_turn_after=-1", "bad_page", 400},
		{"GET", "/chats/compare?left_chat_id=U3&right_chat_id=EMPTY&left_memory_after=bad", "bad_page", 400},
		{"GET", "/chats/compare?left_chat_id=U3&right_chat_id=MISSING", "chat_not_found", 404},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Header.Set(observe.TokenHeader, token)
		w := httptest.NewRecorder()
		srv.Public().ServeHTTP(w, r)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.code) || w.Header().Get("Content-Disposition") != "" {
			t.Fatalf("%s %s %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	// A SQL failure before streaming has a stable error, no SQL/DSN details.
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/chats/compare?left_chat_id=U3&right_chat_id=EMPTY"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set(observe.TokenHeader, token)
		srv.Public().ServeHTTP(w, r)
		if w.Code != 500 || !strings.Contains(w.Body.String(), "store_failed") || strings.Contains(w.Body.String(), "database") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
