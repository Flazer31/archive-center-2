package main

import (
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// The critic's answers as stored: a pregnancy whose father the mother does
// not know, then a birth naming the child. The lines sent name the mother,
// the child and the father as her secret, also after the chat is moved (the
// names follow the move).
func TestBirthThroughTheStore(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", "리나가 왔다"),
		memRow(2, "user", "u2", "", "사십 일 뒤"), memRow(3, "char", "a2", "g2", "사십 일이 지났다"),
		memRow(4, "user", "u3", "", "그날"), memRow(5, "char", "a3", "g3", "카일의 아이를 가졌다"),
		memRow(6, "user", "u4", "", "아홉 달 뒤"), memRow(7, "char", "a4", "g4", "아이가 태어났다"),
		memRow(8, "user", "u5", "", "이름을 짓자"), memRow(9, "char", "a5", "g5", "아이의 이름은 하늘이 되었다")}
	// BB2: the same chat as the host has it under another id, the move's
	// destination
	for i, chat := range []string{"BB", "BB2"} {
		if _, err := p.Process([]observe.Record{memEvent(i+1, "load", chat, rows...)}); err != nil {
			t.Fatal(err)
		}
	}
	var scope []memory.ScopedTurn
	p.WithSession("chat:BB", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	u3Save(t, db, scope[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 왔다"}],"body_events":[{"kind":"period_start","who":"n1"}]}`)
	u3Save(t, db, scope[1].Source, `{"elapsed_days":40,"memories":[{"id":"m1","kind":"event","body":"사십 일이 지났다"}]}`)
	db.SetBodyTracking(body.Toggles{Cycle: true, Pregnancy: true})
	rina := bodyState(t, db, "BB", scope[:2]).Who
	u3Save(t, db, scope[2].Source, fmt.Sprintf(`{"elapsed_days":0,"entities":[{"id":"n1","name":"카일","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"임신을 알았다"}],"body_events":[`+
		`{"kind":"pregnancy_confirmed","who":%q,"paternity":{"status":"confirmed","candidates":["n1"],"unaware":[%q]},"visibility":"private"}]}`, rina, rina))
	// born without a name; named in the next turn (a later name joins the
	// birth)
	u3Save(t, db, scope[3].Source, fmt.Sprintf(`{"elapsed_days":260,"memories":[{"id":"m1","kind":"event","body":"아이가 태어났다"}],"body_events":[`+
		`{"kind":"pregnancy_ended","who":%q,"outcome":"birth","visibility":"public"}]}`, rina))
	u3Save(t, db, scope[4].Source, fmt.Sprintf(`{"elapsed_days":0,"entities":[{"id":"n1","name":"하늘","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"아이의 이름을 하늘로 지었다"}],"body_events":[`+
		`{"kind":"child_named","who":%q,"child":"n1"}]}`, rina))
	lines, err := bodyLines(db, "chat:BB", scope, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "gave birth Day 300; child 하늘; mother 리나; father 카일 (secret: 리나 do not know who the father is)"
	if !strings.Contains(strings.Join(lines, "\n"), want) {
		t.Fatalf("lines lack %q:\n%s", want, strings.Join(lines, "\n"))
	}
	if _, err := db.ChatOperation("move", "chat:BB", "chat:BB2", false); err != nil {
		t.Fatal(err)
	}
	s2, ok, err := db.LoadSession("chat:BB2")
	if err != nil || !ok {
		t.Fatal("destination", ok, err)
	}
	moved, err := bodyLines(db, "chat:BB2", memory.ScopeOf(s2), 1, "")
	if err != nil || !strings.Contains(strings.Join(moved, "\n"), want) {
		t.Fatalf("after the move: %v\n%s", err, strings.Join(moved, "\n"))
	}
}

// One reply's events in any order: the name and the father written
// before the birth they are about, and a learning written before the
// pregnancy it is about, read as if after.
func TestOneReplysBodyEventsInAnyOrder(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", "리나가 왔다"),
		memRow(2, "user", "u2", "", "아홉 달 뒤"), memRow(3, "char", "a2", "g2", "아이가 태어났다"),
		memRow(4, "user", "u3", "", "일 년 뒤"), memRow(5, "char", "a3", "g3", "다시 아이를 가졌다")}
	if _, err := p.Process([]observe.Record{memEvent(1, "load", "BO", rows...)}); err != nil {
		t.Fatal(err)
	}
	var scope []memory.ScopedTurn
	p.WithSession("chat:BO", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	u3Save(t, db, scope[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 왔다"}],"body_events":[{"kind":"period_start","who":"n1"}]}`)
	db.SetBodyTracking(body.Toggles{Cycle: true, Pregnancy: true})
	rina := bodyState(t, db, "BO", scope[:1]).Who
	// another woman's birth written between 리나's name and her birth; next
	// turn, her profile between a learning and the pregnancy it is about
	u3Save(t, db, scope[1].Source, fmt.Sprintf(`{"elapsed_days":300,"entities":[{"id":"n1","name":"하늘","type":"person"},{"id":"n2","name":"카일","type":"person"},{"id":"n3","name":"미아","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"아이가 태어났다"}],"body_events":[`+
		`{"kind":"child_named","who":%q,"child":"n1"},`+
		`{"kind":"paternity_learned","who":%q,"child":"n1","paternity":{"status":"confirmed","candidates":["n2"],"learns":[%q]}},`+
		`{"kind":"pregnancy_ended","who":"n3","outcome":"birth","visibility":"public"},`+
		`{"kind":"pregnancy_ended","who":%q,"outcome":"birth","visibility":"public"}]}`, rina, rina, rina, rina))
	u3Save(t, db, scope[2].Source, fmt.Sprintf(`{"elapsed_days":365,"entities":[{"id":"n1","name":"도윤","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"다시 아이를 가졌다"}],"body_events":[`+
		`{"kind":"paternity_learned","who":%q,"paternity":{"status":"confirmed","candidates":["n1"],"learns":[%q]}},`+
		`{"kind":"profile","who":%q,"sex":"female"},`+
		`{"kind":"pregnancy_confirmed","who":%q,"visibility":"private"}]}`, rina, rina, rina, rina))
	lines, err := bodyLines(db, "chat:BO", scope, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "gave birth Day 300; child 하늘; mother 리나; father 카일") || !strings.Contains(all, "parent established: 도윤") {
		t.Fatalf("lines:\n%s", all)
	}
}

// A learning written before a loss in the same reply is about the pregnancy
// lost: it does not reach the child born before.
func TestALearningBeforeALossStaysWithThatPregnancy(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", "리나가 왔다"),
		memRow(2, "user", "u2", "", "아홉 달 뒤"), memRow(3, "char", "a2", "g2", "아이가 태어났다"),
		memRow(4, "user", "u3", "", "일 년 뒤"), memRow(5, "char", "a3", "g3", "다시 아이를 가졌다"),
		memRow(6, "user", "u4", "", "한 달 뒤"), memRow(7, "char", "a4", "g4", "아이를 잃었다")}
	if _, err := p.Process([]observe.Record{memEvent(1, "load", "BL", rows...)}); err != nil {
		t.Fatal(err)
	}
	var scope []memory.ScopedTurn
	p.WithSession("chat:BL", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	u3Save(t, db, scope[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 왔다"}],"body_events":[{"kind":"period_start","who":"n1"}]}`)
	db.SetBodyTracking(body.Toggles{Cycle: true, Pregnancy: true})
	rina := bodyState(t, db, "BL", scope[:1]).Who
	u3Save(t, db, scope[1].Source, fmt.Sprintf(`{"elapsed_days":300,"entities":[{"id":"n1","name":"하늘","type":"person"},{"id":"n2","name":"카일","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"아이가 태어났다"}],"body_events":[`+
		`{"kind":"pregnancy_ended","who":%q,"outcome":"birth","child":"n1","paternity":{"status":"confirmed","candidates":["n2"]},"visibility":"public"}]}`, rina))
	u3Save(t, db, scope[2].Source, fmt.Sprintf(`{"elapsed_days":365,"memories":[{"id":"m1","kind":"event","body":"다시 아이를 가졌다"}],"body_events":[`+
		`{"kind":"pregnancy_confirmed","who":%q,"visibility":"private"}]}`, rina))
	// another woman's pregnancy confirmed after the loss in the same reply
	// moves nothing of 리나's
	u3Save(t, db, scope[3].Source, fmt.Sprintf(`{"elapsed_days":30,"entities":[{"id":"n1","name":"도윤","type":"person"},{"id":"n2","name":"미아","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"아이를 잃었다"}],"body_events":[`+
		`{"kind":"paternity_learned","who":%q,"paternity":{"status":"confirmed","candidates":["n1"],"learns":[%q]}},`+
		`{"kind":"pregnancy_ended","who":%q,"outcome":"loss"},`+
		`{"kind":"pregnancy_confirmed","who":"n2","visibility":"private"}]}`, rina, rina, rina))
	lines, err := bodyLines(db, "chat:BL", scope, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "child 하늘; mother 리나; father 카일") {
		t.Fatalf("lines:\n%s", all)
	}
}

// The name written under another name of hers (an alias the turn records)
// before her birth: she is one person, the name joins that birth.
func TestANameUnderHerOtherName(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", "리나가 왔다"),
		memRow(2, "user", "u2", "", "아홉 달 뒤"), memRow(3, "char", "a2", "g2", "아이가 태어났다")}
	if _, err := p.Process([]observe.Record{memEvent(1, "load", "BA", rows...)}); err != nil {
		t.Fatal(err)
	}
	var scope []memory.ScopedTurn
	p.WithSession("chat:BA", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	u3Save(t, db, scope[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 왔다"}],"body_events":[{"kind":"period_start","who":"n1"}]}`)
	db.SetBodyTracking(body.Toggles{Cycle: true, Pregnancy: true})
	rina := bodyState(t, db, "BA", scope[:1]).Who
	u3Save(t, db, scope[1].Source, fmt.Sprintf(`{"elapsed_days":300,"entities":[{"id":"n1","name":"린","type":"person","same_as":%q,"how":"alias"},{"id":"n2","name":"하늘","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"아이가 태어났다"}],"body_events":[`+
		`{"kind":"child_named","who":"n1","child":"n2"},`+
		`{"kind":"pregnancy_ended","who":%q,"outcome":"birth","visibility":"public"}]}`, rina, rina))
	lines, err := bodyLines(db, "chat:BA", scope, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(lines, "\n"); !strings.Contains(all, "gave birth Day 300; child 하늘") {
		t.Fatalf("lines:\n%s", all)
	}
}
