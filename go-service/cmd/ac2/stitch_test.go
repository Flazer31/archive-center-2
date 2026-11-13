package main

import (
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// pastChats is the order of the chats the chat's read scope reads before
// its own turns.
func pastChats(scope []memory.ScopedTurn) []string {
	var out []string
	for _, t := range scope {
		if !t.Past {
			break
		}
		if n := len(out); n == 0 || out[n-1] != t.Source.Session {
			out = append(out, t.Source.Session)
		}
	}
	return out
}

// C25 (AC1's stitch): stored chats attached in the order chosen are read
// in that order before the chat's own turns, the chat itself last; the
// order survives the request-identity cases and a restart; each chat is
// read once; deleting one leaves the others.
func TestStitchStoredChatsInOrder(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	for i, c := range []string{"A", "B"} {
		s := u2LoadHTTP(t, pipe, c, i+1)
		u2GenerateHTTP(t, db, s) // each: one turn, two memories
	}
	h := attachHandler(pipe, db, nil)
	both := attachResult{Chats: []attachedChat{{ChatID: "B", Turns: 1, Memories: 2}, {ChatID: "A", Turns: 1, Memories: 2}}, Turns: 2, Memories: 4}
	preview := attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["B","A","B"],"dry_run":true}`, 200)
	both.DryRun = true
	if !reflect.DeepEqual(preview, both) || attPastRows(t, db, "chat:X") != 0 {
		t.Fatalf("preview %+v", preview)
	}
	both.DryRun = false
	if got := attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["B","A"]}`, 200); !reflect.DeepEqual(got, both) {
		t.Fatalf("stitch %+v", got)
	}
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["A","X"]}`, 400)
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["A","none"]}`, 404)
	if got := attCall(t, h, "GET", "/chats/attach?chat_id=X", "", 200); !reflect.DeepEqual(got, both) {
		t.Fatalf("a refused request changed the past: %+v", got)
	}

	seq := 10
	apply := func(hook string, rows ...string) *lifecycle.Session {
		t.Helper()
		seq++
		if _, err := pipe.Process([]observe.Record{attEvent(seq, hook, "X", rows...)}); err != nil {
			t.Fatal(err)
		}
		var s *lifecycle.Session
		pipe.WithSession("chat:X", func(v *lifecycle.Session) { s = v })
		scope := memory.ScopeOf(s)
		if got := pastChats(scope); strings.Join(got, ",") != "chat:B,chat:A" {
			t.Fatalf("past order after %s: %v", hook, got)
		}
		return s
	}
	own := func(s *lifecycle.Session, n int) []*lifecycle.Turn {
		t.Helper()
		if got := s.ActiveTurns(); len(got) != n {
			t.Fatalf("own turns %d, want %d", len(got), n)
		}
		return s.ActiveTurns()
	}
	u1, a1 := memRow(0, "user", "u1", "", "안녕"), memRow(1, "char", "a1", "g1", "하나")
	u2, a2 := memRow(2, "user", "u2", "", "다음"), memRow(3, "char", "a2", "g2", "둘")
	own(apply("output", u1, a1), 1)         // a new turn appends
	own(apply("output", u1, a1, u2, a2), 2) // and another
	u3 := memRow(4, "user", "u3", "", "다음") // the same text in a new row
	own(apply("output", u1, a1, u2, a2, u3, memRow(5, "char", "a3", "g3", "셋")), 3)
	turns := own(apply("output", u1, a1, u2, a2, u3, memRow(5, "char", "a9", "g9", "셋 다시")), 3) // reroll replaces
	if r := turns[2].Responses; r[len(r)-1].Revisions[0].Data != "셋 다시" {
		t.Fatal("reroll did not replace")
	}
	u3e := memRow(4, "user", "u3", "", "다음 고침") // reply deleted, row edited, regenerated
	own(apply("beforeRequest", u1, a1, u2, a2, u3e), 3)
	own(apply("output", u1, a1, u2, a2, u3e, memRow(5, "char", "a10", "g10", "새로 만듦")), 3)
	a10, u4 := memRow(5, "char", "a10", "g10", "새로 만듦"), memRow(6, "user", "u4", "", "계속")
	turns = own(apply("beforeRequest", u1, a1, u2, a2, u3e, a10, u4), 4)
	turns = own(apply("beforeRequest", u1, a1, u2, a2, u3e, a10, u4), 4) // the same request again
	if !turns[2].Confirmed || turns[3].Confirmed {
		t.Fatal("retry changed confirmation")
	}
	// injection numbers the story in order: B's turn, A's turn, then X's
	var scope []memory.ScopedTurn
	pipe.WithSession("chat:X", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	d, err := db.Deliver(scope, store.DeliveryRequest{Input: "열었던 문을 다시 본다", Chars: 2000, Secret: 4000})
	if err != nil || !strings.Contains(d.Block, "(T1") || !strings.Contains(d.Block, "(T2") {
		t.Fatalf("both stitched chats are not read in story order: %v\n%s", err, d.Block)
	}
	// a restart reads the same order back
	pipe = pipeline.New(db)
	h = attachHandler(pipe, db, nil)
	if got := attCall(t, h, "GET", "/chats/attach?chat_id=X", "", 200); !reflect.DeepEqual(got, both) {
		t.Fatalf("after restart %+v", got)
	}
	// the other order is another past
	if got := attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["A","B"]}`, 200); got.Chats[0].ChatID != "A" || got.Chats[1].ChatID != "B" {
		t.Fatalf("reordered %+v", got)
	}
	// deleting A leaves B's part
	ops := chatOpsHandler(pipe, db, nil, nil)
	if p := u2Post(t, ops, "/chats/delete", `{"chat_id":"A","dry_run":true}`); strings.Join(p.PastReaders, ",") != "X" {
		t.Fatalf("readers %v", p.PastReaders)
	}
	u2Post(t, ops, "/chats/delete", `{"chat_id":"A"}`)
	if got := attCall(t, h, "GET", "/chats/attach?chat_id=X", "", 200); !reflect.DeepEqual(got, attOne("B", 1, 2, false)) {
		t.Fatalf("after deleting A %+v", got)
	}
}

// A stitched chat that already reads another stitched chat as its past
// carries that chat's turns too; each turn is read once, where it first
// comes, so the story numbers and days are not repeated.
func TestStitchOverlapReadsEachTurnOnce(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	for i, c := range []string{"A", "B"} {
		s := u2LoadHTTP(t, pipe, c, i+1)
		u2GenerateHTTP(t, db, s)
	}
	h := attachHandler(pipe, db, nil)
	attCall(t, h, "POST", "/chats/attach", `{"chat_id":"B","chats":["A"]}`, 200) // B reads A first
	want := attachResult{Chats: []attachedChat{{ChatID: "A", Turns: 1, Memories: 2}, {ChatID: "B", Turns: 1, Memories: 2}}, Turns: 2, Memories: 4}
	if got := attCall(t, h, "POST", "/chats/attach", `{"chat_id":"X","chats":["A","B"]}`, 200); !reflect.DeepEqual(got, want) {
		t.Fatalf("overlap %+v", got)
	}
	if n := attPastRows(t, db, "chat:X"); n != 2 {
		t.Fatalf("%d past rows, want 2", n)
	}
	var scope []memory.ScopedTurn
	pipe.WithSession("chat:X", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	d, err := db.Deliver(scope, store.DeliveryRequest{Input: "열었던 문을 다시 본다", Chars: 2000, Secret: 4000})
	if err != nil || !strings.Contains(d.Block, "(T2") || strings.Contains(d.Block, "(T3") {
		t.Fatalf("a turn read twice: %v\n%s", err, d.Block)
	}
}
