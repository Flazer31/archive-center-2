package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestDeliveryTraceC21Projection(t *testing.T) {
	db := testDB(t)
	locator := &DeliveryLocator{TurnID: 19, Turn: 4, SourceKey: "observed-input-source"}
	trace := []TraceRow{
		{ID: "M1", Kind: "event", Result: "over_ceiling", Size: 350},
		{ID: "M2", Kind: "fact", Parent: "M1", Result: "parent_not_taken"},
		{ID: "M3", Kind: "view", Result: "taken", Secret: true, Size: 120},
		{ID: "M4", Kind: "fact", Result: "contrast_removed", Size: 60},
	}
	sum := DeliverySummary{Locator: locator, Input: "private input", Candidates: len(trace) - 1,
		IDs: []string{"M3"}, CharsMax: 300, SecretMax: 400, SecretChars: trace[2].Size,
		Memories: []InjectedMemory{{ListedMemory: ListedMemory{ID: "M3", Body: "historical private snapshot"}}}}
	id, err := db.SaveDeliveryTrace("chat:A", time.Now(), sum, "historical block", trace)
	if err != nil || id == 0 {
		t.Fatalf("save: %d %v", id, err)
	}
	// Close and reopen the physical SQL pool, not just the HTTP projection.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_" + t.Name() + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	db = reopened
	index, err := db.DeliveryTraceIndex("chat:A", 1)
	if err != nil || len(index) != 1 {
		t.Fatalf("index: %+v %v", index, err)
	}
	item := index[0]
	if item.ID != id || !reflect.DeepEqual(item.Locator, locator) || item.Candidates != sum.Candidates || item.Items != len(sum.IDs) || item.RowTotal != len(trace) {
		t.Fatalf("populations/locator: %+v", item)
	}
	b, _ := json.Marshal(index)
	for _, private := range []string{sum.Input, sum.Memories[0].Body, "historical block", `"summary"`, `"trace"`} {
		if strings.Contains(string(b), private) {
			t.Fatalf("metadata leak: %s", b)
		}
	}
	var all []TraceRow
	for cursor := 0; ; {
		page, err := db.DeliveryTraceRows("chat:A", id, cursor, 1)
		if err != nil || page == nil {
			t.Fatalf("page: %+v %v", page, err)
		}
		if len(page.Trace) != 1 || page.RowTotal != len(trace) || page.Block != "historical block" || !reflect.DeepEqual(page.Summary, sum) {
			t.Fatalf("page/snapshot mismatch: %+v", page)
		}
		if page.Trace[0].Result == "parent_not_taken" {
			b, _ := json.Marshal(page.Trace[0])
			var raw map[string]any
			json.Unmarshal(b, &raw)
			if _, ok := raw["size"]; ok {
				t.Fatalf("uncomputed size: %s", b)
			}
			if _, ok := raw["secret"]; ok {
				t.Fatalf("uncomputed lane: %s", b)
			}
		}
		all = append(all, page.Trace...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if !reflect.DeepEqual(all, trace) {
		t.Fatalf("page omitted/reordered results: %+v", all)
	}
	if page, err := db.DeliveryTraceRows("chat:B", id, 0, 100); err != nil || page != nil {
		t.Fatalf("cross-chat: %+v %v", page, err)
	}
	if page, err := db.DeliveryTraceRows("chat:A", id, len(trace)+1, 1); err != nil || len(page.Trace) != 0 || page.NextCursor != nil {
		t.Fatalf("past end: %+v %v", page, err)
	}
	// Truly old JSON has no locator key. Do not infer it from text or turn IDs.
	if _, err := db.sql.Exec(`UPDATE delivery_traces SET summary=JSON_REMOVE(summary,'$.locator') WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	old, err := db.DeliveryTraceIndex("chat:A", 1)
	if err != nil || old[0].Locator != nil {
		t.Fatalf("legacy inferred locator: %+v %v", old, err)
	}
	_, err = db.SaveDeliveryTrace("chat:EMPTY", time.Now(), DeliverySummary{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := db.DeliveryTraceIndex("chat:EMPTY", 1)
	if err != nil || len(empty) != 1 || empty[0].Items != 0 || empty[0].RowTotal != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	page, err := db.DeliveryTraceRows("chat:EMPTY", empty[0].ID, 0, 100)
	if err != nil || page.Trace == nil || len(page.Trace) != 0 || page.NextCursor != nil {
		t.Fatalf("empty page: %+v %v", page, err)
	}
}

// Observe bytes returned by the REAL MariaDB driver. The wrapper does not
// implement SQL or manufacture rows; it counts exactly what production reads.
type traceReadMeter struct {
	bytes, rows int
	queries     []string
}
type traceMeterConnector struct {
	driver.Connector
	meter *traceReadMeter
}
type traceMeterConn struct {
	driver.Conn
	meter *traceReadMeter
}
type traceMeterRows struct {
	driver.Rows
	meter *traceReadMeter
}

func (c traceMeterConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return traceMeterConn{conn, c.meter}, nil
}
func (c traceMeterConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.meter.queries = append(c.meter.queries, q)
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return traceMeterRows{r, c.meter}, nil
}
func (r traceMeterRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err != nil {
		return err
	}
	r.meter.rows++
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

func TestDeliveryTraceC21PageReadsOnlyProjectedRows(t *testing.T) {
	db := testDB(t)
	cfg, err := mysql.ParseDSN(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_" + t.Name() + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	cfg.InterpolateParams = true
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	meter := &traceReadMeter{}
	projected := &DB{sql: sql.OpenDB(traceMeterConnector{conn, meter})}
	defer projected.Close()
	var wire []int
	for _, count := range []int{100, 2000} {
		trace := make([]TraceRow, count)
		for i := range trace {
			trace[i] = TraceRow{ID: fmt.Sprintf("M%06d", i), Result: "taken"}
		}
		// Huge unrequested rows and snapshots must not cross the list boundary.
		for i := 10; i < count; i++ {
			trace[i].Parent = strings.Repeat("unused", 512)
		}
		id, err := db.SaveDeliveryTrace("chat:BIG", time.Now(), DeliverySummary{Input: strings.Repeat("input", 1000), Memories: []InjectedMemory{{ListedMemory: ListedMemory{Body: strings.Repeat("snapshot", 1000)}}}}, "fixed block", trace)
		if err != nil {
			t.Fatal(err)
		}
		*meter = traceReadMeter{}
		index, err := projected.DeliveryTraceIndex("chat:BIG", 1)
		if err != nil || len(index) != 1 || index[0].RowTotal != count {
			t.Fatalf("index: %+v %v", index, err)
		}
		if meter.rows != 1 || meter.bytes > 1024 {
			t.Fatalf("index read snapshots/trace: %+v", meter)
		}
		*meter = traceReadMeter{}
		page, err := projected.DeliveryTraceRows("chat:BIG", id, 0, 3)
		if err != nil || len(page.Trace) != 3 || page.RowTotal != count {
			t.Fatalf("page: %+v %v", page, err)
		}
		if meter.rows != 1+len(page.Trace) {
			t.Fatalf("read full trace: %+v", meter)
		}
		for _, q := range meter.queries {
			if strings.Contains(q, "summary, block, trace") {
				t.Fatal("legacy full reader used")
			}
		}
		wire = append(wire, meter.bytes)
		t.Logf("stored_rows=%d returned_rows=%d driver_text_bytes=%d", count, len(page.Trace), meter.bytes)
	}
	if wire[1] > wire[0]+16 {
		t.Fatalf("page bytes scale with whole trace: %v", wire)
	}
}
