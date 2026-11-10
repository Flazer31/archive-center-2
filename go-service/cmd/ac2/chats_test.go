package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// GET /chats lists every chat with turns, named as the plugin sent the
// names with its snapshots (a name not sent keeps the one known), most
// recently observed first, with its counts and, for a branch, its parent.
func TestChatList(t *testing.T) {
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, _ := sql.Open("mysql", base)
	boot.Exec("DROP DATABASE IF EXISTS ac2_test_chats")
	if _, err := boot.Exec("CREATE DATABASE ac2_test_chats CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE ac2_test_chats"); boot.Close() })
	db, err := store.Open(base + "ac2_test_chats?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pipe := pipeline.New(db)
	labels := 0
	pipe.Label = func(key, ch, ct string) {
		labels++
		if err := db.SetChatLabel(key, ch, ct); err != nil {
			t.Fatal(err)
		}
	}
	row := func(i int, role, id, text string) string {
		b, _ := json.Marshal(map[string]any{"i": i, "role": role, "chat_id": id, "data": map[string]any{"text": text}})
		return string(b)
	}
	seq := 0
	load := func(chat, extra string, rows ...string) {
		seq++
		ev := fmt.Sprintf(`{"boot_id":"b","seq":%d,"hook":"load","stored_chat":{"chat_id":"%s"%s,"rows":[%s]}}`, seq, chat, extra, strings.Join(rows, ","))
		// received a second apart, so the list's order is the order observed
		recs := []observe.Record{{Event: json.RawMessage(ev), ReceivedAt: time.Date(2026, 10, 28, 0, 0, seq, 0, time.UTC).Format(time.RFC3339Nano), ReceiveSeq: uint64(seq)}}
		if _, err := (store.RawSink{DB: db}).Write(recs); err != nil {
			t.Fatal(err)
		}
		if _, err := pipe.Process(recs); err != nil {
			t.Fatal(err)
		}
	}
	load("A", `,"name":"첫 채팅","char_name":"리나"`, row(0, "user", "u1", "하나"), row(1, "char", "a1", "첫째"), row(2, "user", "u2", "둘"), row(3, "char", "a2", "둘째"))
	load("B", `,"name":"두 번째"`, row(0, "user", "v1", "셋"), row(1, "char", "b1", "셋째"))
	// the same names again are not passed on; a missing character name keeps the known one
	load("A", `,"name":"첫 채팅","char_name":"리나"`, row(0, "user", "u1", "하나"), row(1, "char", "a1", "첫째"), row(2, "user", "u2", "둘"), row(3, "char", "a2", "둘째"))
	load("A", `,"name":"이름 바꿈"`, row(0, "user", "u1", "하나"), row(1, "char", "a1", "첫째"), row(2, "user", "u2", "둘"), row(3, "char", "a2", "둘째"))
	if labels != 3 {
		t.Fatalf("labels passed on %d times, want 3", labels)
	}

	rec := httptest.NewRecorder()
	chatsHandler(db)(rec, httptest.NewRequest(http.MethodGet, "/chats", nil))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Chats []struct {
			ChatID   string         `json:"chat_id"`
			CharName string         `json:"char_name"`
			ChatName string         `json:"chat_name"`
			Turns    int            `json:"turns"`
			Memories map[string]int `json:"memories"`
			LastAt   *string        `json:"last_at"`
			BranchOf any            `json:"branch_of"`
		} `json:"chats"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Chats) != 2 {
		t.Fatalf("chats %s", rec.Body.String())
	}
	a, b := out.Chats[0], out.Chats[1]
	if a.ChatID != "A" || b.ChatID != "B" {
		t.Fatalf("A was observed last and comes first: %s", rec.Body.String())
	}
	if a.CharName != "리나" || a.ChatName != "이름 바꿈" || b.CharName != "" || b.ChatName != "두 번째" {
		t.Fatalf("names %+v %+v", a, b)
	}
	if a.Turns != 2 || b.Turns != 1 || a.LastAt == nil || a.Memories["total"] != 0 || a.BranchOf != nil {
		t.Fatalf("counts %+v %+v", a, b)
	}
}
