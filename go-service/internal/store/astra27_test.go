package store

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"strings"
	"testing"
)

func TestA27ScalarDoesNotStoreInventedName(t *testing.T) {
	db := testDB(t)
	raw := `{"entities":[{"id":"n1","name":"민재"},17],"memories":[{"id":"m1","body":"사실"}]}`
	out, notes := memory.Parse(raw)
	job, err := db.JobFor(turnSource(twoTurnSession(), 0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SaveGeneration(job, Generation{Request: "synthetic", Response: raw, Output: out, Notes: notes})
	if err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf([]int64{job.ID})
	if err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := db.sql.QueryRow("SELECT response FROM gen_jobs WHERE job_id=?", job.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Logf("names=%+v memories=%+v notes=%v raw_preserved=%v", it.Names, it.Memories, notes, saved == raw)
	if saved != raw || len(it.Names) != 1 || len(it.Memories) != 1 {
		t.Fatal("numeric array entry created a canonical empty name")
	}
}

func TestA27StoredEffectProtectionNames(t *testing.T) {
	db := testDB(t)
	j, _, _ := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"entities":[{"id":"n1","name":"민재"},{"id":"n2","name":"서진"}],"memories":[{"id":"m1","kind":"event","body":"몰래 받았다","effects":[{"about":"n1","slot":"돈","value":"100냥"}],"hide_from":["n2"]}]}`)
	k, err := db.KnownOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	r := memory.RenderKnown(memory.Select(k, "민재가 웃었다", nil), memory.Superseded(k))
	t.Log(r)
	if len(k.Names) != 2 || len(k.Awareness) != 1 {
		t.Fatal("fixture not stored")
	}
	if !strings.Contains(r, "서진") {
		t.Fatal("stored protection target name lost at next input")
	}
}

func TestA27CatchupReloadJobIdentity(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	snap := func(src lifecycle.Source, gen, data string, unread bool) lifecycle.Snapshot {
		return lifecycle.Snapshot{SessionKey: s.Key, Source: src, TargetIndex: 1, Rows: []lifecycle.Row{{Index: 0, ChatID: "u", Role: "user", Data: "입력"}, {Index: 1, ChatID: "a", Role: "char", GenerationID: gen, Data: data, Unreadable: unread}}}
	}
	s.Apply(snap(lifecycle.SourceOutputEvent, "g1", "같은 본문", false))
	j1, err := db.JobFor(memory.SourceOf(s.Key, s.ActiveTurns()[0]))
	if err != nil {
		t.Fatal(err)
	}
	s.Apply(snap(lifecycle.SourceBeforeRequest, "g2", "", true))
	s.Apply(snap(lifecycle.SourceOutputEvent, "g2", "", true))
	next := snap(lifecycle.SourceBeforeRequest, "g2", "", true) // the next input confirms
	next.Rows = append(next.Rows, lifecycle.Row{Index: 2, ChatID: "n", Role: "user", Data: "다음"})
	s.Apply(next)
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := db.LoadSession(s.Key)
	if err != nil || !ok {
		t.Fatalf("load %v %v", ok, err)
	}
	s = loaded
	s.Apply(snap(lifecycle.SourceLoad, "", "같은 본문", false))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	s, ok, err = db.LoadSession(s.Key)
	if err != nil || !ok {
		t.Fatalf("load %v %v", ok, err)
	}
	turn := s.ActiveTurns()[0]
	src := memory.SourceOf(s.Key, turn)
	j2, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if !src.Readable() || j2.ID == j1.ID || !turn.Confirmed || !turn.Responses[0].Revisions[1].Confirmed || src.Responses[0].Gen != "g2" {
		t.Fatalf("source=%+v jobs=%d/%d turn=%+v", src, j1.ID, j2.ID, turn)
	}
	s.Apply(snap(lifecycle.SourceBeforeRequest, "", "같은 본문", false))
	again, err := db.JobFor(memory.SourceOf(s.Key, s.ActiveTurns()[0]))
	if err != nil || again.ID != j2.ID {
		t.Fatalf("retry %v %v", again, err)
	}
	t.Logf("jobs original=%d catchup=%d retry=%d revisions=%+v", j1.ID, j2.ID, again.ID, turn.Responses[0].Revisions)
}
