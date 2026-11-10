package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// Wrap only the SQL transport, not the production reader. The real MariaDB
// still executes every query; count bytes/rows actually returned to Go.
type chatSQLTrace struct{ bytes, execs, maxBodyRows int }
type chatTraceConnector struct {
	driver.Connector
	trace *chatSQLTrace
}

func (c chatTraceConnector) Connect(ctx context.Context) (driver.Conn, error) {
	v, e := c.Connector.Connect(ctx)
	if e != nil {
		return nil, e
	}
	return chatTraceConn{Conn: v, trace: c.trace}, nil
}

type chatTraceConn struct {
	driver.Conn
	trace *chatSQLTrace
}

func (c chatTraceConn) Prepare(q string) (driver.Stmt, error) {
	s, e := c.Conn.Prepare(q)
	if e != nil {
		return nil, e
	}
	return chatTraceStmt{Stmt: s, trace: c.trace, query: q}, nil
}

type chatTraceStmt struct {
	driver.Stmt
	trace *chatSQLTrace
	query string
}

func (s chatTraceStmt) Query(args []driver.Value) (driver.Rows, error) {
	r, e := s.Stmt.Query(args)
	if e != nil {
		return nil, e
	}
	return chatTracedRows(r, s.query, s.trace), nil
}
func (s chatTraceStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.trace.execs++
	return s.Stmt.Exec(args)
}
func chatTracedRows(r driver.Rows, q string, trace *chatSQLTrace) driver.Rows {
	body := strings.Contains(q, "SELECT") && (strings.Contains(q, "detail") || strings.Contains(q, "data")) && !strings.HasPrefix(q, "SET STATEMENT")
	return &chatTraceRows{Rows: r, trace: trace, body: body}
}

func (c chatTraceConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c chatTraceConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	r, e := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if e != nil {
		return nil, e
	}
	return chatTracedRows(r, q, c.trace), nil
}
func (c chatTraceConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.trace.execs++
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

type chatTraceRows struct {
	driver.Rows
	trace *chatSQLTrace
	body  bool
	n     int
}

func (r *chatTraceRows) Next(values []driver.Value) error {
	e := r.Rows.Next(values)
	if e != nil {
		return e
	}
	r.n++
	for _, v := range values {
		switch b := v.(type) {
		case []byte:
			r.trace.bytes += len(b)
		case string:
			r.trace.bytes += len(b)
		}
	}
	if r.body && r.n > r.trace.maxBodyRows {
		r.trace.maxBodyRows = r.n
	}
	return nil
}
func chatTracedDB(t *testing.T, db *DB) (*DB, *chatSQLTrace) {
	t.Helper()
	var name string
	if err := db.sql.QueryRow("SELECT DATABASE()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(os.Getenv("AC2_TEST_MARIADB_DSN") + name + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	c, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	trace := &chatSQLTrace{}
	traced := &DB{sql: sql.OpenDB(chatTraceConnector{Connector: c, trace: trace})}
	t.Cleanup(func() { traced.Close() })
	return traced, trace
}

func TestC26BoundedSQLTransport(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:BIG", strings.Repeat("x", 120000))
	u2Save(t, db, s, 0, `{"memories":[{"kind":"fact","body":"`+strings.Repeat("y", 200000)+`"}]}`)
	other := u2Session(t, db, "chat:OTHER", "other")
	traced, trace := chatTracedDB(t, db)
	v, err := traced.CompareChats(context.Background(), s.Key, other.Key, [2]ComparePage{{Limit: 1}, {Limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if v[0].Memories["total"] != 1 || v[0].Originals[0]["input"] != strings.Repeat("x", 500) {
		t.Fatal("fixture read bypassed")
	}
	if trace.bytes > 10000 || trace.execs != 0 {
		t.Fatalf("whole text read or write: %+v", trace)
	}
	t.Logf("compare SQL text bytes=%d, Exec calls=%d", trace.bytes, trace.execs)
}
