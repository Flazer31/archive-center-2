package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

func u2Session(t *testing.T, db *DB, key string, texts ...string) *lifecycle.Session {
	t.Helper()
	s := lifecycle.NewSession(key)
	var rows []lifecycle.Row
	for i, txt := range texts {
		rows = append(rows, lrow(fmt.Sprintf("%su%d", key, i), "user", "", txt), lrow(fmt.Sprintf("%sa%d", key, i), "char", "", txt+" 답"))
		s.Apply(lsnap(lifecycle.SourceOutputEvent, append([]lifecycle.Row(nil), rows...)...))
	}
	s.Progress["u2"] = 10
	if err := db.SaveSession(s, s.TakeEvents(), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChatLabel(key, "시험 인물", key); err != nil {
		t.Fatal(err)
	}
	return s
}

func u2Save(t *testing.T, db *DB, s *lifecycle.Session, i int, output string) (Job, Items) {
	t.Helper()
	j, err := db.JobFor(turnSource(s, i))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveGeneration(j, Generation{Model: "fake", Request: "N1 is literal prose", ReadItems: "kept input", Response: output, Output: parse(t, output)}); err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	return j, it
}

const u2Output = `{"entities":[{"id":"n1","name":"동명","type":"person"},{"id":"n2","name":"동명","type":"person","same_as":"n1","relation":"r1","how":"disguise","hide_from":["n1"]}],"scenes":[{"id":"s1","change":"open"}],"memories":[{"id":"m1","kind":"event","body":"약속을 지켰다 N1","q":"원문 N1","scene":"s1","roles":{"actor":["n1"],"target":["n2"]},"effects":[{"about":"n1","slot":"문","value":"열림"}]},{"id":"m2","kind":"fact","body":"열린 문","about":"n1","at":"m1","was":"m1.1","knows":[{"who":"n2","ref":"m1","from":"n1"}]},{"id":"m3","kind":"view","body":"안도함","owner":"n2","about":"m1","basis":{"ref":"m1"},"hide_from":["n1"]},{"ref":"m1","q":"약속 재언급"}],"links":[{"id":"l1","from":"m1.1","to":"m3","rel":"causes","hide_from":["n1"]}]}`

func TestU2CopyCurrentJobsAndMatching(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:A", "같음", "다름", "밀린 턴", "기존")
	d := u2Session(t, db, "chat:B", "같음", "다른 본문", "삽입", "기존", "밀린 턴")
	_, source := u2Save(t, db, s, 0, u2Output)
	for i := 1; i < len(s.ActiveTurns()); i++ {
		u2Save(t, db, s, i, `{"memories":[{"id":"m1","kind":"fact","body":"원본"}]}`)
	}
	_, existing := u2Save(t, db, d, 3, `{"memories":[{"id":"m1","kind":"fact","body":"대상 유지"}]}`)
	hash := TextHash("shared exact document")
	if err := db.SetDocHashes(map[string]string{source.Memories[0].ID: hash}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbeddings("u2", []string{hash}, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	// An old revision's job is history, never copied as the current result.
	old := turnSource(s, 0)
	old.Responses = append([]memory.SourceResponse(nil), old.Responses...)
	old.Responses[0].Seq++
	j, err := db.JobFor(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveGeneration(j, Generation{Output: parse(t, `{"memories":[{"id":"m1","kind":"fact","body":"옛 작업 제외"}]}`)}); err != nil {
		t.Fatal(err)
	}
	before := u2Dump(t, db)
	preview, err := db.ChatOperation("copy", s.Key, d.Key, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, u2Dump(t, db)) {
		t.Fatal("dry_run changed database rows or auto-increment values")
	}
	got, err := db.ChatOperation("copy", s.Key, d.Key, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.CopiedTurns != 1 || got.UnmatchedTurns != 2 || got.ExistingTurns != 1 || got.CopiedMemories != len(source.Memories) {
		t.Fatalf("counts: %+v", got)
	}
	preview.Changed, got.Changed = nil, nil
	if !reflect.DeepEqual(preview, got) {
		t.Fatalf("preview differs: %+v / %+v", preview, got)
	}
	js, err := db.CurrentJobs([]memory.Source{turnSource(d, 0)})
	if err != nil || len(js) != 1 {
		t.Fatalf("target jobs %v %v", js, err)
	}
	it, err := db.ItemsOf(js)
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Names) != len(source.Names) || len(it.Relations) != len(source.Relations) || len(it.Memories) != len(source.Memories) || len(it.Links) != len(source.Links) || len(it.Scenes) != len(source.Scenes) || len(it.Awareness) != len(source.Awareness) || len(it.Mentions) != len(source.Mentions) {
		t.Fatalf("lost rows: %+v", it)
	}
	if it.Names[0].ID == it.Names[1].ID || it.Names[0].ID == source.Names[0].ID {
		t.Fatal("name occurrence identity was merged or not reissued")
	}
	event, fact, view := it.Memories[0], it.Memories[1], it.Memories[2]
	if event.Roles.Actor[0] != it.Names[0].ID || event.Roles.Target[0] != it.Names[1].ID || event.Effects[0].About != it.Names[0].ID || fact.At != event.ID || fact.Was[0] != event.ID+".1" || view.Basis.Ref != event.ID || view.Owner != it.Names[1].ID || it.Links[0].From != event.ID+".1" || it.Links[0].To != view.ID || it.Relations[0].Name != it.Names[1].ID || it.Relations[0].SameAs != it.Names[0].ID {
		t.Fatalf("references not remapped: %+v", it)
	}
	if event.Body != source.Memories[0].Body || event.Q != source.Memories[0].Q {
		t.Fatal("prose rewritten as IDs")
	}
	var doc string
	if err := db.sql.QueryRow(`SELECT doc_hash FROM memories WHERE id=?`, strings.TrimPrefix(event.ID, "M")).Scan(&doc); err != nil || doc != hash {
		t.Fatalf("document hash changed: %q %v", doc, err)
	}
	if _, ok, err := db.Embedding("u2", hash); err != nil || !ok {
		t.Fatalf("shared vector lost %v", err)
	}
	remaining, err := db.ItemsOf([]int64{existing.Memories[0].Job})
	if err != nil || remaining.Memories[0].Body != "대상 유지" {
		t.Fatal("existing destination overwritten")
	}
	if count(t, db, "embedding_vectors_3") != 1 {
		t.Fatal("copy changed vector population")
	}
	for _, table := range []string{"gen_items", "search_refs", "search_words", "effects", "memory_was"} {
		if count(t, db, table) == 0 {
			t.Fatalf("missing copied %s", table)
		}
	}
	// Repeating copy leaves all populated destination turns alone.
	again, err := db.ChatOperation("copy", s.Key, d.Key, false)
	if err != nil || again.CopiedTurns != 0 || again.ExistingTurns != 2 {
		t.Fatalf("repeat %+v %v", again, err)
	}
}

// Include every table and its next generated ID: a preview that writes and
// rolls back would still consume AUTO_INCREMENT and fail this snapshot.
func u2Dump(t *testing.T, db *DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := db.sql.Query(`SELECT TABLE_NAME,COALESCE(AUTO_INCREMENT,0) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() ORDER BY TABLE_NAME`)
	if err != nil {
		t.Fatal(err)
	}
	type table struct {
		name string
		next int64
	}
	var tables []table
	for rows.Next() {
		var x table
		if err := rows.Scan(&x.name, &x.next); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, x)
	}
	rows.Close()
	for _, x := range tables {
		r, err := db.sql.Query("SELECT * FROM " + x.name)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		var records []string
		for r.Next() {
			v := make([]any, len(cols))
			p := make([]any, len(cols))
			for i := range v {
				p[i] = &v[i]
			}
			if err := r.Scan(p...); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(v)
			records = append(records, string(b))
		}
		r.Close()
		sort.Strings(records)
		out[x.name] = fmt.Sprint(x.next) + strings.Join(records, "\n")
	}
	return out
}

func TestU2DeleteEveryOwnedTableAndSharedVectors(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:A", "하나")
	other := u2Session(t, db, "chat:B", "둘")
	j, it := u2Save(t, db, s, 0, u2Output)
	_, keep := u2Save(t, db, other, 0, `{"memories":[{"id":"m1","kind":"fact","body":"다른 채팅"}]}`)
	if err := db.Grant(s.Key, []int{s.ActiveTurns()[0].ID}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO session_branch VALUES ('chat:A','chat:gone','point',NULL,NULL,NULL)`, `INSERT INTO branch_copies VALUES ('chat:A',1,'k','{}')`, `INSERT INTO raw_observations (received_at,receive_seq,chat_id,event) VALUES (NOW(),1,'A','{}')`, `INSERT INTO raw_observations (received_at,receive_seq,chat_id,chat_id2,event) VALUES (NOW(),2,'B','A','other observation')`} {
		if _, err := db.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	shared, unique, orphan := TextHash("shared"), TextHash("unique"), TextHash("unrelated existing orphan")
	if err := db.SetDocHashes(map[string]string{it.Memories[0].ID: shared, it.Memories[1].ID: unique, keep.Memories[0].ID: shared}); err != nil {
		t.Fatal(err)
	}
	for _, dim := range []int{3, 4} {
		vs := make([][]float32, 3)
		for i := range vs {
			vs[i] = make([]float32, dim)
			vs[i][0] = 1
		}
		if err := db.SaveEmbeddings(fmt.Sprint(dim), []string{shared, unique, orphan}, vs); err != nil {
			t.Fatal(err)
		}
	}
	before := u2Dump(t, db)
	if _, err := db.ChatOperation("delete", s.Key, "", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, u2Dump(t, db)) {
		t.Fatal("delete preview wrote data")
	}
	if _, err := db.ChatOperation("delete", s.Key, "", false); err != nil {
		t.Fatal(err)
	}
	r, err := db.sql.Query(`SELECT TABLE_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND COLUMN_NAME='session_key'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for r.Next() {
		var table string
		r.Scan(&table)
		tables = append(tables, table)
	}
	r.Close()
	for _, table := range tables {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE session_key=?`, s.Key).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s still has %d rows: %v", table, n, err)
		}
	}
	var n int
	db.sql.QueryRow(`SELECT COUNT(*) FROM gen_items WHERE job_id=?`, j.ID).Scan(&n)
	if n != 0 {
		t.Fatal("gen_items left")
	}
	for _, table := range []string{"effects", "memory_was"} {
		if count(t, db, table) != 0 {
			t.Fatalf("%s left", table)
		}
	}
	if count(t, db, "raw_observations") != 1 {
		t.Fatal("wrong raw deletion ownership")
	}
	var primary, secondary, event string
	if err := db.sql.QueryRow(`SELECT chat_id,chat_id2,event FROM raw_observations`).Scan(&primary, &secondary, &event); err != nil || primary != "B" || secondary != "A" || event != "other observation" {
		t.Fatalf("other observation changed: %q %q %q %v", primary, secondary, event, err)
	}
	for _, model := range []string{"3", "4"} {
		for _, h := range []string{shared, orphan} {
			if _, ok, err := db.Embedding(model, h); err != nil || !ok {
				t.Fatalf("removed other vector %s %v", h, err)
			}
		}
		if _, ok, err := db.Embedding(model, unique); err != nil || ok {
			t.Fatalf("unused deleted vector remains %v", err)
		}
	}
	if _, ok, err := db.LoadSession(other.Key); err != nil || !ok {
		t.Fatal("other chat removed")
	}
}

func TestU2DeleteParentPreservesChildAndDescendantDelivery(t *testing.T) {
	db := testDB(t)
	p := u2Session(t, db, "chat:P", "약속", "문", "길", "끝")
	_, it := u2Save(t, db, p, 0, u2Output)
	for i := 1; i < len(p.ActiveTurns()); i++ {
		u2Save(t, db, p, i, `{"memories":[{"id":"m1","kind":"fact","body":"별도 기록"}]}`)
	}
	var children []*lifecycle.Session
	for _, key := range []string{"chat:C", "chat:G"} {
		c := u2Session(t, db, key, "약속", "문", "길", "끝", "자식의 뒤", "또 뒤", "최신")
		c.BranchOf = [2]string{p.Key, "parent-point"}
		copies := map[int]lifecycle.Copy{}
		for i := 0; i < 4; i++ {
			src := turnSource(p, i)
			b, _ := json.Marshal(src)
			own := memory.WholeSourceOf(c.Key, c.ActiveTurns()[i])
			copies[own.Turn] = lifecycle.Copy{Own: own.Key(), Source: string(b)}
		}
		if key == "chat:G" {
			c.BranchOf = [2]string{"chat:C", "child-point"}
		}
		c.RecordCopies(c.BranchOf, copies)
		if err := db.SaveSession(c, nil, nil); err != nil {
			t.Fatal(err)
		}
		// This fallback was made before the parent caught up; it is hidden
		// while the parent has its own result and must not surface on delete.
		u2Save(t, db, c, 0, `{"memories":[{"id":"m1","kind":"fact","body":"숨은 옛 자식 결과"}]}`)
		u2Save(t, db, c, 4, `{"memories":[{"id":"m1","kind":"view","body":"약속을 기억함","owner":"`+it.Names[0].ID+`","basis":{"ref":"`+it.Memories[0].ID+`"}}]}`)
		children = append(children, c)
	}
	req := DeliveryRequest{Input: "약속 문", Payload: printsOf(turnSource(children[0], 6)), Chars: 10000, Secret: 10000}
	type evidence struct {
		list     []ListedMemory
		delivery Delivery
	}
	before := map[string]evidence{}
	for _, c := range children {
		list, err := db.ChatMemories(c.Key, memory.Own(c))
		if err != nil {
			t.Fatal(err)
		}
		d, err := db.Deliver(memory.ScopeOf(c), req)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 || len(d.IDs) == 0 {
			t.Fatal("empty parity control")
		}
		before[c.Key] = evidence{list, d}
	}
	preview, err := db.ChatOperation("delete", p.Key, "", true)
	if err != nil || len(preview.Branches) != len(children) {
		t.Fatalf("preview branches %+v %v", preview, err)
	}
	if _, err := db.ChatOperation("delete", p.Key, "", false); err != nil {
		t.Fatal(err)
	}
	for _, old := range children {
		c, ok, err := db.LoadSession(old.Key)
		if err != nil || !ok {
			t.Fatal(err)
		}
		list, err := db.ChatMemories(c.Key, memory.Own(c))
		if err != nil {
			t.Fatal(err)
		}
		want := before[c.Key].list
		if len(list) != len(want) {
			t.Fatalf("%s lost memories: %d -> %d", c.Key, len(want), len(list))
		}
		mapping := map[string]string{}
		for i := range want {
			mapping[want[i].ID] = list[i].ID
		}
		ref := func(s string) string {
			if v, ok := mapping[s]; ok {
				return v
			}
			return s
		}
		for i := range want { // child own later record sorts after its materialized past in story order
			if want[i].Body != list[i].Body {
				t.Fatalf("list order/body changed %s -> %s", want[i].Body, list[i].Body)
			}
			mapping[want[i].ID] = list[i].ID
			if list[i].Inherited {
				t.Fatal("parent memory still inherited")
			}
			if ref(want[i].About) != list[i].About || ref(want[i].Owner) != list[i].Owner || want[i].Secret != list[i].Secret || !reflect.DeepEqual(want[i].Aware, list[i].Aware) {
				t.Fatalf("list meaning/awareness changed %+v -> %+v", want[i], list[i])
			}
		}
		d, err := db.Deliver(memory.ScopeOf(c), req)
		if err != nil {
			t.Fatal(err)
		}
		body := func(block string) string { _, b, _ := strings.Cut(block, "\n"); return b }
		previous := body(before[c.Key].delivery.Block)
		for a, b := range mapping {
			previous = strings.ReplaceAll(previous, a+" ", b+" ")
		}
		// Delivery renders names and record text. Persistent IDs may be new;
		// normalize just those IDs before comparing the complete payload.
		normal := func(s string) string {
			for a, b := range mapping {
				s = strings.ReplaceAll(s, a+".", b+".")
			}
			return s
		}
		if normal(previous) != body(d.Block) {
			t.Fatalf("%s delivery changed\n%s\n%s", c.Key, previous, d.Block)
		}
	}
}

func TestU2MoveCopiesThenDeletesSource(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:A", "같음", "불일치")
	d := u2Session(t, db, "chat:B", "같음", "다름")
	u2Save(t, db, s, 0, u2Output)
	u2Save(t, db, s, 1, `{"memories":[{"id":"m1","kind":"fact","body":"짝 없음"}]}`)
	before := u2Dump(t, db)
	if _, err := db.ChatOperation("move", s.Key, d.Key, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, u2Dump(t, db)) {
		t.Fatal("move preview wrote data")
	}
	r, err := db.ChatOperation("move", s.Key, d.Key, false)
	if err != nil || r.CopiedTurns != 1 || r.UnmatchedTurns != 1 {
		t.Fatalf("move %+v %v", r, err)
	}
	if _, ok, err := db.LoadSession(s.Key); err != nil || ok {
		t.Fatal("source remains")
	}
	got, err := db.ChatMemories(d.Key, memory.Own(d))
	if err != nil || len(got) != len(parse(t, u2Output).Memories)-1 {
		t.Fatalf("destination missing %d %v", len(got), err)
	}
}

func TestU2CopyCrossTurnRefsAndNameDependencies(t *testing.T) {
	// Two distinct source names share their spelling. A later turn refers
	// to each one; the map must span jobs, rather than reset on every turn.
	db := testDB(t)
	s := u2Session(t, db, "chat:A", "정의", "후속")
	d := u2Session(t, db, "chat:B", "정의", "후속")
	_, first := u2Save(t, db, s, 0, u2Output)
	u2Save(t, db, s, 1, `{"memories":[{"id":"m1","kind":"view","owner":"`+first.Names[0].ID+`","about":"`+first.Names[1].ID+`","body":"뒤의 관점","basis":{"ref":"`+first.Memories[0].ID+`"}}],"scenes":[{"ref":"`+first.Scenes[0].Ref+`","change":"continue"}]}`)
	if _, err := db.ChatOperation("copy", s.Key, d.Key, false); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.CurrentJobs([]memory.Source{turnSource(d, 0), turnSource(d, 1)})
	if err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf(jobs)
	if err != nil {
		t.Fatal(err)
	}
	last := it.Memories[len(it.Memories)-1]
	if last.Owner != it.Names[0].ID || last.About != it.Names[1].ID || last.Basis.Ref != it.Memories[0].ID || it.Scenes[1].Ref != it.Scenes[0].Ref {
		t.Fatalf("cross-turn IDs not preserved %+v", it)
	}
	// An unpaired definition must still supply distinct names to the one
	// matched turn, without copying its unrelated records.
	partial := u2Session(t, db, "chat:C", "다른 정의", "후속")
	r, err := db.ChatOperation("copy", s.Key, partial.Key, false)
	if err != nil || r.CopiedTurns != 1 || r.UnmatchedTurns != 1 {
		t.Fatalf("partial %+v %v", r, err)
	}
	js, err := db.CurrentJobs([]memory.Source{turnSource(partial, 1)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.ItemsOf(js)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Names) != len(first.Names) || len(got.Memories) != 1 || got.Memories[0].Owner == got.Memories[0].About {
		t.Fatalf("name dependencies lost/merged %+v", got)
	}
	names, err := db.NamesByID([]string{got.Memories[0].Owner, got.Memories[0].About})
	if err != nil || len(names) != len(first.Names) {
		t.Fatalf("names not owned %v %v", names, err)
	}
}

func TestU2UnpairedNameDefinitionKeepsItsAwareness(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:A", "정의", "후속")
	d := u2Session(t, db, "chat:B", "다른 정의", "후속")
	_, first := u2Save(t, db, s, 0, `{"entities":[{"id":"n1","name":"숨긴 이름","hide_from":["n2"]},{"id":"n2","name":"모르는 사람"}],"memories":[{"id":"m1","kind":"fact","body":"정의 턴"}]}`)
	u2Save(t, db, s, 1, `{"memories":[{"id":"m1","kind":"view","owner":"`+first.Names[0].ID+`","about":"`+first.Names[1].ID+`","body":"후속 관점"}]}`)
	if _, err := db.ChatOperation("copy", s.Key, d.Key, false); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.CurrentJobs([]memory.Source{turnSource(d, 1)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.ItemsOf(jobs)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range got.Awareness {
		if a.Target == got.Memories[0].Owner && a.Kind == "hide_from" && a.Who == got.Memories[0].About {
			found = true
		}
	}
	if !found {
		t.Fatal("referenced name's disclosure policy was lost")
	}
}

func TestU2MoveRollsBackCopyAndDeletionTogether(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:A", "같음")
	d := u2Session(t, db, "chat:B", "같음")
	u2Save(t, db, s, 0, u2Output)
	if _, err := db.sql.Exec(`CREATE TRIGGER u2_failure BEFORE DELETE ON chat_labels FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic deletion failure'`); err != nil {
		t.Fatal(err)
	}
	rowsOnly := func(in map[string]string) map[string]string {
		out := map[string]string{}
		for table, s := range in {
			if i := strings.IndexByte(s, '['); i >= 0 {
				out[table] = s[i:]
			} else {
				out[table] = ""
			}
		}
		return out
	}
	before := rowsOnly(u2Dump(t, db))
	if _, err := db.ChatOperation("move", s.Key, d.Key, false); err == nil {
		t.Fatal("failure control did not reach deletion")
	}
	if !reflect.DeepEqual(before, rowsOnly(u2Dump(t, db))) {
		t.Fatal("failed move partially committed copy or deletion")
	}
}

// The same reply after a different user input is not the same turn: both
// the input and the reply must match.
func TestU2CopyNeedsTheSameInputToo(t *testing.T) {
	db := testDB(t)
	mk := func(key, input, reply string) *lifecycle.Session {
		s := lifecycle.NewSession(key)
		s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow(key+"u", "user", "", input), lrow(key+"a", "char", "", reply)))
		s.Progress["u2"] = 10
		if err := db.SaveSession(s, s.TakeEvents(), nil); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a := mk("chat:IA", "질문 하나", "같은 답")
	mk("chat:IB", "질문 둘", "같은 답")
	mk("chat:IC", "질문 하나", "같은 답")
	u2Save(t, db, a, 0, `{"memories":[{"id":"m1","kind":"fact","body":"원본"}]}`)
	if r, err := db.ChatOperation("copy", a.Key, "chat:IB", true); err != nil || r.CopiedTurns != 0 || r.UnmatchedTurns != 1 {
		t.Fatalf("different input copied: %+v %v", r, err)
	}
	if r, err := db.ChatOperation("copy", a.Key, "chat:IC", true); err != nil || r.CopiedTurns != 1 || r.UnmatchedTurns != 0 {
		t.Fatalf("same turn not copied: %+v %v", r, err)
	}
}
