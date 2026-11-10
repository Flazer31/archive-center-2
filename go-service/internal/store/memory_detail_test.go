package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"github.com/go-sql-driver/mysql"
)

// Preserve the driver's read-only transaction support so a forbidden
// LoadSession mutation really executes instead of failing at BeginTx.
func (c traceMeterConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func detailFixtureTurn(id int, text string) *lifecycle.Turn {
	return &lifecycle.Turn{ID: id, Confirmed: true,
		Members: []*lifecycle.Member{{Key: fmt.Sprintf("u%d", id), Data: "입력 " + text, State: lifecycle.Active}, {Key: fmt.Sprintf("ub%d", id), Data: "추가 입력 " + text, State: lifecycle.Active}},
		Responses: []*lifecycle.ResponseRow{{Key: fmt.Sprintf("a%d", id), State: lifecycle.Active, Gen: "g1", Revisions: []lifecycle.Revision{{Seq: 1, Data: "응답 " + text, GenerationID: "g1"}}},
			{Key: fmt.Sprintf("ab%d", id), State: lifecycle.Active, Gen: "g2", Revisions: []lifecycle.Revision{{Seq: 2, Data: "이어서 " + text, GenerationID: "g2"}}}}}
}

func detailSaveSession(t *testing.T, db *DB, s *lifecycle.Session) {
	t.Helper()
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
}

func detailSave(t *testing.T, db *DB, src memory.Source, raw string) (Job, Items) {
	t.Helper()
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	o := parse(t, raw)
	if _, err = db.SaveGeneration(j, Generation{Request: "MODEL REQUEST IS NOT ORIGINAL", Response: "MODEL OUTPUT IS NOT ORIGINAL", Output: o}); err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	return j, it
}

func detailGet(t *testing.T, db *DB, chat string, turn int, id string) MemoryDetail {
	t.Helper()
	v, err := db.MemoryDetail(chat, turn, id, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMemoryDetailSourceRawAndDirectEvidence(t *testing.T) {
	db := testDB(t)
	tr := detailFixtureTurn(7, "여러 행 원문 😀")
	s := lifecycle.Restore("chat:DETAIL", []*lifecycle.Turn{tr}, nil)
	detailSaveSession(t, db, s)
	src := memory.SourceOf(s.Key, tr)
	j, it := detailSave(t, db, src, `{"entities":[{"id":"n1","name":"이름 원본"},{"id":"n2","name":"보호 대상"}],"memories":[{"id":"m1","kind":"event","body":"사건 기억","q":"MODEL QUOTE IS NOT ORIGINAL","roles":{"actor":["n1"]},"effects":[{"about":"n1","slot":"잔액","value":"2"}]},{"id":"m2","kind":"view","owner":"n1","body":"생각","at":"m1.1","basis":{"ref":"m1","via":"추측","from":"n2","confidence":"낮음"},"was":["m1.1"],"hide_from":["n2"]}],"links":[{"id":"l1","from":"m1","rel":"causes","to":"m2","q":"연결 인용"},{"id":"l2","from":"m2","rel":"after","to":"m1"}]}`)
	v := detailGet(t, db, s.Key, tr.ID, it.Memories[1].ID)
	if !reflect.DeepEqual(v.Source, src) || v.SourceKey != src.Key() {
		t.Fatalf("not the stored job source: %+v", v)
	}
	if v.TurnID != tr.ID || v.Turn != 1 || v.SourceTurn != src.Turn || v.Job != j.ID || v.Inherited {
		t.Fatalf("locator: %+v", v.DetailLocator)
	}
	if !reflect.DeepEqual(v.Memory, it.Memories[1].Memory) || v.Memory.Owner != it.Names[0].ID || v.Labels[v.Memory.Owner] != it.Names[0].Name || !v.Secret {
		t.Fatalf("raw IDs/labels/protection: %+v", v)
	}
	var link, aware, effect, event int
	for _, r := range v.Related {
		if r.Status != "available" || r.Locator == nil || r.Locator.TurnID != tr.ID {
			t.Fatalf("related: %+v", r)
		}
		if r.Link != nil {
			link++
			if len(r.Endpoints) != 2 {
				t.Fatal("link endpoints have no read locators")
			}
			for _, ep := range r.Endpoints {
				if ep.Status != "available" || ep.Locator == nil || ep.Locator.TurnID != tr.ID {
					t.Fatalf("endpoint: %+v", ep)
				}
			}
		}
		if r.Awareness != nil {
			aware++
			if r.Awareness.Job != j.ID || r.Awareness.Target != v.ID {
				t.Fatal("awareness provenance")
			}
		}
		if r.Effect != nil {
			effect++
			if r.Effect.Value != "2" {
				t.Fatal("wrong effect")
			}
		}
		if r.Memory != nil {
			event++
			if r.Memory.Body != it.Memories[0].Body {
				t.Fatal("wrong event")
			}
		}
	}
	if link != len(it.Links) || aware != 1 || effect != 2 || event != 1 {
		t.Fatalf("direct evidence links=%d aware=%d effect=%d event=%d", link, aware, effect, event)
	}
	listed, err := db.ChatMemories(s.Key, memory.Own(s))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range listed {
		if m.TurnID != tr.ID || m.Turn != v.Turn || m.SourceTurn != src.Turn {
			t.Fatalf("list locator: %+v", m)
		}
	}
	// Pages must be precisely the same ordered rows as the whole direct
	// page, including references to the same node through different fields.
	for _, size := range []int{1, 2, 3} {
		var got []DetailRelated
		cursor := ""
		seen := map[string]bool{}
		for {
			p, err := db.MemoryDetail(s.Key, tr.ID, v.ID, cursor, size)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Related) > size {
				t.Fatal("page exceeded limit")
			}
			got = append(got, p.Related...)
			if p.NextCursor == nil {
				break
			}
			cursor = *p.NextCursor
			if seen[cursor] {
				t.Fatal("cursor loop")
			}
			seen[cursor] = true
		}
		if !reflect.DeepEqual(got, v.Related) {
			t.Fatalf("cursor duplicated/omitted rows, size=%d: got %+v want %+v", size, got, v.Related)
		}
	}
	if _, err := db.MemoryDetail(s.Key, tr.ID, v.ID, "links:-1", 1); !errors.Is(err, ErrDetailCursor) {
		t.Fatalf("cursor: %v", err)
	}
}

func TestMemoryDetailBranchFrozenSourceAndChildFallback(t *testing.T) {
	db := testDB(t)
	pturn := detailFixtureTurn(4, "분기 전")
	parent := lifecycle.Restore("chat:PARENT", []*lifecycle.Turn{pturn}, nil)
	detailSaveSession(t, db, parent)
	psrc := memory.SourceOf(parent.Key, pturn)
	old, it := detailSave(t, db, psrc, `{"memories":[{"id":"m1","kind":"event","body":"이전 기억"}]}`)
	cturn := detailFixtureTurn(9, "분기 전")
	child := lifecycle.Restore("chat:CHILD", []*lifecycle.Turn{cturn}, nil)
	child.BranchOf = [2]string{parent.Key, "ab4"}
	b, _ := json.Marshal(psrc)
	child.RecordCopies(child.BranchOf, map[int]lifecycle.Copy{cturn.ID: {Own: memory.WholeSourceOf(child.Key, cturn).Key(), Source: string(b)}})
	detailSaveSession(t, db, child)
	// The parent changes after the copy. Its latest source must never be
	// substituted for gen_jobs.source or the recorded branch source.
	pturn.Responses[0].Gen = "reroll"
	pturn.Responses[0].Revisions = append(pturn.Responses[0].Revisions, lifecycle.Revision{Seq: 3, GenerationID: "reroll", Data: "PARENT LATEST MUST NOT LEAK"})
	later := detailFixtureTurn(12, "분기 뒤 MUST NOT LEAK")
	parent.Turns = append(parent.Turns, later)
	detailSaveSession(t, db, parent)
	_, now := detailSave(t, db, memory.SourceOf(parent.Key, pturn), `{"memories":[{"id":"m1","kind":"event","body":"부모 최신"}]}`)
	_, post := detailSave(t, db, memory.SourceOf(parent.Key, later), `{"memories":[{"id":"m1","kind":"event","body":"분기 뒤"}]}`)
	v := detailGet(t, db, child.Key, cturn.ID, it.Memories[0].ID)
	if !reflect.DeepEqual(v.Source, psrc) || v.SourceTurn != pturn.ID || v.TurnID != cturn.ID || !v.Inherited {
		t.Fatalf("branch source: %+v", v)
	}
	for _, id := range []string{now.Memories[0].ID, post.Memories[0].ID} {
		if _, err := db.MemoryDetail(child.Key, cturn.ID, id, "", 5); !errors.Is(err, ErrMemoryNotFound) {
			t.Fatalf("branch leak: %s %v", id, err)
		}
	}
	m := it.Memories[0].Memory
	m.At = post.Memories[0].ID
	m.Basis = &memory.Basis{Ref: now.Memories[0].ID}
	b, _ = json.Marshal(m)
	if _, err := db.sql.Exec(`UPDATE memories SET detail=? WHERE id=?`, string(b), strings.TrimPrefix(it.Memories[0].ID, "M")); err != nil {
		t.Fatal(err)
	}
	v = detailGet(t, db, child.Key, cturn.ID, it.Memories[0].ID)
	if len(v.Related) != 2 {
		t.Fatal("branch related refs absent")
	}
	for _, r := range v.Related {
		if r.Status != "out_of_scope" || r.Locator != nil || r.Memory != nil {
			t.Fatalf("post-branch related body leaked: %+v", r)
		}
	}
	// A requested parent job has no memory in scope; the already completed
	// child-owned job is the existing turnJobs fallback, not parent latest.
	if _, err := db.sql.Exec(`UPDATE gen_jobs SET state='requested' WHERE job_id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	_, own := detailSave(t, db, memory.SourceOf(child.Key, cturn), `{"memories":[{"id":"m1","kind":"fact","body":"자식이 만든 기억"}]}`)
	v = detailGet(t, db, child.Key, cturn.ID, own.Memories[0].ID)
	if v.Inherited || v.SourceSession != child.Key || v.SourceTurn != cturn.ID {
		t.Fatalf("child fallback: %+v", v)
	}
	if _, err := db.MemoryDetail(child.Key, cturn.ID, it.Memories[0].ID, "", 5); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatal("incomplete parent remained visible")
	}
}

func TestMemoryDetailDisabledRepliesAndTurn(t *testing.T) {
	db := testDB(t)
	tr := detailFixtureTurn(3, "켜짐")
	s := lifecycle.Restore("chat:OFF", []*lifecycle.Turn{tr}, nil)
	detailSaveSession(t, db, s)
	_, before := detailSave(t, db, memory.SourceOf(s.Key, tr), `{"memories":[{"id":"m1","kind":"event","body":"모든 응답 기억"}]}`)
	tr.Responses[1].Off = true
	detailSaveSession(t, db, s)
	src := memory.SourceOf(s.Key, tr)
	_, active := detailSave(t, db, src, `{"memories":[{"id":"m1","kind":"event","body":"켜진 응답 기억"}]}`)
	v := detailGet(t, db, s.Key, tr.ID, active.Memories[0].ID)
	if !reflect.DeepEqual(v.Source, src) || len(v.Source.Responses) != 1 {
		t.Fatalf("disabled reply mixed: %+v", v.Source)
	}
	if _, err := db.MemoryDetail(s.Key, tr.ID, before.Memories[0].ID, "", 1); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatal("old response source remained current")
	}
	tr.Off = true
	detailSaveSession(t, db, s)
	if _, err := db.MemoryDetail(s.Key, tr.ID, v.ID, "", 1); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatal("off turn read")
	}
}

func TestMemoryDetailMissingDeletedAndScope(t *testing.T) {
	db := testDB(t)
	tr := detailFixtureTurn(2, "현재")
	s := lifecycle.Restore("chat:SCOPE", []*lifecycle.Turn{tr}, nil)
	detailSaveSession(t, db, s)
	otherTurn := detailFixtureTurn(8, "OTHER CHAT PRIVATE")
	other := lifecycle.Restore("chat:OTHER", []*lifecycle.Turn{otherTurn}, nil)
	detailSaveSession(t, db, other)
	oj, oi := detailSave(t, db, memory.SourceOf(other.Key, otherTurn), `{"memories":[{"id":"m1","kind":"event","body":"OTHER CHAT PRIVATE"}]}`)
	current, deleted := detailSave(t, db, memory.SourceOf(s.Key, tr), `{"memories":[{"id":"m1","kind":"event","body":"없어질 기억"},{"id":"m2","kind":"view","body":"현재 생각"}]}`)
	id, dead := deleted.Memories[1].ID, deleted.Memories[0].ID
	if err := db.DeleteMemories(s.Key, []string{dead}); err != nil {
		t.Fatal(err)
	}
	// A manually corrected stored reference can be broken or point outside
	// scope. Keep it as written; do not guess a substitute by body text.
	m := deleted.Memories[1].Memory
	m.At = "M999999999.1"
	m.Basis = &memory.Basis{Ref: oi.Memories[0].ID, Via: "추측", From: "전언자", Confidence: "모름"}
	m.Was = memory.Refs{dead, oi.Memories[0].ID + ".1", id + ".999"}
	b, _ := json.Marshal(m)
	if _, err := db.sql.Exec(`UPDATE memories SET detail=? WHERE id=?`, string(b), strings.TrimPrefix(id, "M")); err != nil {
		t.Fatal(err)
	}
	// Out-of-scope incoming links/awareness must not leak prose, nor make
	// this memory secret. Their references alone do not grant read scope.
	if _, err := db.sql.Exec(`INSERT INTO links(session_key,job_id,from_ref,rel,to_ref,q) VALUES(?,?,?,?,?,?)`, other.Key, oj.ID, id, "causes", oi.Memories[0].ID, "OTHER CHAT PRIVATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO awareness(session_key,job_id,target,kind,who,part,q) VALUES(?,?,?,?,?,'',?)`, other.Key, oj.ID, id, "hide_from", "PRIVATE NAME", "OTHER CHAT PRIVATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO links(session_key,job_id,from_ref,rel,to_ref,q) VALUES(?,?,?,?,?,?)`, s.Key, current.ID, id, "causes", oi.Memories[0].ID, "현재 출처의 연결"); err != nil {
		t.Fatal(err)
	}
	v := detailGet(t, db, s.Key, tr.ID, id)
	want := map[string]string{"at": "missing", "basis.ref": "out_of_scope", "was.0": "deleted", "was.1": "out_of_scope", "was.2": "missing"}
	if len(v.Related) != len(want)+1 || v.Secret {
		t.Fatalf("cross-scope relations/protection: %+v", v)
	}
	for _, r := range v.Related {
		if r.Link != nil {
			if len(r.Endpoints) != 2 || r.Endpoints[0].Status != "available" || r.Endpoints[1].Status != "out_of_scope" || r.Endpoints[1].Locator != nil {
				t.Fatalf("link endpoint scope leaked: %+v", r)
			}
			continue
		}
		if r.Status != want[r.Field] || r.Memory != nil || r.Effect != nil || r.Locator != nil {
			t.Fatalf("bad unavailable ref: %+v", r)
		}
	}
	wire, _ := json.Marshal(v)
	if strings.Contains(string(wire), "OTHER CHAT PRIVATE") || strings.Contains(string(wire), "PRIVATE NAME") {
		t.Fatal("scope leaked")
	}
	if _, err := db.MemoryDetail(other.Key, otherTurn.ID, id, "", 1); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatal("selected memory cross-chat read")
	}
}

func TestMemoryDetailColdAndBoundedReads(t *testing.T) {
	db := testDB(t)
	cfg, err := mysql.ParseDSN(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_" + t.Name() + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	cfg.InterpolateParams = true
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	meter := &traceReadMeter{}
	cold := &DB{sql: sql.OpenDB(traceMeterConnector{connector, meter})}
	defer cold.Close()
	selected := detailFixtureTurn(1, "선택 원문")
	s := lifecycle.Restore("chat:LONG", []*lifecycle.Turn{selected}, nil)
	detailSaveSession(t, db, s)
	_, it := detailSave(t, db, memory.SourceOf(s.Key, selected), `{"memories":[{"id":"m1","kind":"event","body":"선택 기억"}]}`)
	var bytes []int
	for _, turns := range []int{1, 350} {
		for len(s.Turns) < turns {
			tr := detailFixtureTurn(len(s.Turns)+1, strings.Repeat("UNREQUESTED", 512))
			s.Turns = append(s.Turns, tr)
			detailSave(t, db, memory.SourceOf(s.Key, tr), `{"memories":[{"id":"m1","kind":"event","body":"`+strings.Repeat("UNREQUESTED BODY ", 512)+`"}]}`)
		}
		detailSaveSession(t, db, s)
		*meter = traceReadMeter{}
		v := detailGet(t, cold, s.Key, selected.ID, it.Memories[0].ID)
		if !reflect.DeepEqual(v.Source, memory.SourceOf(s.Key, selected)) {
			t.Fatal("cold source changed")
		}
		for _, q := range meter.queries {
			if strings.Contains(q, "session_progress") || strings.Contains(q, "SELECT id, job_id, detail FROM memories WHERE job_id IN") || strings.Contains(q, "SELECT turn_id, greeting, confirmed, inactive, off, dates FROM logical_turns") {
				t.Fatalf("whole reader used: %s", q)
			}
		}
		bytes = append(bytes, meter.bytes)
		t.Logf("chat_turns=%d driver_rows=%d driver_text_bytes=%d queries=%d", turns, meter.rows, meter.bytes, len(meter.queries))
		if meter.bytes > 8192 {
			t.Fatalf("unrequested bodies read: %d", meter.bytes)
		}
	}
	if bytes[1] != bytes[0] {
		t.Fatalf("read grew with chat length: %v", bytes)
	}
	// Reopening actual store (including idempotent index migration) has the
	// same cold result; no Pipeline or persisted session cache is involved.
	reopened, err := Open(cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	before := detailGet(t, cold, s.Key, selected.ID, it.Memories[0].ID)
	after := detailGet(t, reopened, s.Key, selected.ID, it.Memories[0].ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("cold reopen changed detail")
	}
	for _, q := range []string{
		`EXPLAIN SELECT id,job_id FROM links WHERE id>0 AND (from_ref='M1' OR to_ref='M1' OR from_ref LIKE 'M1.%' OR to_ref LIKE 'M1.%') ORDER BY id LIMIT 1`,
		`EXPLAIN SELECT id,job_id FROM awareness WHERE id>0 AND (target='M1' OR target LIKE 'M1.%') ORDER BY id LIMIT 1`,
	} {
		rows, err := db.sql.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			v := make([]sql.RawBytes, len(cols))
			args := make([]any, len(cols))
			for i := range v {
				args[i] = &v[i]
			}
			if err = rows.Scan(args...); err != nil {
				t.Fatal(err)
			}
			t.Logf("plan %s: %s", q, v)
		}
		rows.Close()
	}
}

func TestMemoryDetailRelatedPageBounded(t *testing.T) {
	db := testDB(t)
	tr := detailFixtureTurn(1, "단건 페이지")
	s := lifecycle.Restore("chat:PAGE", []*lifecycle.Turn{tr}, nil)
	detailSaveSession(t, db, s)
	j, it := detailSave(t, db, memory.SourceOf(s.Key, tr), `{"memories":[{"id":"m1","kind":"event","body":"선택 기억"}]}`)
	cfg, err := mysql.ParseDSN(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_" + t.Name() + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	cfg.InterpolateParams = true
	c, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	meter := &traceReadMeter{}
	narrow := &DB{sql: sql.OpenDB(traceMeterConnector{c, meter})}
	defer narrow.Close()
	var wire []int
	for _, total := range []int{100, 2000} {
		if _, err := db.sql.Exec(`DELETE FROM links`); err != nil {
			t.Fatal(err)
		}
		tx, err := db.sql.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < total; i++ {
			q := "먼저 세 행"
			if i > 2 {
				q = strings.Repeat("NOT REQUESTED ", 1024)
			}
			if _, err := tx.Exec(`INSERT INTO links(session_key,job_id,from_ref,rel,to_ref,q) VALUES(?,?,?,?,?,?)`, s.Key, j.ID, it.Memories[0].ID, "causes", it.Memories[0].ID, q); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		*meter = traceReadMeter{}
		v, err := narrow.MemoryDetail(s.Key, tr.ID, it.Memories[0].ID, "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Related) != 2 || v.NextCursor == nil {
			t.Fatalf("page: %+v", v)
		}
		for _, r := range v.Related {
			if r.Link == nil || r.Link.Q != "먼저 세 행" {
				t.Fatal("not the first page")
			}
		}
		if meter.bytes > 4096 {
			t.Fatalf("unrequested related prose crossed driver: %d", meter.bytes)
		}
		wire = append(wire, meter.bytes)
		t.Logf("direct_links=%d page_rows=%d driver_rows=%d driver_text_bytes=%d", total, len(v.Related), meter.rows, meter.bytes)
	}
	// Later globally allocated link IDs grow by a few digits only; they
	// are not a reason to read the thousands of unrequested link bodies.
	if wire[1] > wire[0]+32 {
		t.Fatalf("page read grew with all links: %v", wire)
	}
	rows, err := db.sql.Query(`EXPLAIN SELECT id,job_id FROM links WHERE id>0 AND (from_ref=? OR to_ref=? OR from_ref LIKE ? OR to_ref LIKE ?) ORDER BY id LIMIT 1`, it.Memories[0].ID, it.Memories[0].ID, it.Memories[0].ID+".%", it.Memories[0].ID+".%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	for rows.Next() {
		v := make([]sql.RawBytes, len(cols))
		args := make([]any, len(cols))
		for i := range v {
			args[i] = &v[i]
		}
		if err := rows.Scan(args...); err != nil {
			t.Fatal(err)
		}
		t.Logf("actual link plan at 2000 rows: %s", v)
	}
}
