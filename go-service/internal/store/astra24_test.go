package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

func a24Save(t *testing.T, db *DB, src memory.Source, raw string) (Job, Items, []string) {
	t.Helper()
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := db.SaveGeneration(j, Generation{Request: "synthetic request", ReadItems: "[]", Response: raw, Output: parse(t, raw)})
	if err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	return j, it, notes
}

func TestA24RelationWithoutTempID(t *testing.T) {
	db := testDB(t)
	_, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"entities":[{"id":"n1","name":"민재"},{"id":"n2","name":"가면","same_as":"n1","how":"disguise","unaware":["서진"],"hide_from":["public_narration"]}]}`)
	if len(it.Relations) != 1 || len(it.Awareness) != 2 {
		t.Fatalf("unexpected rows: %+v", it)
	}
	for _, a := range it.Awareness {
		if a.Target != it.Relations[0].ID {
			t.Errorf("identity knowledge targets %s; relation is %s (name %s), notes=%v", a.Target, it.Relations[0].ID, it.Names[1].ID, notes)
		}
	}
}

func TestA24DuplicateTempMemoryIDs(t *testing.T) {
	db := testDB(t)
	_, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"memories":[{"id":"m1","kind":"fact","body":"첫 사실"},{"id":"m1","kind":"fact","body":"둘째 사실"}],"links":[{"from":"m1","rel":"same_fact","to":"M999"}]}`)
	for _, m := range it.Memories {
		if m.ID != m.Memory.ID {
			t.Errorf("row ID=%s detail ID=%s body=%s", m.ID, m.Memory.ID, m.Body)
		}
	}
	if len(notes) == 0 {
		t.Errorf("ambiguous m1 silently resolved to %s without diagnostics", it.Links[0].From)
	}
}

func TestA24InvalidEffectReference(t *testing.T) {
	db := testDB(t)
	_, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"memories":[{"id":"m1","kind":"event","body":"돈 변화","effects":[{"value":"10"}]},{"id":"m2","kind":"fact","body":"새 값","was":"m1.9"}]}`)
	if it.Memories[1].Was[0] != "m1.9" || !strings.Contains(strings.Join(notes, " "), "m1.9") {
		t.Fatalf("nonexistent effect rewritten: was=%v, actual effects=%d, notes=%v", it.Memories[1].Was, len(it.Memories[0].Effects), notes)
	}
}

func TestA24UnreadableNewGeneration(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	u := lrow("u1", "user", "", "입력")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, u, lrow("a1", "char", "g1", "옛 응답")))
	before := turnSource(s, 0)
	j, _, _ := a24Save(t, db, before, `{"memories":[{"id":"m1","kind":"fact","body":"옛 응답의 사실"}]}`)
	unreadable := lrow("a1", "char", "g2", "")
	unreadable.Unreadable = true
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, u, unreadable))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, u, unreadable))
	after := turnSource(s, 0)
	cur, err := db.CurrentJobs([]memory.Source{after})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("confirmed=%v gen=%s revisionGen=%s readable=%v source=%+v oldJob=%d current=%v", s.ActiveTurns()[0].Confirmed, s.ActiveTurns()[0].Responses[0].Gen, s.ActiveTurns()[0].Responses[0].Revisions[0].GenerationID, after.Readable(), after, j.ID, cur)
	if before.Key() == after.Key() || len(cur) != 0 {
		t.Error("known new generation reused old readable source/current job")
	}
	s.Apply(lsnap(lifecycle.SourceLoad, u, lrow("a1", "char", "g2", "옛 응답")))
	late := turnSource(s, 0)
	if late.Key() == before.Key() {
		t.Error("even after g2 original arrives with same body, old g1 job is reused")
	}
}

func TestA24CoordinateUpgrade(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lifecycle.Row{Role: "user", Time: 7, Data: "입력"}, lifecycle.Row{Role: "char", Data: "응답"}))
	before := turnSource(s, 0)
	j, _, _ := a24Save(t, db, before, `{"memories":[{"id":"m1","kind":"fact","body":"사실"}]}`)
	s.Apply(lsnap(lifecycle.SourceLoad, lifecycle.Row{Role: "user", ChatID: "u1", Time: 7, Data: "입력"}, lifecycle.Row{Role: "char", ChatID: "a1", Data: "응답"}))
	after := turnSource(s, 0)
	next, err := db.JobFor(after)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before=%+v after=%+v turns=%d jobs=%d/%d", before, after, len(s.ActiveTurns()), j.ID, next.ID)
	if before.Turn != after.Turn || before.Responses[0].Seq != after.Responses[0].Seq {
		t.Fatal("fixture did not preserve turn/revision")
	}
	if j.ID != next.ID {
		t.Fatal("optional row-key upgrade duplicated generation job")
	}
}

func TestA24LateOriginalAndSameGenerationEdit(t *testing.T) {
	s := lifecycle.NewSession("chat:A")
	u := lrow("u1", "user", "", "입력")
	unknown := lrow("a1", "char", "g1", "")
	unknown.Unreadable = true
	s.Apply(lsnap(lifecycle.SourceOutputEvent, u, unknown))
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, u, unknown, lrow("n", "user", "", "다음"))) // the next input confirms
	before := turnSource(s, 0)
	if !s.ActiveTurns()[0].Confirmed || before.Readable() || before.Responses[0].Seq != 0 {
		t.Fatal("initial unreadable fixture")
	}
	s.Apply(lsnap(lifecycle.SourceLoad, u, lrow("a1", "char", "g1", "늦은 원문")))
	after := turnSource(s, 0)
	if !s.ActiveTurns()[0].Confirmed || !after.Readable() || before.Key() == after.Key() {
		t.Fatal("late original lost confirmation or source identity")
	}
	s.Apply(lsnap(lifecycle.SourceLoad, u, lrow("a1", "char", "g1", "편집한 원문")))
	edited := turnSource(s, 0)
	if !s.ActiveTurns()[0].Confirmed || edited.Key() == after.Key() {
		t.Fatal("same-generation edit not distinguished")
	}
	revs := s.ActiveTurns()[0].Responses[0].Revisions
	if revs[len(revs)-1].Confirmed {
		t.Fatal("fixture changed revision confirmation")
	}
}

func TestA24FullDetailRoundTrip(t *testing.T) {
	db := testDB(t)
	raw := `{"memories":[{"id":"m1","kind":"event","body":"10냥 중 3냥을 주어 7냥","q":"인용","imp":0,"at":"M88","was":["M1.1","M2"],"scene":"S1","roles":{"actor":["N1"],"target":["N2"],"object":["N3"],"place":["N4"],"speaker":["N5"],"listener":["N6"]},"time":"이틀 전","effects":[{"about":"N1","slot":"돈","before":"10냥","value":"7냥","delta":"-3냥","was":"M1.1"},{"about":"M4","status":"partial"}],"outcome":"partial","setting":"흐린 날","seen_by":["N2"],"knows":[{"who":"N2","via":"전언","from":"N5","extent":"부분","part":"금액","ref":"M9","q":"들었다"}],"unaware":["N3"],"hide_from":["N4"],"allow_to":["public_narration"]},{"id":"m2","kind":"fact","body":"매일 저녁 연락","about":"N1","slot":"연락","mode":"의무","to":"N2","scope":"여행 중","terms":"매일 저녁, 예외는 통신 불능","status":"active"},{"id":"m3","kind":"view","body":"믿지만 불안","owner":"N2","about":"M9","aspect":"emotion","basis":{"via":"전언","from":"N5","ref":"M9","confidence":"아마"},"affect":{"intensity":"조금","persistence":"남음"},"expressed":"속으로만","differs":"unknown"}]}`
	j, it, _ := a24Save(t, db, turnSource(twoTurnSession(), 0), raw)
	original := parse(t, raw)
	for i, m := range it.Memories {
		want := original.Memories[i]
		want.ID = m.ID
		if !reflect.DeepEqual(want, m.Memory) {
			t.Errorf("detail mismatch %d: want=%+v got=%+v", i, want, m.Memory)
		}
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM effects WHERE memory_id=(SELECT id FROM memories WHERE job_id=? AND kind='event')`, j.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("effects count=%d err=%v", n, err)
	}
	var before, value, delta, was string
	if err := db.sql.QueryRow(`SELECT before_value,value,delta,was FROM effects WHERE memory_id=(SELECT id FROM memories WHERE job_id=? AND kind='event') AND k=1`, j.ID).Scan(&before, &value, &delta, &was); err != nil {
		t.Fatal(err)
	}
	if before != "10냥" || value != "7냥" || delta != "-3냥" || was != `["M1.1"]` {
		t.Fatal("effect SQL value loss")
	}
	var zero float64
	if err := db.sql.QueryRow(`SELECT imp FROM memories WHERE job_id=? AND kind='event'`, j.ID).Scan(&zero); err != nil || zero != 0 {
		t.Fatalf("SQL imp zero: %v %v", zero, err)
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM memories WHERE job_id=? AND imp IS NULL`, j.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("SQL imp null count=%d err=%v", n, err)
	}
	if len(it.Awareness) != 4 {
		t.Fatalf("SQL awareness rows=%d", len(it.Awareness))
	}
	kinds := map[string]string{}
	for _, a := range it.Awareness {
		if a.Target != it.Memories[0].ID || a.Job != j.ID {
			t.Fatal("awareness provenance lost")
		}
		kinds[a.Kind] = a.Who
		if a.Kind == "knows" && !reflect.DeepEqual(a.Knows, original.Memories[0].Knows[0]) {
			t.Fatalf("SQL knowledge detail lost: %+v", a.Knows)
		}
	}
	if !reflect.DeepEqual(kinds, map[string]string{"knows": "N2", "unaware": "N3", "hide_from": "N4", "allow_to": "public_narration"}) {
		t.Fatalf("knowledge/disclosure kinds mixed: %v", kinds)
	}
	var status string
	if err := db.sql.QueryRow(`SELECT status FROM effects WHERE memory_id=(SELECT id FROM memories WHERE job_id=? AND kind='event') AND k=2`, j.ID).Scan(&status); err != nil || status != "partial" {
		t.Fatalf("SQL obligation effect=%s err=%v", status, err)
	}
	rows, err := db.sql.Query(`SELECT target FROM memory_was WHERE memory_id=(SELECT id FROM memories WHERE job_id=? AND kind='event') ORDER BY ord`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(targets, []string{"M1.1", "M2"}) {
		t.Fatalf("SQL was array lost: %v", targets)
	}
	var storedRaw string
	if err := db.sql.QueryRow(`SELECT response FROM gen_jobs WHERE job_id=?`, j.ID).Scan(&storedRaw); err != nil || storedRaw != raw {
		t.Fatal("raw response loss")
	}
}

func TestA24EffectWasArray(t *testing.T) {
	var out memory.Output
	err := json.Unmarshal([]byte(`{"memories":[{"id":"m1","kind":"event","body":"두 기록의 값을 바꿈","effects":[{"about":"N1","slot":"돈","value":"20냥","was":["M1.1","M2.1"]}]}]}`), &out)
	if err != nil {
		t.Fatalf("effect was array cannot enter storage: %v", err)
	}
}

func TestA24CrossKindTempCollision(t *testing.T) {
	db := testDB(t)
	_, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"entities":[{"id":"x1","name":"서진"}],"memories":[{"id":"x1","kind":"fact","body":"알고 있다","about":"x1"}]}`)
	if it.Memories[0].About == it.Memories[0].ID || len(notes) == 0 {
		t.Fatalf("cross-kind collision silently becomes self reference: name=%s memory=%s about=%s notes=%v", it.Names[0].ID, it.Memories[0].ID, it.Memories[0].About, notes)
	}
}

func TestA24PlainTextIsNotRewritten(t *testing.T) {
	db := testDB(t)
	_, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"entities":[{"id":"n1","name":"n1","q":"n1"}],"memories":[{"id":"m1","kind":"event","body":"n1","q":"n1","setting":"n1","roles":{"actor":["서진"]},"effects":[{"about":"n1","value":"n1","before":"n1","delta":"n1"}]}]}`)
	m := it.Memories[0]
	if it.Names[0].Name != "n1" || m.Body != "n1" || m.Q != "n1" || m.Setting != "n1" || m.Roles.Actor[0] != "서진" || m.Effects[0].Value != "n1" || m.Effects[0].About != it.Names[0].ID || len(notes) != 0 {
		t.Fatalf("text/ID confusion: %+v notes=%v", it, notes)
	}
}

func TestA24RetryEditRegenerateAndNewRow(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	initial := lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a1", "char", "g1", "첫 응답"))
	s.Apply(initial)
	first := turnSource(s, 0)
	j1, _, _ := a24Save(t, db, first, `{"memories":[{"id":"m1","kind":"fact","body":"첫 사실"}]}`)
	s.Apply(initial)
	retry, err := db.JobFor(turnSource(s, 0))
	if err != nil || retry.ID != j1.ID {
		t.Fatalf("retry job=%+v err=%v", retry, err)
	}
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "수정 입력")))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "수정 입력"), lrow("a2", "char", "g2", "수정 응답")))
	edited := turnSource(s, 0)
	if edited.Turn != first.Turn || edited.Key() == first.Key() || edited.Members[0].Data != "수정 입력" {
		t.Fatalf("edit/regenerate: %+v -> %+v", first, edited)
	}
	j2, _, _ := a24Save(t, db, edited, `{"memories":[{"id":"m1","kind":"fact","body":"수정 사실"}]}`)
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "수정 입력"), lrow("a2", "char", "g2", "수정 응답"), lrow("u2", "user", "", "수정 입력"), lrow("a3", "char", "g3", "새 턴 응답")))
	newSrc := turnSource(s, 1)
	if newSrc.Turn == edited.Turn {
		t.Fatal("identical text in new user row replaced previous turn")
	}
	j3, _, _ := a24Save(t, db, newSrc, `{"memories":[{"id":"m1","kind":"fact","body":"새 턴 사실"}]}`)
	jobs, err := db.CurrentJobs([]memory.Source{edited, newSrc})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jobs, []int64{j2.ID, j3.ID}) {
		t.Fatalf("current jobs %v", jobs)
	}
	var sourceText string
	if err := db.sql.QueryRow(`SELECT source FROM gen_jobs WHERE job_id=?`, j1.ID).Scan(&sourceText); err != nil {
		t.Fatal(err)
	}
	var stored memory.Source
	if err := json.Unmarshal([]byte(sourceText), &stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, stored) {
		t.Fatal("old input source not preserved")
	}
}
