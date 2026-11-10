package store

import (
	"encoding/json"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// Storage contract tests with written outputs (not generation quality).

func twoTurnSession() *lifecycle.Session {
	s := lifecycle.NewSession("chat:M")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "돈을 갚았다"), lrow("a1", "char", "g1", "한얼은 30냥을 갚았다")))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "돈을 갚았다"), lrow("a1", "char", "g1", "한얼은 30냥을 갚았다"),
		lrow("u2", "user", "", "다음"), lrow("a2", "char", "g2", "서진은 그 일을 몰랐다")))
	return s
}

func turnSource(s *lifecycle.Session, i int) memory.Source {
	return memory.SourceOf(s.Key, s.ActiveTurns()[i])
}

func count(t *testing.T, d *DB, table string) int {
	t.Helper()
	var n int
	if err := d.sql.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func parse(t *testing.T, s string) memory.Output {
	t.Helper()
	var o memory.Output
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

const turn1Output = `{
 "entities":[{"id":"n1","name":"강한얼","type":"person","q":"한얼은"},
             {"id":"n2","name":"민재","type":"person","q":"민재","same_as":"n1","how":"disguise","relation":"r1","unaware":["서진"]}],
 "memories":[
  {"id":"m1","kind":"event","body":"한얼이 빚 30냥을 갚았다","q":"30냥을 갚았다","imp":0,
   "roles":{"actor":["n1"]},"effects":[{"about":"n1","slot":"돈","before":"50냥","value":"20냥","delta":"-30"}],"outcome":"success"},
  {"id":"m2","kind":"fact","about":"n1","slot":"돈","mode":"상태","body":"20냥","q":"30냥을 갚았다","at":"m1","was":"m1.1"},
  {"id":"m3","kind":"view","owner":"n1","about":"m1","aspect":"emotion","body":"후련함","basis":{"via":"내면 서술","ref":"m1"},
   "affect":{"intensity":"몹시"},"knows":[{"who":"n2","via":"직접","ref":"m1"}]},
  {"id":"m4","kind":"fact","body":"잘못된 참조","was":"m9"}
 ],
 "scenes":[{"id":"s1","change":"open","q":"주막"}],
 "links":[{"id":"l1","from":"m1","rel":"causes","to":"m3","hide_from":["서진"]}]
}`

func TestGenerationStoredWithPersistentIDs(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	job, err := db.JobFor(turnSource(s, 0))
	if err != nil {
		t.Fatal(err)
	}
	notes, err := db.SaveGeneration(job, Generation{Model: "test", Request: "req", Response: turn1Output, Output: parse(t, turn1Output)})
	if err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf([]int64{job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Names) != 2 || len(it.Memories) != 4 || len(it.Links) != 1 || len(it.Scenes) != 1 || len(it.Relations) != 1 {
		t.Fatalf("items: names %d memories %d links %d scenes %d relations %d", len(it.Names), len(it.Memories), len(it.Links), len(it.Scenes), len(it.Relations))
	}
	n1, m1 := it.Names[0].ID, it.Memories[0].ID
	ev, fact, view := it.Memories[0], it.Memories[1], it.Memories[2]
	if ev.Effects[0].About != n1 || ev.Roles.Actor[0] != n1 || ev.Outcome != "success" {
		t.Fatalf("event refs: %+v", ev.Memory)
	}
	if fact.At != m1 || len(fact.Was) != 1 || fact.Was[0] != m1+".1" || fact.About != n1 {
		t.Fatalf("fact refs at=%s was=%v about=%s (want %s, %s.1, %s)", fact.At, fact.Was, fact.About, m1, m1, n1)
	}
	if view.Basis.Ref != m1 || view.Affect.Intensity != "몹시" || view.Knows[0].Who != it.Names[1].ID {
		t.Fatalf("view: %+v", view.Memory)
	}
	if ev.Imp == nil || *ev.Imp != 0 || fact.Imp != nil {
		t.Fatalf("imp 0 must stay 0 and missing must stay missing: %v %v", ev.Imp, fact.Imp)
	}
	if it.Links[0].From != m1 || it.Links[0].To != view.ID || it.Relations[0].SameAs != n1 {
		t.Fatalf("link/relation: %+v %+v", it.Links[0], it.Relations[0])
	}
	// knowledge of the identity applies to the relation, not the name
	var rel, linkHide bool
	for _, a := range it.Awareness {
		rel = rel || (a.Kind == "unaware" && a.Target == it.Relations[0].ID && a.Who == "서진")
		linkHide = linkHide || (a.Kind == "hide_from" && a.Target == it.Links[0].ID)
	}
	if !rel || !linkHide {
		t.Fatalf("awareness targets: %+v", it.Awareness)
	}
	// an id nobody issued is kept as written and noted
	if bad := it.Memories[3]; bad.Was[0] != "m9" || !strings.Contains(strings.Join(notes, " "), "unresolved reference m9") {
		t.Fatalf("unresolved ref: %v notes %v", bad.Was, notes)
	}
}

func TestSameSourceSameJobAndResultStoredOnce(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	a, _ := db.JobFor(turnSource(s, 0))
	b, _ := db.JobFor(turnSource(s, 0))
	if a.ID != b.ID {
		t.Fatalf("same source gave jobs %d and %d", a.ID, b.ID)
	}
	g := Generation{Response: turn1Output, Output: parse(t, turn1Output)}
	if _, err := db.SaveGeneration(a, g); err != nil {
		t.Fatal(err)
	}
	before := count(t, db, "memories") + count(t, db, "awareness") + count(t, db, "names")
	if _, err := db.SaveGeneration(b, g); err != nil {
		t.Fatal(err)
	}
	if after := count(t, db, "memories") + count(t, db, "awareness") + count(t, db, "names"); after != before {
		t.Fatalf("resaved result added rows: %d -> %d", before, after)
	}
}

// A reroll gives the turn a new source: the earlier job is no longer
// current, its rows stay as history, and another turn's re-mention of a
// memory from turn 1 does not keep a rerolled turn's own memories alive.
func TestRerollMakesEarlierJobHistory(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	j1, _ := db.JobFor(turnSource(s, 0))
	if _, err := db.SaveGeneration(j1, Generation{Output: parse(t, turn1Output)}); err != nil {
		t.Fatal(err)
	}
	it, _ := db.ItemsOf([]int64{j1.ID})
	m1 := it.Memories[0].ID
	j2, _ := db.JobFor(turnSource(s, 1))
	t2 := `{"memories":[{"ref":"` + m1 + `","q":"그 일","unaware":["서진"]},{"id":"m1","kind":"view","owner":"서진","about":"` + m1 + `","aspect":"belief","body":"모른다"}]}`
	if _, err := db.SaveGeneration(j2, Generation{Output: parse(t, t2)}); err != nil {
		t.Fatal(err)
	}
	cur := func() []int64 {
		jobs, err := db.CurrentJobs([]memory.Source{turnSource(s, 0), turnSource(s, 1)})
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	if got := cur(); len(got) != 2 {
		t.Fatalf("current jobs %v", got)
	}
	// reroll turn 2
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "돈을 갚았다"), lrow("a1", "char", "g1", "한얼은 30냥을 갚았다"), lrow("u2", "user", "", "다음")))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "돈을 갚았다"), lrow("a1", "char", "g1", "한얼은 30냥을 갚았다"),
		lrow("u2", "user", "", "다음"), lrow("a2b", "char", "g3", "서진이 알아챘다")))
	got := cur()
	if len(got) != 1 || got[0] != j1.ID {
		t.Fatalf("after reroll current jobs %v, want only %d", got, j1.ID)
	}
	items, _ := db.ItemsOf(got)
	if len(items.Mentions) != 0 || len(items.Memories) != 4 {
		t.Fatalf("current items: mentions %d memories %d", len(items.Mentions), len(items.Memories))
	}
	if all, _ := db.ItemsOf([]int64{j1.ID, j2.ID}); len(all.Mentions) != 1 || len(all.Memories) != 5 {
		t.Fatalf("history lost: mentions %d memories %d", len(all.Mentions), len(all.Memories))
	}
}

// 4-3 checklist: a ref that adds awareness changes the scope of the stored
// item without copying it (another knower's record stays); a plain ref only
// adds evidence; rerolling the turn that added awareness drops that change
// from current reads and keeps the earlier one.
func TestRefAwarenessUpdatesScopeWithoutCopy(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	j1, _ := db.JobFor(turnSource(s, 0))
	if _, err := db.SaveGeneration(j1, Generation{Output: parse(t, turn1Output)}); err != nil {
		t.Fatal(err)
	}
	it, _ := db.ItemsOf([]int64{j1.ID})
	m1, view := it.Memories[0].ID, it.Memories[2].ID
	j2, _ := db.JobFor(turnSource(s, 1))
	t2 := `{"memories":[{"ref":"` + view + `","knows":[{"who":"서진","via":"전언","extent":"부분","part":"후련함"}]},{"ref":"` + m1 + `","q":"그 일"}]}`
	if _, err := db.SaveGeneration(j2, Generation{Output: parse(t, t2)}); err != nil {
		t.Fatal(err)
	}
	both, _ := db.ItemsOf([]int64{j1.ID, j2.ID})
	if len(both.Memories) != 4 {
		t.Fatalf("a ref copied a memory: %d", len(both.Memories))
	}
	var knowers []string
	for _, a := range both.Awareness {
		if a.Target == view && a.Kind == "knows" {
			knowers = append(knowers, a.Who+"/"+a.Extent)
		}
	}
	if strings.Join(knowers, ",") != it.Names[1].ID+"/,서진/부분" {
		t.Fatalf("knowers of the view: %v", knowers)
	}
	var plain, scoped int
	for _, m := range both.Mentions {
		if m.Target == m1 && m.Q == "그 일" {
			plain++
		}
		if m.Target == view {
			scoped++
		}
	}
	if plain != 1 || scoped != 0 {
		t.Fatalf("mentions: plain %d, awareness-only ref recorded as mention %d", plain, scoped)
	}
	// reroll turn 2: its awareness leaves current reads, turn 1's stays
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "돈을 갚았다"), lrow("a1", "char", "g1", "한얼은 30냥을 갚았다"), lrow("u2", "user", "", "다음")))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "돈을 갚았다"), lrow("a1", "char", "g1", "한얼은 30냥을 갚았다"),
		lrow("u2", "user", "", "다음"), lrow("a2b", "char", "g3", "아무도 몰랐다")))
	jobs, _ := db.CurrentJobs([]memory.Source{turnSource(s, 0), turnSource(s, 1)})
	k, _ := db.KnownOf(jobs)
	for _, a := range k.Awareness {
		if a.Target == view && a.Who == "서진" {
			t.Fatal("rerolled turn's awareness still current")
		}
	}
	if len(k.Awareness) != len(it.Awareness) {
		t.Fatalf("turn 1 awareness changed: %d -> %d", len(it.Awareness), len(k.Awareness))
	}
}

// 4-3 checklist: a new entity with a stored name gets its own id (never
// merged by name), both are read with their quotes; a scene continued by
// ref keeps its id; entity type and quote round-trip.
func TestSameNameNewIDAndSceneContinue(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	j1, _ := db.JobFor(turnSource(s, 0))
	db.SaveGeneration(j1, Generation{Output: parse(t, `{"entities":[{"id":"n1","name":"민재","type":"person","q":"주막의 민재"}],"scenes":[{"id":"s1","change":"open"}]}`)})
	it1, _ := db.ItemsOf([]int64{j1.ID})
	j2, _ := db.JobFor(turnSource(s, 1))
	db.SaveGeneration(j2, Generation{Output: parse(t, `{"entities":[{"id":"n1","name":"민재","type":"person","q":"상단의 민재"}],"scenes":[{"ref":"`+it1.Scenes[0].Ref+`","change":"continue"}]}`)})
	k, _ := db.KnownOf([]int64{j1.ID, j2.ID})
	if len(k.Names) != 2 || k.Names[0].ID == k.Names[1].ID || k.Names[0].Type != "person" || k.Names[1].Q != "상단의 민재" {
		t.Fatalf("names %+v", k.Names)
	}
	if len(k.Scenes) != 2 || k.Scenes[1].ID != k.Scenes[0].ID || k.Scenes[1].Change != "continue" || count(t, db, "scenes") != 1 {
		t.Fatalf("scenes %+v (scene rows %d)", k.Scenes, count(t, db, "scenes"))
	}
	out := memory.RenderKnown(memory.Select(k, "민재가 왔다", nil), nil)
	if !strings.Contains(out, "주막의 민재") || !strings.Contains(out, "상단의 민재") || !strings.Contains(out, "current scene: "+k.Scenes[0].ID) {
		t.Fatalf("render:\n%s", out)
	}
}

// 4-5: the read later steps use gives the current memory of the chat's read
// scope: a branch sees its parent's memory up to the branch point and its
// own, not the parent's later turns; a rerolled turn's earlier memory is
// history.
func TestCurrentKnownFollowsScopeAndTurnChanges(t *testing.T) {
	db := testDB(t)
	save := func(src memory.Source, body string) {
		j, err := db.JobFor(src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.SaveGeneration(j, Generation{Output: parse(t, `{"memories":[{"id":"m1","kind":"fact","body":"`+body+`"}]}`)}); err != nil {
			t.Fatal(err)
		}
	}
	at := func(key string, src lifecycle.Source, rows ...lifecycle.Row) lifecycle.Snapshot {
		sn := lsnap(src, rows...)
		sn.SessionKey = key
		return sn
	}
	parent := lifecycle.NewSession("chat:P")
	parent.Apply(at("chat:P", lifecycle.SourceOutputEvent, lrow("u1", "user", "", "하나"), lrow("a1", "char", "g1", "첫째")))
	parent.Apply(at("chat:P", lifecycle.SourceOutputEvent, lrow("u1", "user", "", "하나"), lrow("a1", "char", "g1", "첫째"), lrow("u2", "user", "", "둘"), lrow("a2", "char", "g2", "둘째")))
	db.SaveSession(parent, parent.TakeEvents(), parent.TakeRenames())
	save(memory.SourceOf(parent.Key, parent.ActiveTurns()[0]), "부모 첫 턴")
	save(memory.SourceOf(parent.Key, parent.ActiveTurns()[1]), "부모 둘째 턴")
	marker := lifecycle.Row{ChatID: "mk", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::P::이름::a1::}}"}
	child := lifecycle.NewSession("chat:C")
	child.Apply(at("chat:C", lifecycle.SourceOutputEvent, lrow("u1", "user", "", "하나"), lrow("a1", "char", "g1", "첫째"), marker, lrow("u3", "user", "", "셋"), lrow("a3", "char", "g3", "분기")))
	// the pipeline records the branch's copies at the observation
	ok, err := memory.RecordCopies(child, db.LoadSession)
	if err != nil || !ok {
		t.Fatalf("record %v %v", ok, err)
	}
	own := child.ActiveTurns()[len(child.ActiveTurns())-1]
	save(memory.SourceOf(child.Key, own), "분기 턴")
	bodies := func() string {
		k, err := db.CurrentKnown(child)
		if err != nil {
			t.Fatal(err)
		}
		var b []string
		for _, m := range k.Memories {
			b = append(b, m.Body)
		}
		return strings.Join(b, ",")
	}
	if got := bodies(); got != "부모 첫 턴,분기 턴" {
		t.Fatalf("branch scope: %s", got)
	}
	// reroll the branch's own turn: its earlier memory leaves current reads
	child.Apply(at("chat:C", lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "하나"), lrow("a1", "char", "g1", "첫째"), marker, lrow("u3", "user", "", "셋")))
	child.Apply(at("chat:C", lifecycle.SourceOutputEvent, lrow("u1", "user", "", "하나"), lrow("a1", "char", "g1", "첫째"), marker, lrow("u3", "user", "", "셋"), lrow("a4", "char", "g4", "다시")))
	if got := bodies(); got != "부모 첫 턴" {
		t.Fatalf("after reroll: %s", got)
	}
}

// A memory item written without an id but with content of its own is a new
// record, not dropped: it gets an id. An item with nothing in it is still
// skipped and noted.
func TestMemoryWithoutIDIsKept(t *testing.T) {
	db := testDB(t)
	_, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"memories":[{"kind":"fact","about":"N1","body":"돈이 15냥 있다"},{"q":"아무것도"},{"id":"m1","kind":"event","body":"샀다"}]}`)
	var bodies []string
	for _, m := range it.Memories {
		bodies = append(bodies, m.Body)
	}
	if strings.Join(bodies, ",") != "돈이 15냥 있다,샀다" {
		t.Fatalf("stored %v notes %v", bodies, notes)
	}
	j := strings.Join(notes, " | ")
	if !strings.Contains(j, "a memory without id was given a new id") || !strings.Contains(j, "memory without id, ref or content skipped") {
		t.Fatalf("notes %v", notes)
	}
}

// A long id the model wrote does not stop the answer from being stored: the
// item gets its id and the id map keeps the written id as it is.
func TestLongTemporaryIDIsStored(t *testing.T) {
	db := testDB(t)
	long := strings.Repeat("m", 70) + "1"
	j, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"memories":[{"id":"`+long+`","kind":"fact","body":"정상 본문"}]}`)
	if len(it.Memories) != 1 || it.Memories[0].Body != "정상 본문" {
		t.Fatalf("stored %+v notes %v", it.Memories, notes)
	}
	ids, err := db.ItemIDs(j.ID)
	if err != nil || ids["memories/0"] == "" {
		t.Fatalf("ids %v %v", ids, err)
	}
}
