package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"archivecenter2/goservice/internal/observe"
)

// Needs a disposable MariaDB: AC2_TEST_MARIADB_DSN=user@tcp(127.0.0.1:port)/
// The test creates and drops its own database.
func testDB(t *testing.T) *DB {
	t.Helper()
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, err := sql.Open("mysql", base)
	if err != nil {
		t.Fatal(err)
	}
	name := "ac2_test_" + t.Name()
	boot.Exec("DROP DATABASE IF EXISTS " + name)
	if _, err := boot.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE " + name); boot.Close() })
	db, err := Open(base + name + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func rec(seq uint64, ev string) observe.Record {
	return observe.Record{ReceivedAt: "2026-10-22T06:00:00.123456Z", ReceiveSeq: seq, PluginVersion: "0.0.3", Event: json.RawMessage(ev)}
}

func TestRawObservationsReadBackByteForByte(t *testing.T) {
	db := testDB(t)
	evs := []string{
		`{"boot_id":"b1","seq":1,"hook":"input","content":{"text":"한국어 입력 😀 \"따옴표\"\n줄바꿈"}}`,
		`{"hook":"gap","dropped_before":3}`,
		`{"boot_id":"b1","seq":2,"hook":"output","event_chat":{"rows":[]}}`,
	}
	sink := RawSink{DB: db}
	if _, err := sink.Write([]observe.Record{rec(1, evs[0]), rec(2, evs[1]), rec(3, evs[2])}); err != nil {
		t.Fatal(err)
	}
	got, err := db.RawAfter(0, 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("read %d err %v", len(got), err)
	}
	for i, e := range got {
		if string(e.Event) != evs[i] {
			t.Fatalf("row %d changed:\n got %s\nwant %s", i, e.Event, evs[i])
		}
	}
	if got[0].Hook != "input" || got[1].Hook != "gap" {
		t.Fatalf("hooks %q %q", got[0].Hook, got[1].Hook)
	}
}

// A batch resent after a lost response must not double the events, and
// events without plugin order (gap records) are all kept.
func TestResentBatchStoredOnce(t *testing.T) {
	db := testDB(t)
	sink := RawSink{DB: db}
	batch := []observe.Record{rec(1, `{"boot_id":"b1","seq":1,"hook":"input"}`), rec(2, `{"hook":"gap","dropped_before":1}`)}
	for i := 0; i < 2; i++ {
		stored, err := sink.Write(batch)
		if err != nil {
			t.Fatal(err)
		}
		// only new records go on to the lifecycle pipeline
		if want := []int{2, 1}[i]; len(stored) != want {
			t.Fatalf("write %d returned %d new records, want %d", i, len(stored), want)
		}
	}
	got, _ := db.RawAfter(0, 10)
	if len(got) != 3 {
		t.Fatalf("want 1 input + 2 gaps, got %d", len(got))
	}
	if _, err := sink.Write([]observe.Record{rec(5, `{"boot_id":"b2","seq":1,"hook":"input"}`)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.RawAfter(0, 10); len(got) != 4 {
		t.Fatalf("same seq from another boot must be kept, got %d", len(got))
	}
}

// Every connection Open makes reads with snapshot isolation, whatever the
// server's default (MariaDB before 11.6.2 has it off): concurrent management
// writes rely on its conflict error to run again (againOnConflict).
func TestOpenReadsWithSnapshotIsolation(t *testing.T) {
	// the setting is asked for on every server (11.8 has it on already, so
	// the session read alone would not show it missing)
	if cfg, err := connConfig("root@tcp(127.0.0.1:1)/x"); err != nil || cfg.Params["innodb_snapshot_isolation"] != "ON" {
		t.Fatalf("connection settings: %v %v", cfg, err)
	}
	db := testDB(t)
	// three connections held at once are three distinct connections
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		c, err := db.sql.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var v string
		if err := c.QueryRowContext(ctx, `SELECT @@session.innodb_snapshot_isolation`).Scan(&v); err != nil || v != "1" {
			t.Fatalf("connection %d: innodb_snapshot_isolation %q %v", i, v, err)
		}
	}
}
