package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

func bodyCall(t *testing.T, h http.HandlerFunc, method, path, req string, code int) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, path, strings.NewReader(req)))
	if rec.Code != code {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	var v map[string]any
	json.Unmarshal(rec.Body.Bytes(), &v)
	return v
}

type bodyView struct {
	People []struct {
		Who, Name string
		Facts     []struct {
			ID   int64
			Kind string
			Day  string
			User bool
		}
		State struct {
			Cycle *struct{ Status string }
			Lines []string
		}
	}
}

func bodyGet(t *testing.T, h http.HandlerFunc, chat string) bodyView {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/body?chat_id="+chat, nil))
	if rec.Code != 200 {
		t.Fatalf("GET body: %d %s", rec.Code, rec.Body.String())
	}
	var v bodyView
	json.Unmarshal(rec.Body.Bytes(), &v)
	return v
}

func bodyKinds(v bodyView) []string {
	var out []string
	for _, p := range v.People {
		for _, f := range p.Facts {
			out = append(out, p.Name+":"+f.Kind+"@"+f.Day)
		}
	}
	return out
}

// 1.0 E09/E10: body events written by a turn's generation are the person's
// facts while that generation is the turn's current memory — the five
// request-identity cases treat them as they treat memory — and the state
// is delivered in its own section inside the fingerprinted block.
func TestBodyEventsFollowTurnsAndAreDelivered(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	h := bodyHandler(p, db)
	if v := bodyGet(t, h, "X"); len(v.People) != 0 {
		t.Fatal("off: nothing")
	}
	bodyCall(t, h, "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	seq := 0
	apply := func(hook string, rows ...string) []memory.ScopedTurn {
		t.Helper()
		seq++
		if _, err := p.Process([]observe.Record{attEvent(seq, hook, "X", rows...)}); err != nil {
			t.Fatal(err)
		}
		var own []memory.ScopedTurn
		p.WithSession("chat:X", func(s *lifecycle.Session) { own = memory.Own(s) })
		return own
	}
	rina := "" // the person's id once the first turn named her
	made := func(src memory.Source, kind string, days int) {
		t.Helper()
		who, entities := rina, ""
		if who == "" {
			who, entities = "n1", `"entities":[{"id":"n1","name":"리나","type":"person"}],`
		}
		u3Save(t, db, src, `{"elapsed_days":`+string(rune('0'+days))+`,`+entities+`"memories":[{"id":"m1","kind":"event","body":"리나가 정원에서 장미를 심었다"}],"body_events":[{"kind":"`+kind+`","who":"`+who+`","q":"리나"}]}`)
		if rina == "" {
			rina = bodyGet(t, h, "X").People[0].Who
		}
	}
	u1, a1 := memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", "리나가 아팠다")
	own := apply("output", u1, a1)
	made(own[0].Source, "period_start", 0)
	if got := strings.Join(bodyKinds(bodyGet(t, h, "X")), ","); got != "리나:period_start@Day 0" {
		t.Fatalf("a new turn's event: %s", got)
	}
	// a normal new turn appends its own event
	u2, a2 := memRow(2, "user", "u2", "", "다음 날"), memRow(3, "char", "a2", "g2", "둘")
	own = apply("output", u1, a1, u2, a2)
	made(own[1].Source, "recovery_started", 3)
	if got := strings.Join(bodyKinds(bodyGet(t, h, "X")), ","); got != "리나:period_start@Day 0,리나:recovery_started@Day 3" {
		t.Fatalf("two turns: %s", got)
	}
	// the same text in a new host row is a new turn: its own event
	u3 := memRow(4, "user", "u3", "", "다음 날")
	own = apply("output", u1, a1, u2, a2, u3, memRow(5, "char", "a3", "g3", "셋"))
	made(own[2].Source, "recovery_ended", 1)
	// a reroll of that row replaces its memory: the old event goes with it
	own = apply("output", u1, a1, u2, a2, u3, memRow(5, "char", "a9", "g9", "셋 다시"))
	if got := strings.Join(bodyKinds(bodyGet(t, h, "X")), ","); strings.Contains(got, "recovery_ended") {
		t.Fatalf("a rerolled reply's event stays: %s", got)
	}
	made(own[2].Source, "period_start", 1)
	// reply deleted, row edited, regenerated: replaces again
	u3e := memRow(4, "user", "u3", "", "다음 날 고침")
	apply("beforeRequest", u1, a1, u2, a2, u3e)
	own = apply("output", u1, a1, u2, a2, u3e, memRow(5, "char", "a10", "g10", "새로"))
	if got := strings.Join(bodyKinds(bodyGet(t, h, "X")), ","); got != "리나:period_start@Day 0,리나:recovery_started@Day 3" {
		t.Fatalf("edited and regenerated: %s", got)
	}
	made(own[2].Source, "period_start", 2)
	// the same request again (provider retry) changes nothing
	a10, u4 := memRow(5, "char", "a10", "g10", "새로"), memRow(6, "user", "u4", "", "계속")
	apply("beforeRequest", u1, a1, u2, a2, u3e, a10, u4)
	before := strings.Join(bodyKinds(bodyGet(t, h, "X")), ",")
	apply("beforeRequest", u1, a1, u2, a2, u3e, a10, u4)
	if got := strings.Join(bodyKinds(bodyGet(t, h, "X")), ","); got != before || got != "리나:period_start@Day 0,리나:recovery_started@Day 3,리나:period_start@Day 5" {
		t.Fatalf("retry: %s / %s", before, got)
	}
	v := bodyGet(t, h, "X")
	if v.People[0].State.Cycle == nil || v.People[0].State.Cycle.Status != "estimated" || len(v.People[0].State.Lines) == 0 {
		t.Fatalf("state %+v", v.People[0].State)
	}
	// delivered in its own section inside the block, the fingerprint over all
	var scope []memory.ScopedTurn
	p.WithSession("chat:X", func(s *lifecycle.Session) { scope = memory.ScopeOf(s) })
	lines, err := bodyLines(db, "chat:X", scope)
	if all := strings.Join(lines, "|"); err != nil || len(lines) != 2 || !strings.Contains(all, "body 리나 [story fact, private]: recovering") || !strings.Contains(all, "cycle day 1 (period started Day 5)") {
		t.Fatalf("lines %v %v", lines, err)
	}
	d, err := db.Deliver(scope, store.DeliveryRequest{Input: "리나", Chars: 2000, Secret: 4000, Body: lines})
	if err != nil {
		t.Fatal(err)
	}
	head, rest, _ := strings.Cut(d.Block, "\n")
	n, hash := ingest.Fingerprint(rest)
	_ = n
	if !strings.Contains(rest, body.Header) || !strings.Contains(rest, lines[0]) || !strings.HasSuffix(head, hash+"]") || d.BodyChars == 0 {
		t.Fatalf("block:\n%s", d.Block)
	}
	if d.PDFBlock != "" && strings.Contains(d.PDFBlock, body.Header) {
		t.Fatal("the body section goes into a PDF")
	}
	// with records chosen too (the request holds only the last turn): the
	// records first, then the body section, one fingerprint over both
	fp := func(s, role string) store.TextPrint {
		n, h := ingest.Fingerprint(s)
		return store.TextPrint{Len: n, H: h, Role: role}
	}
	req := store.DeliveryRequest{Input: "정원의 장미를 본다", Chars: 2000, Secret: 4000, Body: lines}
	req.Payload = []store.TextPrint{fp("다음 날 고침", "user"), fp("새로", "assistant"), fp(req.Input, "user")}
	d, err = db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	head, rest, _ = strings.Cut(d.Block, "\n")
	_, hash = ingest.Fingerprint(rest)
	if len(d.IDs) == 0 || !strings.HasSuffix(head, hash+"]") || strings.Index(rest, "장미") > strings.Index(rest, body.Header) || !strings.Contains(d.RetainedBlock, body.Header) {
		t.Fatalf("records and body: window %d %s candidates %d ids %v\n%s", d.Window, d.WindowFrom, d.Candidates, d.IDs, d.Block)
	}
	// switched off: no section, no rules
	bodyCall(t, h, "POST", "/body/settings", `{}`, 200)
	if lines, _ := bodyLines(db, "chat:X", scope); len(lines) != 0 {
		t.Fatal("off but delivered")
	}
}

// Facts a user records and removes (AC1 fact correction and undo), and a
// person's settings, which must describe a cycle.
func TestBodyRecordsAndSettings(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	h := bodyHandler(p, db)
	bodyCall(t, h, "POST", "/body/settings", `{"cycle_tracking_enabled":true}`, 200)
	bodyCall(t, h, "POST", "/body/record", `{"chat_id":"X","who":"N7","kind":"conception_exposure","day":"Day 3"}`, 400)
	bodyCall(t, h, "POST", "/body/record", `{"chat_id":"X","who":"N7","kind":"period_start","day":"아침"}`, 400)
	id := bodyCall(t, h, "POST", "/body/record", `{"chat_id":"X","who":"N7","kind":"period_start","day":"2026/01/02"}`, 200)["id"]
	v := bodyGet(t, h, "X")
	if len(v.People) != 1 || v.People[0].Who != "N7" || !v.People[0].Facts[0].User || v.People[0].Facts[0].Day != "2026/01/02" {
		t.Fatalf("recorded %+v", v)
	}
	bodyCall(t, h, "POST", "/body/person", `{"chat_id":"X","who":"N7","spec":{"cycle_days":30,"variation_days":0,"period_days":4,"luteal_min_days":12,"luteal_max_days":40,"viability":0.4,"peak":0.7,"gestation_days":266,"can_conceive":true,"cycle_enabled":true}}`, 400)
	bodyCall(t, h, "POST", "/body/person", `{"chat_id":"X","who":"N7","spec":{"cycle_days":30,"variation_days":0,"period_days":4,"luteal_min_days":12,"luteal_max_days":14,"viability":0.4,"peak":0.7,"gestation_days":266,"can_conceive":true,"cycle_enabled":true}}`, 200)
	if got, _ := db.BodyPerson("chat:X", "N7"); got.Spec.CycleDays != 30 {
		t.Fatalf("settings %+v", got)
	}
	bodyCall(t, h, "POST", "/body/record/delete", `{"chat_id":"Y","id":`+jsonNum(id)+`}`, 404)
	bodyCall(t, h, "POST", "/body/record/delete", `{"chat_id":"X","id":`+jsonNum(id)+`}`, 200)
	if v := bodyGet(t, h, "X"); len(v.People) != 0 {
		t.Fatalf("removed %+v", v)
	}
}

func jsonNum(v any) string { b, _ := json.Marshal(v); return string(b) }

// A fact read from a turn is not deleted on the body screen (it changes
// with its turn); copying a chat carries its body events with the copy's
// own person id.
func TestBodyTurnFactsStayAndCopy(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	h := bodyHandler(p, db)
	bodyCall(t, h, "POST", "/body/settings", `{"cycle_tracking_enabled":true}`, 200)
	rows := []string{memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", "리나가 아팠다")}
	var own []memory.ScopedTurn
	for i, chat := range []string{"X", "Y"} {
		if _, err := p.Process([]observe.Record{attEvent(i+1, "load", chat, rows...)}); err != nil {
			t.Fatal(err)
		}
		if chat == "X" {
			p.WithSession("chat:X", func(s *lifecycle.Session) { own = memory.Own(s) })
		}
	}
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 아팠다","roles":{"actor":["n1"]}}],"body_events":[{"kind":"period_start","who":"n1","q":"아팠다"}]}`)
	x := bodyGet(t, h, "X")
	if len(x.People) != 1 {
		t.Fatalf("X %+v", x)
	}
	bodyCall(t, h, "POST", "/body/record/delete", `{"chat_id":"X","id":`+jsonNum(x.People[0].Facts[0].ID)+`}`, 404)
	if v := bodyGet(t, h, "X"); len(v.People) != 1 {
		t.Fatal("a turn's fact was deleted")
	}
	u2Post(t, chatOpsHandler(p, db, nil, nil), "/chats/copy", `{"from":"X","to":"Y"}`)
	y := bodyGet(t, h, "Y")
	if len(y.People) != 1 || y.People[0].Name != "리나" || y.People[0].Who == x.People[0].Who || y.People[0].Facts[0].Kind != "period_start" {
		t.Fatalf("copy: X %+v Y %+v", x.People, y.People)
	}
}
