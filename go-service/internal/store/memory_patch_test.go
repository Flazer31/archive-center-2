package store

import (
	"archivecenter2/goservice/internal/memory"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func c16Patch(t *testing.T, db *DB, chat, id, raw string) bool {
	t.Helper()
	var p MemoryPatch
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	changed, err := db.EditMemoryPatch(chat, id, nil, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}
func c16Memory(t *testing.T, db *DB, id string) memory.Memory {
	t.Helper()
	var raw string
	if err := db.sql.QueryRow(`SELECT detail FROM memories WHERE id=?`, memoryRowNumber(id)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var m memory.Memory
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func c16Mirrors(t *testing.T, db *DB, id string) {
	t.Helper()
	m := c16Memory(t, db, id)
	n := memoryRowNumber(id)
	var body, about, owner, slot, mode, status, aspect, scene string
	var imp *float64
	if err := db.sql.QueryRow(`SELECT body,imp,about,owner,slot,mode,status,aspect,scene FROM memories WHERE id=?`, n).Scan(&body, &imp, &about, &owner, &slot, &mode, &status, &aspect, &scene); err != nil {
		t.Fatal(err)
	}
	if body != m.Body || !reflect.DeepEqual(imp, m.Imp) || about != m.About || owner != m.Owner || slot != m.Slot || mode != m.Mode || status != m.Status || aspect != m.Aspect || scene != m.Scene {
		t.Fatalf("stale columns: %s %+v", id, m)
	}
	var es []memory.Effect
	rows, err := db.sql.Query(`SELECT k,about,slot,before_value,value,delta,was,status FROM effects WHERE memory_id=? ORDER BY k`, n)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k int
		var e memory.Effect
		var was string
		if err := rows.Scan(&k, &e.About, &e.Slot, &e.Before, &e.Value, &e.Delta, &was, &e.Status); err != nil {
			t.Fatal(err)
		}
		json.Unmarshal([]byte(was), &e.Was)
		if k != len(es)+1 {
			t.Fatal("effect k changed")
		}
		es = append(es, e)
	}
	rows.Close()
	b, _ := json.Marshal(es)
	a, _ := json.Marshal(m.Effects)
	if len(es) != len(m.Effects) || len(es) > 0 && string(b) != string(a) {
		t.Fatalf("stale effects: %s vs %s", b, a)
	}
	was := []string{}
	rows, err = db.sql.Query(`SELECT target FROM memory_was WHERE memory_id=? ORDER BY ord`, n)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v string
		rows.Scan(&v)
		was = append(was, v)
	}
	rows.Close()
	if len(was) != len(m.Was) || strings.Join(was, "|") != strings.Join(m.Was, "|") {
		t.Fatal("stale memory_was", was, m.Was)
	}
	for _, spec := range []struct {
		table, col string
		want       []memory.IndexEntry
	}{{"search_refs", "ref", memory.MemoryRefs(m)}, {"search_words", "word", memory.MemoryWords(m)}} {
		got := map[string]int{}
		rows, err = db.sql.Query(`SELECT field,`+spec.col+` FROM `+spec.table+` WHERE item=? AND field<>'mention'`, id)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var f, v string
			rows.Scan(&f, &v)
			got[f+":"+v]++
		}
		rows.Close()
		want := map[string]int{}
		for _, e := range spec.want {
			want[e.Field+":"+e.Value]++
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("stale %s %v want %v", spec.table, got, want)
		}
	}
}

func TestC16KindsMirrorsProtection(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:C16", Turn: 1, Responses: []memory.SourceResponse{{Seq: 1, Data: "synthetic"}}}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"Alpha"},{"id":"n2","name":"Beta"}],"memories":[{"id":"m1","kind":"event","body":"meeting","q":"immutable quote","roles":{"actor":["n1"],"listener":["n2"]},"time":"noon","effects":[{"about":"n1","slot":"coins","before":"9","value":"7","was":["M123"]}],"unaware":["n2"]},{"id":"m2","kind":"fact","body":"seven","about":"n1","slot":"coins","mode":"state","at":"m1","was":["m1.1"],"scope":"old","terms":"old condition","imp":5},{"id":"m3","kind":"view","body":"worried","owner":"n1","about":"n2","aspect":"emotion","basis":{"via":"experience","ref":"m1","from":"n1","confidence":"low"},"affect":{"intensity":"high","persistence":"long"},"hide_from":["n2"]}],"links":[{"id":"l1","from":"m1","rel":"causes","to":"m3"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	event, fact, view := ids["memories/0"], ids["memories/1"], ids["memories/2"]
	a, b := ids["entities/0"], ids["entities/1"]
	src.Turn++
	other := u3Stored(t, db, src, fmt.Sprintf(`{"memories":[{"ref":%q,"q":"extra mention words","hide_from":[%q]},{"ref":%q,"allow_to":[%q]}]}`, event, b, view, a))
	before, _ := db.ItemsOf([]int64{j.ID, other.ID})
	awareBefore, _ := json.Marshal(before.Awareness)
	links := count(t, db, "links")
	db.sql.Exec(`UPDATE memories SET doc_hash=? WHERE job_id=?`, strings.Repeat("a", 64), j.ID)
	if c16Patch(t, db, src.Session, fact, `{"imp":0}`) {
		t.Fatal("imp changed document")
	}
	m := c16Memory(t, db, fact)
	if m.Imp == nil || *m.Imp != 0 {
		t.Fatal("zero lost")
	}
	var hash string
	db.sql.QueryRow(`SELECT doc_hash FROM memories WHERE id=?`, memoryRowNumber(fact)).Scan(&hash)
	if hash == "" {
		t.Fatal("imp-only unlinked vectors")
	}
	if c16Patch(t, db, src.Session, view, `{"basis":{"ref":"M999"}}`) {
		t.Fatal("basis.ref changed document")
	}
	if !c16Patch(t, db, src.Session, event, fmt.Sprintf(`{"roles":{"actor":[%q]},"time":"evening","setting":"library","seen_by":[%q],"outcome":"partial","scene":"S44","effects":{"1":{"about":%q,"slot":"balance","before":"7","value":"4","delta":"-3","status":"done","was":["M321"]}},"was":["M222"]}`, b, a, b)) {
		t.Fatal("event document unchanged")
	}
	if !c16Patch(t, db, src.Session, fact, fmt.Sprintf(`{"about":%q,"slot":"balance","mode":"obligation","to":[%q],"scope":"local","terms":"on return","status":"done","at":%q,"was":[],"scene":"S44"}`, b, a, event)) {
		t.Fatal("fact document unchanged")
	}
	if !c16Patch(t, db, src.Session, view, fmt.Sprintf(`{"owner":%q,"about":%q,"aspect":"belief","basis":{"via":"report","from":%q,"confidence":"high"},"affect":{"intensity":"low"},"expressed":"yes","differs":"unknown"}`, b, a, a)) {
		t.Fatal("view document unchanged")
	}
	for _, id := range []string{event, fact, view} {
		c16Mirrors(t, db, id)
	}
	m = c16Memory(t, db, event)
	if m.ID != event || m.Q != "immutable quote" || len(m.Effects) != 1 || m.Roles.Listener[0] != b || m.Roles.Actor[0] != b {
		t.Fatal("immutable or omitted field changed", m)
	}
	m = c16Memory(t, db, view)
	if m.Owner != b || m.Basis.Ref != "M999" || m.Affect.Persistence != "long" {
		t.Fatal("raw ID or partial object lost", m)
	}
	after, _ := db.ItemsOf([]int64{j.ID, other.ID})
	awareAfter, _ := json.Marshal(after.Awareness)
	if string(awareBefore) != string(awareAfter) || count(t, db, "links") != links {
		t.Fatal("protection or semantic links changed")
	}
	var mention int
	db.sql.QueryRow(`SELECT COUNT(*) FROM search_words WHERE job_id=? AND field='mention'`, other.ID).Scan(&mention)
	if mention == 0 {
		t.Fatal("mention words removed")
	}
	k, err := db.KnownFor([]memory.Source{{Session: src.Session, Turn: 1, Responses: src.Responses}, src})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range memory.CurrentFacts(k) {
		if f.ID == fact && f.About == b && f.Slot == "balance" && f.Terms == "on return" {
			found = true
		}
	}
	if !found {
		t.Fatal("current facts stale", k)
	}
	delivered, err := db.Deliver([]memory.ScopedTurn{{Source: memory.Source{Session: src.Session, Turn: 1, Responses: src.Responses}}, {Source: src}}, DeliveryRequest{Input: "meeting seven worried Beta", Payload: []TextPrint{{Role: "user"}}, Chars: 10000, Secret: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delivered.Block, "on return") || len(delivered.SecretIDs) == 0 {
		t.Fatal("edited delivery/protection missing", delivered)
	}
	c16Patch(t, db, src.Session, fact, `{"imp":null,"at":"","scene":null,"scope":"","to":[]}`)
	m = c16Memory(t, db, fact)
	if m.Imp != nil || m.At != "" || m.Scene != "" || m.Scope != "" || len(m.To) != 0 || m.Terms != "on return" {
		t.Fatal("clear vs omit", m)
	}
	c16Patch(t, db, src.Session, view, `{"basis":{"from":null},"affect":{"intensity":""}}`)
	m = c16Memory(t, db, view)
	if m.Basis.From != "" || m.Basis.Via != "report" || m.Affect.Intensity != "" {
		t.Fatal("nested clear", m)
	}
	for _, bad := range []string{`{"kind":"event"}`, `{"id":"M999"}`, `{"q":"other"}`, `{"job":9}`, `{"source":{}}`, `{"unaware":[]}`, `{"hide_from":[]}`, `{"allow_to":[]}`, `{"effects":{"2":{"value":"new"}}}`, `{"effects":[]}`, `{"effects":{"1":{"k":2}}}`} {
		var p MemoryPatch
		json.Unmarshal([]byte(bad), &p)
		if _, err := db.EditMemoryPatch(src.Session, event, nil, nil, p); err != ErrBadMemoryPatch {
			t.Fatalf("read-only patch %s: %v", bad, err)
		}
	}
}

func TestC16BasisEffectProtectionLanes(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"A changes", "event"}, {"B learns", "aware"}}, "A worry")
	src := turnSource(s, 0)
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"A"},{"id":"n2","name":"B"}],"memories":[{"id":"m1","kind":"event","body":"A changes","effects":[{"about":"n1","slot":"balance","value":"one"}]},{"id":"m2","kind":"view","owner":"n1","about":"n1","body":"A worry","aspect":"emotion","basis":{"ref":"M9999"}}]}`)
	ids, _ := db.ItemIDs(j.ID)
	event, view := ids["memories/0"], ids["memories/1"]
	u3Stored(t, db, turnSource(s, 1), fmt.Sprintf(`{"memories":[{"ref":%q,"hide_from":[%q],"allow_to":[%q]}]}`, event+".1", ids["entities/1"], ids["entities/1"]))
	c16Patch(t, db, src.Session, event, `{"effects":{"1":{"value":"two"}}}`)
	c16Patch(t, db, src.Session, view, fmt.Sprintf(`{"basis":{"ref":%q}}`, event+".1"))
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "A worry changes balance", Payload: []TextPrint{{Role: "user"}}, Chars: 5000, Secret: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if !d.SecretIDs[view] || !d.SecretIDs[event] || !strings.Contains(d.RetainedBlock, "A worry") || strings.Contains(d.PDFBlock, "A worry") || !strings.Contains(d.RetainedBlock, "two") {
		t.Fatal("basis/effect protection left the retained lane", d)
	}
	assertTransportLines(t, d)
}

func memoryRowNumber(id string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(id, "M"), 10, 64)
	return n
}
