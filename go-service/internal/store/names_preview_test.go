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

	"archivecenter2/goservice/internal/memory"
	"github.com/go-sql-driver/mysql"
)

func c17Count(t *testing.T, p NameMergePreview, key string) int64 {
	t.Helper()
	v, ok := p.Counts[key]
	if !ok || v.Count == nil || v.Status != "available" {
		t.Fatalf("%s unavailable: %+v", key, v)
	}
	return *v.Count
}

func c17Preview(t *testing.T, d *DB, chat, keep, merge string) NameMergePreview {
	t.Helper()
	p, err := d.PreviewMergeNames(chat, keep, merge)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestC17CountsAndPrivacy(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:C17", Turn: 1, Responses: []memory.SourceResponse{{Seq: 1, Data: "synthetic"}}}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"keeper","type":"person"},{"id":"n2","name":"protected surface","type":"thing","hide_from":["audience"]},{"id":"n3","name":"protected surface","type":"person"},{"id":"n4","name":"private disguise","same_as":"n2","how":"disguise","unaware":["audience"]}],"memories":[{"id":"m1","kind":"event","body":"original prose","roles":{"actor":["n2"],"object":["n2"]},"effects":[{"about":"n2","slot":"x","value":"y"},{"about":"n1","slot":"z","value":"q"}]},{"id":"m2","kind":"fact","about":"n2","body":"fact","knows":[{"who":"n2","from":"n2","ref":"n2"}]},{"id":"m3","kind":"view","owner":"n2","body":"view"},{"id":"m4","kind":"fact","body":"unchanged"},{"ref":"n2","q":"mention"}],"links":[{"from":"n2","rel":"recalls","to":"m1"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	keep, merge := ids["entities/0"], ids["entities/1"]
	child := src
	child.Session = "chat:CHILD"
	jc := u3Stored(t, db, child, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","about":%q,"body":"child ref"},{"id":"m2","kind":"fact","body":"companion doc"}]}`, merge))
	unrelated := src
	unrelated.Session = "chat:UNRELATED"
	u3Stored(t, db, unrelated, `{"entities":[{"id":"n1","name":"protected surface","type":"thing"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"unrelated"}]}`)
	// Quoted-ID prose matches rewriteName's old LIKE probe but is not a typed ref.
	u3Stored(t, db, memory.Source{Session: src.Session, Turn: 2}, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","body":%q,"q":%q}]}`, merge, merge))
	// A typed provenance ref changes a memory, but not its DocText.
	u3Stored(t, db, memory.Source{Session: src.Session, Turn: 3}, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"view","body":"basis only","basis":{"ref":%q}}]}`, merge))
	p := c17Preview(t, db, src.Session, keep, merge)
	for key, want := range map[string]int64{"memories": 5, "memories_event": 1, "memories_fact": 2, "memories_view": 2, "effects": 1, "links": 1, "relations": 1, "user_aliases": 0, "mentions": 1, "awareness": 2, "sessions": 2, "jobs": 3, "other_sessions": 1, "other_jobs": 1, "same_name_others": 1, "protected_names": 1, "documents_changed": 4, "flat_documents": 4, "embedding_jobs": 2, "contextual_documents": 6} {
		if got := c17Count(t, p, key); got != want {
			t.Errorf("%s=%d want %d", key, got, want)
		}
	}
	for _, warning := range []string{"same_name_other_targets", "different_types", "protected_name_change"} {
		if !strings.Contains(strings.Join(p.Warnings, ","), warning) {
			t.Errorf("warning %s: %+v", warning, p)
		}
	}
	b, _ := json.Marshal(p)
	for _, private := range []string{"protected surface", "private disguise", "original prose", "CHILD", "UNRELATED", "disguise"} {
		if strings.Contains(string(b), private) {
			t.Fatalf("preview leaked %s: %s", private, b)
		}
	}
	// Compare actual document production before/after the existing writer. The
	// preview must not mutate either detail, mirrors, vector hashes or jobs.
	before, _ := db.ItemsOf([]int64{j.ID, jc.ID})
	beforeJSON, _ := json.Marshal(before)
	again := c17Preview(t, db, src.Session, keep, merge)
	afterRead, _ := db.ItemsOf([]int64{j.ID, jc.ID})
	afterJSON, _ := json.Marshal(afterRead)
	if !reflect.DeepEqual(p, again) || string(beforeJSON) != string(afterJSON) {
		t.Fatal("preview changed state")
	}
	if _, err := db.MergeNames(src.Session, keep, merge); err != nil {
		t.Fatal(err)
	}
	after, _ := db.ItemsOf([]int64{j.ID, jc.ID})
	names := map[string]string{}
	for _, n := range before.Names {
		names[n.ID] = n.Name
	}
	var changed int64
	for i, m := range before.Memories {
		if memory.DocText(memory.KnownMemory{Memory: m.Memory}, names) != memory.DocText(memory.KnownMemory{Memory: after.Memories[i].Memory}, names) {
			changed++
		}
	}
	if changed != c17Count(t, p, "documents_changed") {
		t.Fatalf("DocText actual=%d preview=%d", changed, c17Count(t, p, "documents_changed"))
	}
}

func TestC17TypedSlots(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:TYPED", Turn: 1}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"keep"},{"id":"n2","name":"merge"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	keep, merge := ids["entities/0"], ids["entities/1"]
	// Independently enumerate every resolver slot; strings in prose do not count.
	fields := []string{`"ref":"%s"`, `"at":"%s"`, `"scene":"%s"`, `"about":"%s"`, `"owner":"%s"`, `"to":["%s"]`, `"was":["%s"]`, `"seen_by":["%s"]`, `"basis":{"from":"%s"}`, `"basis":{"ref":"%s"}`, `"unaware":["%s"]`, `"hide_from":["%s"]`, `"allow_to":["%s"]`, `"knows":[{"who":"%s"}]`, `"knows":[{"from":"%s"}]`, `"knows":[{"ref":"%s"}]`, `"effects":[{"about":"%s"}]`, `"effects":[{"was":["%s"]}]`}
	for _, role := range []string{"actor", "target", "object", "place", "speaker", "listener"} {
		fields = append(fields, `"roles":{"`+role+`":["%s"]}`)
	}
	for i, f := range fields {
		raw := `{"id":"Mfixture","kind":"event","body":"body",` + fmt.Sprintf(f, merge) + `}`
		if _, err := db.sql.Exec(`INSERT INTO memories(session_key,job_id,kind,body,q,detail) VALUES(?,?,'event','body','',?)`, src.Session, j.ID, raw); err != nil {
			t.Fatal(err)
		}
		p := c17Preview(t, db, src.Session, keep, merge)
		if got := c17Count(t, p, "memories"); got != int64(i+1) {
			t.Fatalf("typed slot %s omitted: %d", f, got)
		}
	}
	for _, f := range []string{`"body":"%s"`, `"q":"%s"`, `"slot":"%s"`, `"terms":"%s"`, `"status":"%s"`, `"basis":{"via":"%s"}`, `"effects":[{"value":"%s"}]`} {
		raw := `{"kind":"fact",` + fmt.Sprintf(f, merge) + `}`
		if _, err := db.sql.Exec(`INSERT INTO memories(session_key,job_id,kind,body,q,detail) VALUES(?,?,'fact','','',?)`, src.Session, j.ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	p := c17Preview(t, db, src.Session, keep, merge)
	if got := c17Count(t, p, "memories"); got != int64(len(fields)) {
		t.Fatalf("prose false positives: %d", got)
	}
	if c17Count(t, p, "effects") != 2 {
		t.Fatal("effect refs omitted or double counted")
	}
}

func TestC17RenameChainAndEqualDocuments(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:CHAIN", Turn: 1}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"A","type":"person"},{"id":"n2","name":"B","type":"person"},{"id":"n3","name":"C","type":"person"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"first"},{"id":"m2","kind":"view","owner":"n2","body":"second"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	a, b, c := ids["entities/0"], ids["entities/1"], ids["entities/2"]
	if _, err := db.EditName(src.Session, a, "renamed"); err != nil {
		t.Fatal(err)
	}
	p := c17Preview(t, db, src.Session, b, a)
	if c17Count(t, p, "memories") != 1 || c17Count(t, p, "user_aliases") != 1 {
		t.Fatal(p)
	}
	if _, err := db.MergeNames(src.Session, b, a); err != nil {
		t.Fatal(err)
	}
	p = c17Preview(t, db, src.Session, c, b)
	if c17Count(t, p, "memories") != 2 || c17Count(t, p, "user_aliases") != 2 || c17Count(t, p, "documents_changed") != 2 {
		t.Fatal(p)
	}
	if _, err := db.EditName(src.Session, c, "B"); err != nil {
		t.Fatal(err)
	}
	p = c17Preview(t, db, src.Session, c, b)
	if c17Count(t, p, "memories") != 2 || c17Count(t, p, "documents_changed") != 0 || c17Count(t, p, "contextual_documents") != 0 {
		t.Fatal("same label is not a new document", p)
	}
	p = c17Preview(t, db, src.Session, b, b)
	for key, v := range p.Counts {
		if v.Count == nil || *v.Count != 0 {
			t.Fatalf("self merge %s: %+v", key, v)
		}
	}
}

// The real SQL driver remains in use. This boundary records and refuses any
// write, session-wide hydration or explicit test-injected read failure.
type c17Meter struct {
	writes, forbidden, bytes, maxPage int
	fail                              string
	queries                           []string
}
type c17Connector struct {
	driver.Connector
	meter *c17Meter
}
type c17Conn struct {
	driver.Conn
	meter *c17Meter
}

type c17Rows struct {
	driver.Rows
	meter *c17Meter
	page  bool
	read  int
}

func (r *c17Rows) Next(dest []driver.Value) error {
	if err := r.Rows.Next(dest); err != nil {
		return err
	}
	r.read++
	if r.page && r.read > r.meter.maxPage {
		r.meter.maxPage = r.read
	}
	for _, v := range dest {
		switch x := v.(type) {
		case []byte:
			r.meter.bytes += len(x)
		case string:
			r.meter.bytes += len(x)
		}
	}
	return nil
}

func (c c17Connector) Connect(ctx context.Context) (driver.Conn, error) {
	v, e := c.Connector.Connect(ctx)
	if e != nil {
		return nil, e
	}
	return c17Conn{v, c.meter}, nil
}

func (c c17Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c c17Conn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.meter.writes++
	return nil, errors.New("unexpected preview write")
}
func (c c17Conn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	c.meter.queries = append(c.meter.queries, q)
	if !strings.HasPrefix(strings.TrimSpace(q), "SELECT") {
		c.meter.writes++
		return nil, errors.New("unexpected non-read")
	}
	if c.meter.fail != "" && strings.Contains(q, c.meter.fail) {
		return nil, errors.New("synthetic query failure")
	}
	if strings.Contains(q, "session_progress") || strings.Contains(q, "SELECT turn_id, greeting, confirmed") || strings.Contains(q, "FROM turn_members") || strings.Contains(q, "FROM revisions") {
		c.meter.forbidden++
		return nil, errors.New("unexpected full session read")
	}
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
	if err != nil {
		return nil, err
	}
	return &c17Rows{Rows: r, meter: c.meter, page: strings.Contains(q, "SELECT id,job_id,detail")}, nil
}
func c17Measured(t *testing.T, db *DB, meter *c17Meter) *DB {
	t.Helper()
	cfg, e := mysql.ParseDSN(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_" + t.Name() + "?parseTime=true")
	if e != nil {
		t.Fatal(e)
	}
	cfg.InterpolateParams = true
	c, e := mysql.NewConnector(cfg)
	if e != nil {
		t.Fatal(e)
	}
	d := &DB{sql: sql.OpenDB(c17Connector{c, meter})}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestC17ReadOnlyPartialAndCursor(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:READ", Turn: 1}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"keep"},{"id":"n2","name":"merge"}],"memories":[{"id":"m1","kind":"fact","about":"n2","body":"one"}],"links":[{"from":"n2","to":"m1","rel":"recalls"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	keep, merge := ids["entities/0"], ids["entities/1"]
	meter := &c17Meter{}
	read := c17Measured(t, db, meter)
	p := c17Preview(t, read, src.Session, keep, merge)
	if c17Count(t, p, "memories") != 1 || c17Count(t, p, "links") != 1 || meter.writes != 0 {
		t.Fatal(p, meter.writes)
	}
	baseBytes := meter.bytes
	// Unrelated bodies in the same chat must not cross the driver boundary.
	other := src
	other.Turn = 2
	u3Stored(t, db, other, `{"memories":[{"id":"m1","kind":"fact","body":"`+strings.Repeat("UNREQUESTED", 2048)+`"}]}`)
	meter.bytes = 0
	p = c17Preview(t, read, src.Session, keep, merge)
	if meter.bytes != baseBytes {
		t.Fatalf("unrelated body read: %d -> %d", baseBytes, meter.bytes)
	}
	t.Logf("unrelated 20KiB body: driver bytes %d -> %d", baseBytes, meter.bytes)
	meter.fail = "SELECT COUNT(*) FROM links"
	p = c17Preview(t, read, src.Session, keep, merge)
	if p.Counts["links"].Status != "unavailable" || p.Counts["links"].Count != nil || c17Count(t, p, "memories") != 1 || c17Count(t, p, "documents_changed") != 1 {
		t.Fatal("partial read destroyed other counts", p)
	}
	meter.fail = "SELECT id,name FROM names"
	p = c17Preview(t, read, src.Session, keep, merge)
	if p.Counts["documents_changed"].Status != "unavailable" || c17Count(t, p, "memories") != 1 || c17Count(t, p, "links") != 1 {
		t.Fatal("document failure destroyed independent counts", p)
	}
	meter.fail = ""
	for i := 0; i < 205; i++ {
		if _, e := db.sql.Exec(`INSERT INTO memories(session_key,job_id,kind,body,q,detail) SELECT session_key,job_id,kind,body,q,detail FROM memories WHERE id=?`, strings.TrimPrefix(ids["memories/0"], "M")); e != nil {
			t.Fatal(e)
		}
	}
	p = c17Preview(t, read, src.Session, keep, merge)
	if c17Count(t, p, "memories") != 206 || c17Count(t, p, "documents_changed") != 206 || c17Count(t, p, "contextual_documents") != 206 || meter.writes != 0 || meter.forbidden != 0 || meter.maxPage != 100 {
		t.Fatal("cursor duplicate/omission or write", p, meter.writes)
	}
	t.Logf("206 matching bodies: max driver page %d, write attempts %d, full-session attempts %d", meter.maxPage, meter.writes, meter.forbidden)
	for _, q := range meter.queries {
		if strings.Contains(q, "SELECT id,job_id,detail") && !strings.Contains(q, "ORDER BY id LIMIT 100") {
			t.Fatal("unbounded body read", q)
		}
	}
	if _, e := read.PreviewMergeNames("chat:OTHER", keep, merge); e != ErrNameNotFound {
		t.Fatal("out of scope name", e)
	}
	if _, e := read.PreviewMergeNames(src.Session, keep, "N999999999"); e != ErrNameNotFound {
		t.Fatal("missing name", e)
	}
}
