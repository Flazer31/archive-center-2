package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// attEvent is one plugin observation of a chat: beforeRequest and load
// carry the stored chat, output its event chat.
func attEvent(seq int, hook, chat string, rows ...string) observe.Record {
	field := "stored_chat"
	if hook == "output" {
		field = "event_chat"
	}
	return observe.Record{Event: json.RawMessage(fmt.Sprintf(`{"boot_id":"b","seq":%d,"hook":%q,%q:{"chat_id":%q,"rows":[%s]}}`, seq, hook, field, chat, strings.Join(rows, ",")))}
}

func attCall(t *testing.T, h http.HandlerFunc, method, path, body string, code int) attachResult {
	t.Helper()
	r := httptest.NewRecorder()
	h(r, httptest.NewRequest(method, path, strings.NewReader(body)))
	if r.Code != code {
		t.Fatalf("%s %s %s: %d %s", method, path, body, r.Code, r.Body.String())
	}
	var out attachResult
	json.Unmarshal(r.Body.Bytes(), &out)
	return out
}

// attOne is the attach result of one chat's past.
func attOne(chat string, turns, memories int, dry bool) attachResult {
	return attachResult{Chats: []attachedChat{{ChatID: chat, Turns: turns, Memories: memories}}, Turns: turns, Memories: memories, DryRun: dry}
}

func attPastRows(t *testing.T, _ *store.DB, key string) int {
	t.Helper()
	conn, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM session_past WHERE session_key=?`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// C22: a stored chat attached to a chat is read before the chat's own
// turns, survives a restart, and is not a host observation — the five
// request-identity cases replace and append as without it, and deleting
// the host's rows never deletes the past.
func TestAttachStoredChatAsPast(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	y := u2LoadHTTP(t, pipe, "Y", 1)
	u2GenerateHTTP(t, db, y) // Y's one turn: two memories
	h := attachHandler(pipe, db, nil)

	if got := attCall(t, h, "GET", "/chats/attach?chat_id=X", "", 200); len(got.Chats) != 0 || got.Turns != 0 {
		t.Fatalf("nothing attached yet: %+v", got)
	}
	attCall(t, h, "GET", "/chats/attach", "", 400)
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["X"]}`, 400)
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X"}`, 400)
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["none"]}`, 404)
	preview := attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["Y"],"dry_run":true}`, 200)
	if !reflect.DeepEqual(preview, attOne("Y", 1, 2, true)) || attPastRows(t, db, "chat:X") != 0 {
		t.Fatalf("preview %+v wrote %d rows", preview, attPastRows(t, db, "chat:X"))
	}
	if got := attCall(t, h, "GET", "/chats/attach?chat_id=X", "", 200); len(got.Chats) != 0 {
		t.Fatalf("preview attached: %+v", got)
	}
	if got := attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["Y"]}`, 200); !reflect.DeepEqual(got, attOne("Y", 1, 2, false)) {
		t.Fatalf("attach %+v", got)
	}

	// a restart before the host showed any turn of X reads the past back
	pipe = pipeline.New(db)
	h = attachHandler(pipe, db, nil)
	if got := attCall(t, h, "GET", "/chats/attach?chat_id=X", "", 200); !reflect.DeepEqual(got, attOne("Y", 1, 2, false)) {
		t.Fatalf("after restart %+v", got)
	}
	seq := 10
	apply := func(hook string, rows ...string) *lifecycle.Session {
		t.Helper()
		seq++
		if _, err := pipe.Process([]observe.Record{attEvent(seq, hook, "X", rows...)}); err != nil {
			t.Fatal(err)
		}
		var s *lifecycle.Session
		if err := pipe.WithSession("chat:X", func(v *lifecycle.Session) { s = v }); err != nil {
			t.Fatal(err)
		}
		// the past stays first in what X reads, whatever the host did
		scope := memory.ScopeOf(s)
		if len(s.Past) != 1 || len(scope) == 0 || !scope[0].Past || scope[0].Source.Session != "chat:Y" {
			t.Fatalf("past lost after %s: %+v", hook, s.Past)
		}
		k, err := db.CurrentKnown(s)
		if err != nil {
			t.Fatal(err)
		}
		past := 0
		for _, m := range k.Memories {
			if m.Turn == 1 && strings.Contains(m.Body, "문") {
				past++
			}
		}
		if past == 0 {
			t.Fatalf("X does not read Y's memory after %s: %+v", hook, k.Memories)
		}
		return s
	}
	own := func(s *lifecycle.Session, n int) []*lifecycle.Turn {
		t.Helper()
		got := s.ActiveTurns()
		if len(got) != n {
			t.Fatalf("own turns %d, want %d", len(got), n)
		}
		return got
	}
	last := func(tn *lifecycle.Turn) string {
		for i := len(tn.Responses) - 1; i >= 0; i-- {
			if r := tn.Responses[i]; r.State == lifecycle.Active {
				return r.Revisions[len(r.Revisions)-1].Data
			}
		}
		return ""
	}
	u1, a1 := memRow(0, "user", "u1", "", "안녕"), memRow(1, "char", "a1", "g1", "하나")
	u2, a2 := memRow(2, "user", "u2", "", "다음"), memRow(3, "char", "a2", "g2", "둘")
	// normal new turn: appends
	own(apply("output", u1, a1), 1)
	own(apply("output", u1, a1, u2, a2), 2)
	// the same text in a new host row: a new turn
	u3 := memRow(4, "user", "u3", "", "다음")
	own(apply("output", u1, a1, u2, a2, u3, memRow(5, "char", "a3", "g3", "셋")), 3)
	// reroll of the same host user row: replaces
	turns := own(apply("output", u1, a1, u2, a2, u3, memRow(5, "char", "a9", "g9", "셋 다시")), 3)
	if last(turns[2]) != "셋 다시" {
		t.Fatalf("reroll did not replace: %q", last(turns[2]))
	}
	// reply deleted, the same user row edited, regenerated: replaces
	u3e := memRow(4, "user", "u3", "", "다음 고침")
	own(apply("beforeRequest", u1, a1, u2, a2, u3e), 3)
	turns = own(apply("output", u1, a1, u2, a2, u3e, memRow(5, "char", "a10", "g10", "새로 만듦")), 3)
	if last(turns[2]) != "새로 만듦" || turns[2].Members[0].Data != "다음 고침" {
		t.Fatalf("edit and regenerate did not replace: %q %q", last(turns[2]), turns[2].Members[0].Data)
	}
	// the next input confirms; the same request again (provider retry) adds
	// nothing and confirms nothing more
	a10 := memRow(5, "char", "a10", "g10", "새로 만듦")
	u4 := memRow(6, "user", "u4", "", "계속")
	turns = own(apply("beforeRequest", u1, a1, u2, a2, u3e, a10, u4), 4)
	confirmed := turns[2].Confirmed
	turns = own(apply("beforeRequest", u1, a1, u2, a2, u3e, a10, u4), 4)
	if !confirmed || turns[2].Confirmed != confirmed || turns[3].Confirmed {
		t.Fatal("retry changed confirmation")
	}
	// injection reads the past as the story's first turn, before X's own
	var scope []memory.ScopedTurn
	pipe.WithSession("chat:X", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	d, err := db.Deliver(scope, store.DeliveryRequest{Input: "열었던 문을 다시 본다", Chars: 2000, Secret: 4000})
	if err != nil || !strings.Contains(d.Block, "(T1") || !strings.Contains(d.Block, "문을 열었다") {
		t.Fatalf("delivery without the past: %v\n%s", err, d.Block)
	}
	// a restart with X's own turns stored reads the past back too
	pipe = pipeline.New(db)
	h = attachHandler(pipe, db, nil)
	if got := attCall(t, h, "GET", "/chats/attach?chat_id=X", "", 200); !reflect.DeepEqual(got, attOne("Y", 1, 2, false)) {
		t.Fatalf("after restart with turns %+v", got)
	}
	// the host deletes every row of X: X's own turns go, the past and Y stay
	own(apply("load"), 0)
	if ys, ok, err := db.LoadSession("chat:Y"); err != nil || !ok || len(ys.ActiveTurns()) != 1 {
		t.Fatal("Y changed with X's host rows")
	}

	// deleting Y names X and takes the attachment with it; X keeps its own state
	ops := chatOpsHandler(pipe, db, nil, nil)
	if p := u2Post(t, ops, "/chats/delete", `{"chat_id":"Y","dry_run":true}`); len(p.PastReaders) != 1 || p.PastReaders[0] != "X" || attPastRows(t, db, "chat:X") != 1 {
		t.Fatalf("delete preview %+v", p)
	}
	u2Post(t, ops, "/chats/delete", `{"chat_id":"Y"}`)
	if attPastRows(t, db, "chat:X") != 0 {
		t.Fatal("attachment to a deleted chat kept")
	}
	if err := pipe.WithSession("chat:X", func(s *lifecycle.Session) {
		if len(s.Past) != 0 {
			t.Fatal("cached past of a deleted chat")
		}
	}); err != nil {
		t.Fatal(err)
	}

	// attach again from another chat, then detach
	z := u2LoadHTTP(t, pipe, "Z", 100)
	u2GenerateHTTP(t, db, z)
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["Z"]}`, 200)
	if got := attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","detach":true}`, 200); len(got.Chats) != 0 || got.Turns != 0 || attPastRows(t, db, "chat:X") != 0 {
		t.Fatalf("detach %+v", got)
	}
	// deleting a chat removes its own attachment rows
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["Z"]}`, 200)
	u2Post(t, ops, "/chats/delete", `{"chat_id":"X"}`)
	if attPastRows(t, db, "chat:X") != 0 {
		t.Fatal("deleted chat kept its attachment")
	}
}
