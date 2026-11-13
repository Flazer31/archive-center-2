package store

import (
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// A run drops the grant of a turn's earlier text while an edit of the turn
// is being saved: the drop waits for the save, which moved the grant to the
// edited text, and the grant stays.
func TestRegrantHoldsAgainstADrop(t *testing.T) {
	db := testDB(t)
	snap := func(text string) lifecycle.Snapshot {
		return lifecycle.Snapshot{SessionKey: "chat:RG", Source: lifecycle.SourceLoad, TargetIndex: -1, Rows: []lifecycle.Row{
			{Index: 0, Role: "user", ChatID: "u1", Data: text}, {Index: 1, Role: "char", ChatID: "a1", Data: "요약", GenerationID: "g1"}}}
	}
	s := lifecycle.NewSession("chat:RG")
	s.Apply(snap("A"))
	if err := db.SaveSession(s, nil, nil); err != nil {
		t.Fatal(err)
	}
	a := memory.SourceOf(s.Key, s.ActiveTurns()[0])
	j, err := db.JobFor(a)
	if err != nil {
		t.Fatal(err)
	}
	out, notes := memory.Parse(`{"memories":[{"id":"m1","kind":"fact","body":"기록"}]}`)
	if _, err := db.SaveGeneration(j, Generation{Request: "r", ReadItems: "[]", Response: "{}", Output: out, Notes: notes}); err != nil {
		t.Fatal(err)
	}
	if err := db.Grant(s.Key, []memory.Source{a}); err != nil {
		t.Fatal(err)
	}
	// the edit's save has read the grants; the run drops A's grant meanwhile
	s.Apply(snap("B"))
	b := memory.SourceOf(s.Key, s.ActiveTurns()[0])
	if b.Key() == a.Key() {
		t.Fatal("fixture: the edit keeps the text")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	asked, err := grantsToMove(tx, s.Key)
	if err != nil {
		t.Fatal(err)
	}
	dropped := make(chan error, 1)
	go func() { dropped <- db.Ungrant(s.Key, a) }()
	time.Sleep(200 * time.Millisecond)
	if err := moveGrants(tx, s, asked); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-dropped; err != nil {
		t.Fatal(err)
	}
	var key string
	if err := db.sql.QueryRow(`SELECT source_key FROM gen_grants WHERE session_key=? AND turn_id=?`, s.Key, b.Turn).Scan(&key); err != nil || key != b.Key() {
		t.Fatalf("the grant after the edit and the drop: %q %v, want the edited text", key, err)
	}
}
