package store

import (
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

func u3Stored(t *testing.T, db *DB, src memory.Source, raw string) Job {
	t.Helper()
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	out, notes := memory.Parse(raw)
	if _, err := db.SaveGeneration(j, Generation{Request: "synthetic", ReadItems: "[]", Response: raw, Output: out, Notes: notes}); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestU3UserAliasKeepsScopedLookup(t *testing.T) {
	db := testDB(t)
	src1 := memory.Source{Session: "chat:names", Turn: 1, Members: []memory.SourceMember{{Data: "one"}}, Responses: []memory.SourceResponse{{Seq: 1, Data: "first"}}}
	src2 := src1
	src2.Turn = 2
	j1 := u3Stored(t, db, src1, `{"entities":[{"id":"n1","name":"옛이름","type":"person"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"약사다"}]}`)
	j2 := u3Stored(t, db, src2, `{"entities":[{"id":"n1","name":"새이름","type":"person"},{"id":"n2","name":"아직 모를 변장","same_as":"n1","how":"disguise"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"미래","body":"범위 밖 비밀"}]}`)
	old, _ := db.ItemIDs(j1.ID)
	keep, _ := db.ItemIDs(j2.ID)
	child := src1
	child.Session = "chat:child"
	j3 := u3Stored(t, db, child, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"view","owner":%q,"body":"기억한다"}]}`, old["entities/0"]))
	if _, err := db.MergeNames(src1.Session, keep["entities/0"], old["entities/0"]); err != nil {
		t.Fatal(err)
	}
	// A branch that copied only turn 1 gets the managed names but no future
	// memory, quote or model-declared identity relation from turn 2.
	k, err := db.KnownFor([]memory.Source{src1})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Memories) != 1 || k.Memories[0].ID != old["memories/0"] {
		t.Fatalf("scope leaked: %+v", k)
	}
	for _, n := range k.Names {
		if n.Name == "아직 모를 변장" {
			t.Fatalf("future model alias leaked: %+v", k)
		}
	}
	for _, surface := range []string{"옛이름", "새이름"} {
		sel := memory.Select(k, surface, nil)
		if len(sel.Memories) != 1 || sel.Memories[0].ID != old["memories/0"] {
			t.Fatalf("%s lost scoped lookup: %+v", surface, sel)
		}
	}
	scope := []memory.ScopedTurn{{Source: src1}}
	delivery, err := db.Deliver(scope, DeliveryRequest{Input: "새이름", Payload: []TextPrint{{Role: "user"}}, Chars: 1000, Secret: 1000})
	if err != nil || !strings.Contains(delivery.Block, "새이름") || strings.Contains(delivery.Block, "범위 밖 비밀") {
		t.Fatalf("delivery %+v %v", delivery, err)
	}
	// Child-owned references move too; another unrelated name does not.
	it, err := db.ItemsOf([]int64{j3.ID})
	if err != nil || it.Memories[0].Owner != keep["entities/0"] {
		t.Fatalf("child alias %+v %v", it, err)
	}
}

func TestU3ModelIdentityDoesNotRewriteNewReferences(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:identity", Turn: 1, Responses: []memory.SourceResponse{{Seq: 1, Data: "reply"}}}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"주인공"},{"id":"n2","name":"가면 이름","same_as":"n1","how":"alias","unaware":["other"]}]}`)
	ids, _ := db.ItemIDs(j.ID)
	src.Turn++
	later := u3Stored(t, db, src, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"view","owner":%q,"body":"가면의 시점"}]}`, ids["entities/1"]))
	it, err := db.ItemsOf([]int64{later.ID})
	if err != nil || len(it.Memories) != 1 || it.Memories[0].Owner != ids["entities/1"] {
		t.Fatalf("model assertion became a user merge: %+v %v", it, err)
	}
}

func TestU3DeleteMemoriesRollsBackAndDetachesVectors(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:bulk", Turn: 1, Responses: []memory.SourceResponse{{Seq: 1, Data: "reply"}}}
	j := u3Stored(t, db, src, `{"memories":[{"id":"m1","kind":"event","body":"first","effects":[{"slot":"x","value":"y"}],"unaware":["somebody"]},{"id":"m2","kind":"fact","body":"second"},{"id":"m3","kind":"fact","body":"kept"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	if _, err := db.sql.Exec(`UPDATE memories SET doc_hash=? WHERE job_id=?`, strings.Repeat("a", 64), j.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteMemories(src.Session, []string{ids["memories/0"], "M999999999"}); err != ErrMemoryNotFound {
		t.Fatalf("missing memory: %v", err)
	}
	it, err := db.ItemsOf([]int64{j.ID})
	if err != nil || len(it.Memories) != 3 || len(it.Memories[0].Effects) != 1 || len(it.Awareness) != 1 {
		t.Fatalf("partial delete %+v %v", it, err)
	}
	if err := db.DeleteMemories(src.Session, []string{ids["memories/0"], ids["memories/1"]}); err != nil {
		t.Fatal(err)
	}
	it, err = db.ItemsOf([]int64{j.ID})
	if err != nil || len(it.Memories) != 1 || it.Memories[0].Body != "kept" || len(it.Awareness) != 0 {
		t.Fatalf("delete %+v %v", it, err)
	}
	var hash string
	if err := db.sql.QueryRow(`SELECT doc_hash FROM memories WHERE job_id=?`, j.ID).Scan(&hash); err != nil || hash != "" {
		t.Fatalf("stale vector %q %v", hash, err)
	}
}
