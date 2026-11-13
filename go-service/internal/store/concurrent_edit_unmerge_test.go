package store

// An explicit C16 edit and an unmerge in flight together; the unmerge
// must not fail with 1020, and the edit must hold.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/memory"
	"github.com/go-sql-driver/mysql"
)

type pauseConnector struct {
	driver.Connector
	hook func(string)
}
type pauseConn struct {
	driver.Conn
	hook func(string)
}

func (c pauseConnector) Connect(ctx context.Context) (driver.Conn, error) {
	d, e := c.Connector.Connect(ctx)
	if e != nil {
		return nil, e
	}
	return pauseConn{d, c.hook}, nil
}
func (c pauseConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	c.hook(q)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
}
func (c pauseConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}
func (c pauseConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}

// Both writers are unmodified production APIs. The connector pauses one SQL
// boundary; the real MariaDB driver executes every statement and lock.
func TestConcurrentExplicitEditAndUnmergeBothSucceed(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:review", Turn: 1}
	j := u3Stored(t, db, src, `{"entities":[{"id":"n1","name":"Alpha"},{"id":"n2","name":"Beta"}],"memories":[{"id":"m1","kind":"fact","body":"synthetic body","about":"n1"}]}`)
	ids, e := db.ItemIDs(j.ID)
	if e != nil {
		t.Fatal(e)
	}
	a, b, id := ids["entities/0"], ids["entities/1"], ids["memories/0"]
	op := c17MergeID(t, db, src.Session, b, a)
	// connections as the backend makes them (Open: snapshot isolation)
	cfg, e := connConfig(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_" + t.Name() + "?parseTime=true")
	if e != nil {
		t.Fatal(e)
	}
	cfg.InterpolateParams = true
	conn, e := mysql.NewConnector(cfg)
	if e != nil {
		t.Fatal(e)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	edited := &DB{sql: sql.OpenDB(pauseConnector{conn, func(q string) {
		if strings.HasPrefix(q, "SELECT r.name_ref,COALESCE") {
			once.Do(func() { close(entered); <-release })
		}
	}})}
	defer edited.Close()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	editDone := make(chan error, 1)
	go func() {
		_, err := edited.EditMemoryPatch(src.Session, id, nil, nil, MemoryPatch{"about": json.RawMessage(fmt.Sprintf("%q", b))})
		editDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("edit never held its memory row")
	}
	undoReadStarted := make(chan struct{})
	var undoOnce sync.Once
	undoDB := &DB{sql: sql.OpenDB(pauseConnector{conn, func(q string) {
		if strings.HasPrefix(q, "SELECT id,session_key,job_id,detail FROM memories WHERE") {
			undoOnce.Do(func() { close(undoReadStarted) })
		}
	}})}
	defer undoDB.Close()
	undoDone := make(chan error, 1)
	go func() { _, err := undoDB.UnmergeNames(src.Session, op); undoDone <- err }()
	select {
	case <-undoReadStarted:
	case err := <-undoDone:
		t.Fatalf("undo finished before memory read: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("undo never reached memory read")
	}
	unblock()
	if err := <-editDone; err != nil {
		t.Fatal("edit", err)
	}
	undoErr := <-undoDone
	if undoErr != nil {
		t.Logf("concurrent unmerge returned: %v", undoErr)
		if _, err := db.UnmergeNames(src.Session, op); err != nil {
			t.Fatal("manual retry", err)
		}
		t.Log("manual retry succeeded after edit ended")
	}
	got := c16Memory(t, db, id)
	rows, err := db.sql.Query(`SELECT origin_ref,before_ref,after_ref FROM name_ref_changes WHERE row_table='memories' AND row_id=? AND path='about' ORDER BY id`, memoryRowNumber(id))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var x, y, z string
		rows.Scan(&x, &y, &z)
		t.Logf("journal origin=%s before=%s after=%s", x, y, z)
	}
	t.Logf("original=%s explicit_edit=%s after_unmerge=%s undo snapshot preceded edit commit", a, b, got.About)
	if got.About != b {
		t.Fatalf("explicit same-value C16 edit was lost: after unmerge about=%s, want supplied %s", got.About, b)
	}
	if undoErr != nil {
		t.Errorf("concurrent management operation failed, manual retry required: %v", undoErr)
	}
}
