package store

import (
	"fmt"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// Record actual SELECT counts on a single DB connection, without server-wide
// logging or counters affected by other tests. Each turn has one memory and
// no name references. This measures ChatList, not fixture setup or HTTP I/O.
func TestFix1ChatQueryGrowth(t *testing.T) {
	db := testDB(t)
	db.sql.SetMaxOpenConns(1)
	db.sql.SetMaxIdleConns(1)
	selects := func() int {
		t.Helper()
		var label string
		var n int
		if err := db.sql.QueryRow("SHOW SESSION STATUS LIKE 'Com_select'").Scan(&label, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, size := range [][2]int{{1, 1}, {1, 10}, {3, 10}, {5, 20}} {
		for c := 0; c < size[0]; c++ {
			key := fmt.Sprintf("chat:query-%d", c)
			s := lifecycle.NewSession(key)
			var rows []lifecycle.Row
			for i := 0; i < size[1]; i++ {
				rows = append(rows, lrow(fmt.Sprintf("u%d", i), "user", "", fmt.Sprintf("입력 %d", i)),
					lrow(fmt.Sprintf("a%d", i), "char", fmt.Sprintf("g%d", i), fmt.Sprintf("응답 %d", i)))
			}
			snap := lsnap(lifecycle.SourceLoad, rows...)
			snap.SessionKey = key
			s.Apply(snap)
			if len(s.ActiveTurns()) != size[1] {
				t.Fatalf("fixture turns=%d want=%d", len(s.ActiveTurns()), size[1])
			}
			if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
				t.Fatal(err)
			}
			for _, turn := range s.ActiveTurns() {
				j, err := db.JobFor(memory.SourceOf(key, turn))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.SaveGeneration(j, Generation{Output: memory.Output{Memories: []memory.Memory{{ID: "m1", Kind: "fact", Body: "기록"}}}}); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Repeat from the same stored fixture to check the measured count.
		for repeat := 0; repeat < 2; repeat++ {
			before := selects()
			chats, err := db.ChatList()
			n := selects() - before
			if err != nil || len(chats) != size[0] {
				t.Fatalf("chat list len=%d err=%v", len(chats), err)
			}
			for _, chat := range chats {
				if chat.Memories["fact"] != size[1] {
					t.Fatalf("count=%d want=%d", chat.Memories["fact"], size[1])
				}
			}
			t.Logf("chats=%d turns_per_chat=%d total_turns=%d read=%d selects=%d", size[0], size[1], size[0]*size[1], repeat+1, n)
		}
	}
}
