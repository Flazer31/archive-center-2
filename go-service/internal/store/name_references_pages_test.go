package store

import (
	"archivecenter2/goservice/internal/memory"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"github.com/go-sql-driver/mysql"
	"strings"
	"testing"
)

type c05Connector struct {
	driver.Connector
	meter *c17Meter
}
type c05Conn struct {
	driver.Conn
	meter *c17Meter
}

func (c c05Connector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return c05Conn{conn, c.meter}, nil
}
func (c c05Conn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c c05Conn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}
func (c c05Conn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	c.meter.queries = append(c.meter.queries, q)
	if strings.Contains(q, "SELECT turn_id, greeting, confirmed") || strings.Contains(q, "session_progress") {
		c.meter.forbidden++
		return nil, fmt.Errorf("whole session read")
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
	if err != nil {
		return nil, err
	}
	return &c17Rows{Rows: rows, meter: c.meter, page: strings.Contains(q, "SELECT id,session_key,job_id,detail FROM memories")}, nil
}
func TestC17RewriteBoundedPages(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:page", Turn: 1}
	out := memory.Output{Entities: []memory.Entity{{ID: "n1", Name: "A"}, {ID: "n2", Name: "B"}}}
	for i := 0; i < 206; i++ {
		out.Memories = append(out.Memories, memory.Memory{ID: fmt.Sprintf("m%d", i+1), Kind: "fact", Body: fmt.Sprintf("record %d", i), About: "n1"})
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveGeneration(j, Generation{Output: out}); err != nil {
		t.Fatal(err)
	}
	ids, _ := db.ItemIDs(j.ID)
	u3Stored(t, db, memory.Source{Session: "chat:other", Turn: 1}, `{"memories":[{"id":"m1","kind":"fact","body":"`+strings.Repeat("unrelated", 3000)+`"}]}`)
	cfg, err := mysql.ParseDSN(dbDSNForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.InterpolateParams = true
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	meter := &c17Meter{}
	measured := &DB{sql: sql.OpenDB(c05Connector{conn, meter})}
	defer measured.Close()
	op := c17MergeID(t, measured, src.Session, ids["entities/1"], ids["entities/0"])
	it, err := db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range it.Memories {
		if m.About != ids["entities/1"] {
			t.Fatal("cursor omitted merge row", m.ID)
		}
	}
	if _, err := measured.UnmergeNames(src.Session, op); err != nil {
		t.Fatal(err)
	}
	it, err = db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range it.Memories {
		if m.About != ids["entities/0"] {
			t.Fatal("cursor omitted undo row", m.ID)
		}
	}
	if meter.maxPage != 100 || meter.forbidden != 0 {
		t.Fatal("unbounded read", meter.maxPage, meter.forbidden)
	}
	for _, q := range meter.queries {
		if strings.Contains(q, "SELECT id,session_key,job_id,detail FROM memories") && !strings.Contains(q, "ORDER BY id LIMIT 100") {
			t.Fatal("unbounded memory page", q)
		}
	}
	t.Logf("206 affected bodies: max page=%d; full-session attempts=%d", meter.maxPage, meter.forbidden)
}
