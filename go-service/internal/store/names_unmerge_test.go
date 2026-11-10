package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

func c17MergeID(t *testing.T, db *DB, chat, keep, merge string) int64 {
	t.Helper()
	if _, err := db.MergeNames(chat, keep, merge); err != nil {
		t.Fatal(err)
	}
	ops, err := db.NameMerges(chat)
	if err != nil || len(ops) == 0 {
		t.Fatal(ops, err)
	}
	return ops[len(ops)-1].ID
}
func TestC17UnmergeOriginChainEdit(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:unmerge", Turn: 1}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"A","type":"person"},{"id":"n2","name":"B","type":"thing"},{"id":"n3","name":"C"},{"id":"n4","name":"A","same_as":"n1","how":"disguise","unaware":["n2"]}],"memories":[{"id":"m1","kind":"event","body":"A original prose","q":"A original quote","roles":{"actor":["n1","n2"],"target":["n1"],"object":["n2"]},"effects":[{"about":"n1","slot":"balance","value":"2"}],"was":["n1"],"hide_from":["n1"]},{"id":"m2","kind":"fact","body":"original A","about":"n1","to":["n1","n2"]},{"id":"m3","kind":"view","body":"original B","owner":"n2","basis":{"from":"n1","ref":"m1"}},{"ref":"n1","q":"mention A"},{"ref":"n2","q":"mention B"}],"links":[{"id":"l1","from":"n1","to":"n2","rel":"recalls","hide_from":["n1"]}]}`)
	ids, _ := db.ItemIDs(j.ID)
	a, b, c := ids["entities/0"], ids["entities/1"], ids["entities/2"]
	event, fact, view := ids["memories/0"], ids["memories/1"], ids["memories/2"]
	// Same spelling in an unrelated chat remains a different identity.
	unrelated := u3Stored(t, db, memory.Source{Session: "chat:unrelated", Turn: 1}, `{"entities":[{"id":"n1","name":"A"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"unrelated"}]}`)
	unrelatedIDs, _ := db.ItemIDs(unrelated.ID)
	child := u3Stored(t, db, memory.Source{Session: "chat:child", Turn: 1}, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"view","owner":%q,"body":"deleted child"},{"id":"m2","kind":"view","owner":%q,"body":"surviving child"}]}`, a, a))
	childIDs, _ := db.ItemIDs(child.ID)
	if _, err := db.EditName(src.Session, b, "renamed B"); err != nil {
		t.Fatal(err)
	}
	ab := c17MergeID(t, db, src.Session, b, a)
	bc := c17MergeID(t, db, src.Session, c, b)
	m := c16Memory(t, db, event)
	if m.Roles.Actor[0] != c || m.Roles.Actor[1] != c || m.Effects[0].About != c {
		t.Fatal("merge did not materialize", m)
	}
	// New generation must retain the pre-alias source independently for every
	// typed owner, including non-memory rows and repeated list positions.
	src.Turn++
	late := u3Stored(t, db, src, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"event","body":"later","roles":{"actor":[%q,%q,%q]},"effects":[{"about":%q}],"unaware":[%q]},{"ref":%q,"q":"later mention"}],"links":[{"from":%q,"to":%q,"rel":"recalls","knows":[{"who":%q,"from":%q,"ref":%q}]}]}`, a, b, c, a, a, a, a, b, a, b, a))
	lateIDs, _ := db.ItemIDs(late.ID)
	// Body-only keeps old reference provenance. An explicit same-value edit of
	// an owner starts from C, while a different field can explicitly start A.
	c16Patch(t, db, src.Session, fact, `{"body":"edited body survives"}`)
	c16Patch(t, db, src.Session, view, fmt.Sprintf(`{"owner":%q,"basis":{"from":%q}}`, c, a))
	c16Patch(t, db, src.Session, event, fmt.Sprintf(`{"roles":{"target":[%q,%q]},"effects":{"1":{"value":"manual value"}}}`, b, a))
	// Deletion is final; unmerge must never recreate it or its mirrors.
	if err := db.DeleteMemory("chat:child", childIDs["memories/0"]); err != nil {
		t.Fatal(err)
	}
	// Reopen the actual database before undo, proving no in-memory snapshot owns it.
	fresh, err := Open(dbDSNForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err = fresh.UnmergeNames(src.Session, ab); err != nil {
		t.Fatal(err)
	}
	m = c16Memory(t, db, event)
	if m.Roles.Actor[0] != a || m.Roles.Actor[1] != c || m.Roles.Target[0] != c || m.Roles.Target[1] != a || m.Effects[0].About != a || m.Effects[0].Value != "manual value" || m.Body != "A original prose" || m.Q != "A original quote" {
		t.Fatal("middle undo lost origin/edit", m)
	}
	if f := c16Memory(t, db, fact); f.About != a || f.To[0] != a || f.To[1] != c || f.Body != "edited body survives" {
		t.Fatal("fact undo", f)
	}
	if v := c16Memory(t, db, view); v.Owner != c || v.Basis.From != a {
		t.Fatal("explicit edit ignored", v)
	}
	if v := c16Memory(t, db, lateIDs["memories/0"]); v.Roles.Actor[0] != a || v.Roles.Actor[1] != c || v.Roles.Actor[2] != c || v.Effects[0].About != a || v.Unaware[0] != a {
		t.Fatal("late origins lost", v)
	}
	for _, id := range []string{event, fact, view, lateIDs["memories/0"]} {
		c16Mirrors(t, db, id)
	}
	var missing int
	db.sql.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=?`, memoryRowNumber(childIDs["memories/0"])).Scan(&missing)
	if missing != 0 {
		t.Fatal("deleted memory resurrected")
	}
	if m := c16Memory(t, db, childIDs["memories/1"]); m.Owner != a {
		t.Fatal("surviving child reference not restored", m)
	}
	if v := c16Memory(t, db, unrelatedIDs["memories/0"]); v.About != unrelatedIDs["entities/0"] {
		t.Fatal("unrelated same name changed")
	}
	var target string
	db.sql.QueryRow(`SELECT from_ref FROM links WHERE job_id=?`, late.ID).Scan(&target)
	if target != a {
		t.Fatal("late link origin", target)
	}
	db.sql.QueryRow(`SELECT target FROM mentions WHERE job_id=?`, late.ID).Scan(&target)
	if target != a {
		t.Fatal("late mention origin", target)
	}
	var words int
	db.sql.QueryRow(`SELECT COUNT(*) FROM search_words WHERE job_id=? AND item=? AND field='mention'`, late.ID, a).Scan(&words)
	if words == 0 {
		t.Fatal("mention words not restored")
	}
	it, err := db.ItemsOf([]int64{j.ID, late.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Awareness) < 4 {
		t.Fatal("protection deleted")
	}
	var relation int
	db.sql.QueryRow(`SELECT COUNT(*) FROM name_relations WHERE how='disguise'`).Scan(&relation)
	if relation != 1 {
		t.Fatal("model disguise removed")
	}
	if _, err := db.UnmergeNames(src.Session, ab); err != nil {
		t.Fatal("idempotent undo", err)
	}
	if _, err := db.UnmergeNames(src.Session, bc); err != nil {
		t.Fatal(err)
	}
	if v := c16Memory(t, db, lateIDs["memories/0"]); v.Roles.Actor[0] != a || v.Roles.Actor[1] != b || v.Roles.Actor[2] != c {
		t.Fatal("second undo", v)
	}
	if v := c16Memory(t, db, view); v.Owner != c {
		t.Fatal("explicit canonical owner reversed", v)
	}
	// Rename's separately-created alias survives both merge undos.
	var alias int
	db.sql.QueryRow(`SELECT COUNT(*) FROM name_relations WHERE how='user_alias' AND same_as=?`, b).Scan(&alias)
	if alias != 1 {
		t.Fatal("rename alias lost", alias)
	}
	if _, err := db.UnmergeNames(src.Session, 999999999); err != ErrMergeNoRecord {
		t.Fatal("legacy/no-record undo", err)
	}
	// Journals contain references only; prose/quotes have no storage column.
	var leak int
	db.sql.QueryRow(`SELECT COUNT(*) FROM name_ref_changes WHERE origin_ref LIKE '%original%' OR before_ref LIKE '%original%' OR after_ref LIKE '%original%'`).Scan(&leak)
	if leak != 0 {
		t.Fatal("body snapshot in journal")
	}
}

func dbDSNForTest(t *testing.T) string {
	return os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_" + t.Name() + "?parseTime=true"
}

func TestC17TransactionRollback(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:rollback", Turn: 1}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"A"},{"id":"n2","name":"B"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"body"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	a, b := ids["entities/0"], ids["entities/1"]
	if _, err := db.sql.Exec(`CREATE TRIGGER fail_journal BEFORE INSERT ON name_ref_changes FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic journal failure'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MergeNames(src.Session, b, a); err == nil {
		t.Fatal("journal failure accepted")
	}
	if count(t, db, "name_merges") != 0 || count(t, db, "name_relations") != 0 || c16Memory(t, db, ids["memories/0"]).About != a {
		t.Fatal("partial merge committed")
	}
	db.sql.Exec(`DROP TRIGGER fail_journal`)
	op := c17MergeID(t, db, src.Session, b, a)
	db.sql.Exec(`CREATE TRIGGER fail_detail BEFORE UPDATE ON memories FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic write failure'`)
	if _, err := db.UnmergeNames(src.Session, op); err == nil {
		t.Fatal("failed undo accepted")
	}
	ops, _ := db.NameMerges(src.Session)
	if len(ops) != 1 || count(t, db, "name_relations") != 1 || c16Memory(t, db, ids["memories/0"]).About != b {
		t.Fatal("partial undo committed")
	}
	var patch MemoryPatch
	json.Unmarshal([]byte(`{"body":"new"}`), &patch)
	if _, err := db.EditMemoryPatch(src.Session, ids["memories/0"], nil, nil, patch); err == nil {
		t.Fatal("failed patch accepted")
	}
	db.sql.Exec(`DROP TRIGGER fail_detail`)
	if _, err := db.UnmergeNames(src.Session, op); err != nil {
		t.Fatal(err)
	}
	// Fail after detail/columns and reference provenance have been written,
	// while rebuilding a derived index. Every earlier write must roll back.
	journalCount := count(t, db, "name_ref_changes")
	if _, err := db.sql.Exec(`CREATE TRIGGER fail_index BEFORE INSERT ON search_refs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic index failure'`); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(fmt.Sprintf(`{"about":%q,"body":"changed before failure"}`, b)), &patch)
	if _, err := db.EditMemoryPatch(src.Session, ids["memories/0"], nil, nil, patch); err == nil {
		t.Fatal("mid-patch failure accepted")
	}
	db.sql.Exec(`DROP TRIGGER fail_index`)
	if m := c16Memory(t, db, ids["memories/0"]); m.About != a || m.Body != "body" || count(t, db, "name_ref_changes") != journalCount {
		t.Fatal("partial C16 transaction committed", m)
	}
	c16Mirrors(t, db, ids["memories/0"])
	if err := db.ResetData(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count(t, db, "name_merges") != 0 || count(t, db, "name_ref_changes") != 0 {
		t.Fatal("reset left journals")
	}
}

func TestC17LegacyMergeNoRecord(t *testing.T) {
	db := testDB(t)
	j := u3Stored(t, db, memory.Source{Session: "chat:legacy", Turn: 1}, `{"entities":[{"id":"n1","name":"A"},{"id":"n2","name":"B"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	db.sql.Exec(`INSERT INTO name_relations(session_key,job_id,name_ref,same_as,how,q) VALUES (?,?,?,?,'user_alias','')`, j.Session, j.ID, ids["entities/0"], ids["entities/1"])
	var relation int64
	db.sql.QueryRow(`SELECT id FROM name_relations LIMIT 1`).Scan(&relation)
	if _, err := db.UnmergeNames(j.Session, relation); err != ErrMergeNoRecord {
		t.Fatal(err)
	}
	if count(t, db, "name_relations") != 1 {
		t.Fatal("legacy edge deleted")
	}
}

func TestC17EqualNameKeepsDocuments(t *testing.T) {
	db := testDB(t)
	j := u3Stored(t, db, memory.Source{Session: "chat:equal", Turn: 1}, `{"entities":[{"id":"n1","name":"same"},{"id":"n2","name":"same"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"one"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	db.sql.Exec(`UPDATE memories SET doc_hash=? WHERE job_id=?`, strings.Repeat("b", 64), j.ID)
	op := c17MergeID(t, db, j.Session, ids["entities/1"], ids["entities/0"])
	if _, err := db.UnmergeNames(j.Session, op); err != nil {
		t.Fatal(err)
	}
	var hash string
	db.sql.QueryRow(`SELECT doc_hash FROM memories WHERE job_id=?`, j.ID).Scan(&hash)
	if hash == "" {
		t.Fatal("unchanged document unlinked")
	}
}

func TestC17CopyUndoAndDelete(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:copy-source", "same", "later")
	dest := u2Session(t, db, "chat:copy-dest", "same", "later")
	j, items := u2Save(t, db, s, 0, `{"entities":[{"id":"n1","name":"A"},{"id":"n2","name":"B"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"copy fact","hide_from":["n1"]},{"ref":"n1","q":"mention"}],"links":[{"from":"n1","to":"n2","rel":"recalls"}]}`)
	sourceOp := c17MergeID(t, db, s.Key, items.Names[1].ID, items.Names[0].ID)
	later, _ := u2Save(t, db, s, 1, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"view","owner":%q,"body":"later view"}]}`, items.Names[0].ID))
	if _, err := db.ChatOperation("copy", s.Key, dest.Key, false); err != nil {
		t.Fatal(err)
	}
	ops, err := db.NameMerges(dest.Key)
	if err != nil || len(ops) != 1 || ops[0].ID == sourceOp {
		t.Fatal("operation ID mapping", ops, err)
	}
	if ops[0].Keep == items.Names[1].ID || ops[0].Merge == items.Names[0].ID {
		t.Fatal("operation endpoint ID mapping", ops)
	}
	if _, err := db.UnmergeNames(dest.Key, ops[0].ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.CurrentJobs([]memory.Source{turnSource(dest, 0), turnSource(dest, 1)})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := db.ItemsOf(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if copied.Memories[0].About != ops[0].Merge || copied.Memories[1].Owner != ops[0].Merge || copied.Mentions[0].Target != ops[0].Merge || copied.Links[0].From != ops[0].Merge || copied.Awareness[0].Who != ops[0].Merge {
		t.Fatal("copied reference origins not mapped", copied)
	}
	source, err := db.ItemsOf([]int64{j.ID, later.ID})
	if err != nil || source.Memories[0].About != items.Names[1].ID || source.Memories[1].Owner != items.Names[1].ID {
		t.Fatal("destination undo changed source", source, err)
	}
	if _, err := db.ChatOperation("delete", s.Key, "", false); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, table := range []string{"name_merges", "name_ref_changes"} {
		db.sql.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE session_key=?`, s.Key).Scan(&n)
		if n != 0 {
			t.Fatal("delete left operation data", table, n)
		}
	}
}

func TestC17RemakeUndoKeepsCurrentJob(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:remake", Turn: 1}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"A"},{"id":"n2","name":"B"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"old"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	op := c17MergeID(t, db, src.Session, ids["entities/1"], ids["entities/0"])
	if err := db.RemakeGeneration(src); err != nil {
		t.Fatal(err)
	}
	remade := u3Stored(t, db, src, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","about":%q,"body":"new"}]}`, ids["entities/0"]))
	current, _ := db.ItemIDs(remade.ID)
	if _, err := db.UnmergeNames(src.Session, op); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.CurrentJobs([]memory.Source{src})
	if err != nil || len(jobs) != 1 || jobs[0] != remade.ID {
		t.Fatal("undo revived retired job", jobs, err)
	}
	if m := c16Memory(t, db, current["memories/0"]); m.About != ids["entities/0"] || m.Body != "new" {
		t.Fatal("remade reference origin", m)
	}
}

func TestC17CopyLaterAliasDependency(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:dependency-source", "unpaired", "same")
	dest := u2Session(t, db, "chat:dependency-dest", "different", "same")
	j, it := u2Save(t, db, s, 0, `{"entities":[{"id":"n1","name":"A"},{"id":"n2","name":"B"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"UNPAIRED MEMORY MUST NOT COPY"}]}`)
	a, b := it.Names[0].ID, it.Names[1].ID
	if _, err := db.EditName(s.Key, a, "renamed A"); err != nil {
		t.Fatal(err)
	}
	var alias string
	db.sql.QueryRow(`SELECT CONCAT('N',id) FROM names WHERE job_id=? AND name='A'`, j.ID).Scan(&alias)
	c17MergeID(t, db, s.Key, b, a)
	u2Save(t, db, s, 1, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"view","owner":%q,"body":"later alias"}]}`, alias))
	if _, err := db.ChatOperation("copy", s.Key, dest.Key, false); err != nil {
		t.Fatal(err)
	}
	ops, err := db.NameMerges(dest.Key)
	if err != nil || len(ops) != 1 {
		t.Fatal("dependency operation missing", ops, err)
	}
	jobs, err := db.CurrentJobs([]memory.Source{turnSource(dest, 0), turnSource(dest, 1)})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := db.ItemsOf(jobs)
	if err != nil || len(copied.Memories) != 1 || copied.Memories[0].Owner != ops[0].Keep {
		t.Fatal("dependency current refs", copied, err)
	}
	if _, err := db.UnmergeNames(dest.Key, ops[0].ID); err != nil {
		t.Fatal(err)
	}
	copied, err = db.ItemsOf(jobs)
	if err != nil || copied.Memories[0].Owner != ops[0].Merge {
		t.Fatal("dependency original refs", copied, err)
	}
	for _, m := range copied.Memories {
		if strings.Contains(m.Body, "UNPAIRED") {
			t.Fatal("unpaired memory copied")
		}
	}
}
