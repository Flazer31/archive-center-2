package store

// Upgrade test 11-06 (20261106-ac2-upgrade-from-003): an index run commits
// doc_hash on memories while management writes run. A C16 edit takes its row
// FOR UPDATE first and succeeds; the unmerge failed (1020) and now runs at the
// worker's between-runs point (cmd/ac2 TestC17MergeAndUnmergeWaitForWorkerRuns).

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"archivecenter2/goservice/internal/memory"
)

func TestEditSucceedsWhileIndexWritesDocHashes(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:index-edit", Turn: 1}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"Alpha"}],"memories":[{"id":"m1","kind":"fact","body":"synthetic body","about":"n1","mode":"old"}]}`)
	ids, err := db.ItemIDs(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := ids["memories/0"]
	stop, runs, written := indexLikeWriter(t, db, []string{id})
	_, editErr := db.EditMemoryPatch(src.Session, id, nil, nil, MemoryPatch{"mode": json.RawMessage(`"new"`)})
	stop()
	if editErr != nil {
		t.Fatalf("edit during %d index batches: %v", runs(), editErr)
	}
	// the writer really rewrote the row (review 03: a no-op writer also passed)
	if written() == 0 {
		t.Fatalf("the index-like writer changed no row in %d batches", runs())
	}
	if got := c16Memory(t, db, id); got.Mode != "new" {
		t.Fatalf("mode after edit = %q", got.Mode)
	}
}

// indexLikeWriter writes new doc hashes for the memories in back-to-back
// transactions, each held a moment as a slow index batch does.
func indexLikeWriter(t *testing.T, db *DB, mems []string) (stop func(), runs, written func() int64) {
	var halt atomic.Bool
	var wg sync.WaitGroup
	var n, rows atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !halt.Load(); i++ {
			tx, err := db.sql.Begin()
			if err != nil {
				t.Error(err)
				return
			}
			for _, id := range mems {
				res, err := tx.Exec(`UPDATE memories SET doc_hash=? WHERE id=?`, fmt.Sprintf("index-%d", i), memoryRowNumber(id))
				if err != nil {
					t.Error(err)
				} else if k, _ := res.RowsAffected(); k > 0 {
					rows.Add(k)
				}
			}
			time.Sleep(150 * time.Millisecond)
			if err := tx.Commit(); err != nil {
				t.Error(err)
			}
			n.Add(1)
		}
	}()
	time.Sleep(50 * time.Millisecond) // the first index batch holds the rows
	return func() { halt.Store(true); wg.Wait() }, n.Load, rows.Load
}
