package main

import (
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// C12 (user 11-06: every memory has a date): the memory list gives each
// memory the recorded date injection reads (Day counted from the chat's
// first turn) and when it happened, through the same reads.
func TestMemoryListGivesEveryMemoryADate(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	if _, err := p.Process([]observe.Record{attEvent(1, "load", "X",
		memRow(0, "user", "u1", "", "아침"), memRow(1, "char", "a1", "g1", "문을 열었다"),
		memRow(2, "user", "u2", "", "이틀 뒤"), memRow(3, "char", "a2", "g2", "다시 왔다"))}); err != nil {
		t.Fatal(err)
	}
	var s *lifecycle.Session
	p.WithSession("chat:X", func(v *lifecycle.Session) { s = v })
	own := memory.Own(s)
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"memories":[{"id":"m1","kind":"event","body":"문을 열었다","time":"첫날 아침"},{"id":"m2","kind":"fact","body":"문이 열려 있다","at":"m1"},{"id":"m3","kind":"event","body":"바람이 불었다"}]}`)
	u3Save(t, db, own[1].Source, `{"elapsed_days":2,"memories":[{"id":"m1","kind":"fact","body":"다시 왔다"},{"id":"m2","kind":"view","owner":"","body":"바람이 기억난다","at":"M3"}]}`)
	list, err := db.ChatMemories(s.Key, own)
	if err != nil {
		t.Fatal(err)
	}
	k, err := db.CurrentKnown(s)
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]string{}
	for _, m := range k.Memories {
		read[m.ID] = m.Date
	}
	type row struct{ date, when, from string }
	got := map[string]row{}
	for _, m := range list {
		if m.Date != read[m.ID] {
			t.Fatalf("%s: list date %q, injection reads %q", m.ID, m.Date, read[m.ID])
		}
		got[m.Body] = row{m.Date, m.When.Text, m.When.From}
	}
	want := map[string]row{
		"문을 열었다":   {"Day 0", "첫날 아침", "time"},
		"문이 열려 있다": {"Day 0", "첫날 아침", "at"},
		"바람이 불었다":  {"Day 0", "Day 0", "turn"},
		"다시 왔다":    {"Day 2", "Day 2", "turn"},
		"바람이 기억난다": {"Day 2", "Day 0", "turn"},
	}
	for body, w := range want {
		if got[body] != w {
			t.Fatalf("%s: got %+v want %+v (all %+v)", body, got[body], w, got)
		}
	}
}
