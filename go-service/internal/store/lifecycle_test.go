package store

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

func lrow(id, role, gen, data string) lifecycle.Row {
	return lifecycle.Row{ChatID: id, Role: role, GenerationID: gen, Data: data}
}

func lsnap(src lifecycle.Source, rows ...lifecycle.Row) lifecycle.Snapshot {
	for i := range rows {
		rows[i].Index = i
	}
	return lifecycle.Snapshot{SessionKey: "chat:A", Source: src, Rows: rows, TargetIndex: -1}
}

// Saved state reads back equal (turns, members, rows, revision history,
// states, confirmation), and the loaded session continues the same IDs.
func TestSessionSaveLoadRoundTrip(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("g0", "char", "", "인사"), lrow("u1", "user", "", "첫 입력 😀"), lrow("u2", "user", "", "둘째"), lrow("a1", "char", "g1", "응답1")))
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("g0", "char", "", "인사"), lrow("u1", "user", "", "첫 입력 😀"), lrow("u2", "user", "", "둘째"), lrow("a1", "char", "g1", "응답1 수정"), lrow("u3", "user", "", "셋째")))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("g0", "char", "", "인사"), lrow("u1", "user", "", "첫 입력 😀"), lrow("u2", "user", "", "둘째"), lrow("a1", "char", "g1", "응답1 수정"), lrow("u3", "user", "", "셋째"), lrow("a3", "char", "g3", "응답3")))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("g0", "char", "", "인사"), lrow("u1", "user", "", "첫 입력 😀"), lrow("u2", "user", "", "둘째"), lrow("a1", "char", "g1", "응답1 수정"), lrow("u3", "user", "", "셋째"), lrow("a4", "char", "g4", "리롤")))
	events := s.TakeEvents()
	if err := db.SaveSession(s, events, s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.LoadSession("chat:A")
	if err != nil || !ok {
		t.Fatalf("load ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got.Turns, s.Turns) {
		for i := range s.Turns {
			t.Logf("want %+v\n got %+v", *s.Turns[i], *got.Turns[i])
		}
		t.Fatalf("loaded turns differ")
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM lifecycle_events WHERE session_key='chat:A'`).Scan(&n); err != nil || n != len(events) || n == 0 {
		t.Fatalf("events stored %d, want %d (err %v)", n, len(events), err)
	}
	// saving again is idempotent and the loaded session keeps issuing new IDs
	if err := db.SaveSession(got, got.TakeEvents(), got.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	last := got.ActiveTurns()[len(got.ActiveTurns())-1].ID
	got.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("g0", "char", "", "인사"), lrow("u1", "user", "", "첫 입력 😀"), lrow("u2", "user", "", "둘째"), lrow("a1", "char", "g1", "응답1 수정"), lrow("u3", "user", "", "셋째"), lrow("a4", "char", "g4", "리롤"), lrow("u5", "user", "", "다음")))
	if id := got.ActiveTurns()[len(got.ActiveTurns())-1].ID; id <= last {
		t.Fatalf("new turn id %d not after %d", id, last)
	}
}

func TestLoadMissingSession(t *testing.T) {
	db := testDB(t)
	if _, ok, err := db.LoadSession("none"); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

// Rows stored before RisuAI gave them a chatId keep one stored row each
// after the key change, with their revisions.
func TestRenamedRowsKeepOneStoredRow(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	s.Apply(lsnap(lifecycle.SourceLoad, lifecycle.Row{Role: "user", Time: 7, Data: "one"}, lifecycle.Row{Role: "char", Data: "r1"}))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lifecycle.Row{ChatID: "u1", Role: "user", Time: 7, Data: "one"}, lrow("a1", "char", "g1", "r1")))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	var members, rows, revs int
	db.sql.QueryRow(`SELECT COUNT(*) FROM turn_members`).Scan(&members)
	db.sql.QueryRow(`SELECT COUNT(*) FROM response_rows`).Scan(&rows)
	db.sql.QueryRow(`SELECT COUNT(*) FROM revisions WHERE row_key='c:a1'`).Scan(&revs)
	if members != 1 || rows != 1 || revs != 1 {
		t.Fatalf("members %d rows %d revisions under new key %d", members, rows, revs)
	}
	got, _, err := db.LoadSession("chat:A")
	if err != nil || !reflect.DeepEqual(got.Turns, s.Turns) {
		t.Fatalf("reload differs (err %v)", err)
	}
}

// The applied plugin seq per chat is stored with the chat: after a restart a
// late older observation still does not move the chat back.
func TestProgressSurvivesRestart(t *testing.T) {
	db := testDB(t)
	rec := func(seq int, body string) observe.Record {
		return observe.Record{Event: json.RawMessage(`{"boot_id":"b","seq":` + strconv.Itoa(seq) + `,"hook":"beforeRequest","stored_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a1","generation_id":"g1","data":{"text":"` + body + `"}}]}}`)}
	}
	p := pipeline.New(db)
	p.Process([]observe.Record{rec(1, "old"), rec(2, "current")})
	p = pipeline.New(db) // restart
	p.Process([]observe.Record{rec(1, "old")})
	s, _, err := db.LoadSession("chat:A")
	if err != nil {
		t.Fatal(err)
	}
	r := s.Turns[0].Responses[0]
	if got := r.Revisions[len(r.Revisions)-1].Data; got != "current" || s.Progress["b"] != 2*4 { // seq 2, first snapshot of its event
		t.Fatalf("after restart current %q progress %v", got, s.Progress)
	}
}

// A reroll split by a backend restart (old reply removed, restart, new reply)
// still records the old reply as replaced.
func TestRerollAcrossRestartReplaces(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "q"), lrow("a1", "char", "g1", "first")))
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "q")))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	r, _, err := db.LoadSession("chat:A")
	if err != nil {
		t.Fatal(err)
	}
	r.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "q"), lrow("a2", "char", "g2", "second")))
	// the deleted reply waiting for its replacement is still known after the
	// restart: the new reply replaces it on the same turn, and only the
	// newest value is kept, also in the database
	old := r.Turns[0].Responses[0]
	if len(r.ActiveTurns()) != 1 || old.State != lifecycle.Replaced || len(old.Revisions) != 1 || old.Revisions[0].Data != "" {
		t.Fatalf("old reply after restart: %+v", old)
	}
	if err := db.SaveSession(r, r.TakeEvents(), r.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM revisions WHERE session_key='chat:A' AND row_key='c:a1' AND data<>''`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the replaced reply's text is still stored: %d %v", n, err)
	}
}

// A turn keeps the date variables seen while it was the newest turn (the
// date its reply left); they survive a restart, reach the model input, and
// do not change the source key.
func TestStoryDatePerTurnSavedAndInPrompt(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	dated := func(sn lifecycle.Snapshot, d string) lifecycle.Snapshot {
		sn.Dates = map[string]string{"clock": d}
		return sn
	}
	s.Apply(dated(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "하나"), lrow("a1", "char", "g1", "첫째")), "1423/05/15"))
	s.Apply(dated(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "하나"), lrow("a1", "char", "g1", "첫째"), lrow("u2", "user", "", "둘")), "1423/05/15"))
	s.Apply(dated(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "하나"), lrow("a1", "char", "g1", "첫째"), lrow("u2", "user", "", "둘"), lrow("a2", "char", "g2", "다음 날")), "1423/05/16"))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	r, _, err := db.LoadSession("chat:A")
	if err != nil {
		t.Fatal(err)
	}
	turns := r.ActiveTurns()
	if turns[0].Dates["clock"] != "1423/05/15" || turns[1].Dates["clock"] != "1423/05/16" {
		t.Fatalf("dates %v %v", turns[0].Dates, turns[1].Dates)
	}
	src := memory.SourceOf(r.Key, turns[1])
	if p := memory.Prompt(src, ""); !strings.Contains(p, "STORY DATE (chat variables after this turn): clock = 1423/05/16") {
		t.Fatalf("prompt lacks the date:\n%s", p[len(p)-200:])
	}
	plain := src
	plain.Dates = nil
	if plain.Key() != src.Key() {
		t.Fatal("the date changed the source key")
	}
}
