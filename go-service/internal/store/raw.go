package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/observe"
)

type DB struct{ sql *sql.DB }

// Open connects and applies the schema.
func Open(dsn string) (*DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	var version string
	if err := db.QueryRow(`SELECT VERSION()`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if err := checkMariaDBVersion(version); err != nil {
		db.Close()
		return nil, err
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	return &DB{sql: db}, nil
}

func checkMariaDBVersion(version string) error {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) >= 2 {
		major, majorErr := strconv.Atoi(parts[0])
		minor, minorErr := strconv.Atoi(parts[1])
		if majorErr == nil && minorErr == nil && (major > 11 || major == 11 && minor >= 8) {
			return nil
		}
	}
	return fmt.Errorf("MariaDB 11.8 or newer required; actual server version: %q", version)
}

func (d *DB) Close() error { return d.sql.Close() }

// RawSink stores observation records verbatim; it implements observe.Sink.
type RawSink struct{ DB *DB }

func (s RawSink) Ready() error { return s.DB.sql.Ping() }

// Write stores a batch in one transaction. A resent event (same boot_id and
// seq, after a lost response) is stored once.
func (s RawSink) Write(records []observe.Record) ([]observe.Record, error) {
	tx, err := s.DB.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT IGNORE INTO raw_observations
		(received_at, receive_seq, plugin_version, boot_id, seq, hook, chat_id, chat_id2, event) VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	var stored []observe.Record
	for _, r := range records {
		var head struct {
			BootID *string `json:"boot_id"`
			Seq    *int64  `json:"seq"`
			Hook   string  `json:"hook"`
		}
		_ = json.Unmarshal(r.Event, &head)
		at, err := time.Parse(time.RFC3339Nano, r.ReceivedAt)
		if err != nil {
			return nil, fmt.Errorf("received_at: %w", err)
		}
		var chat, chat2 *string
		if ids := ingest.ChatsOf(r.Event); len(ids) > 0 {
			chat = &ids[0]
			if len(ids) > 1 {
				chat2 = &ids[1]
			}
		}
		res, err := stmt.Exec(at, r.ReceiveSeq, r.PluginVersion, head.BootID, head.Seq, head.Hook, chat, chat2, string(r.Event))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			stored = append(stored, r)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stored, nil
}

// RawTextsOf passes each stored event of a plugin chat, in receive order.
func (d *DB) RawTextsOf(chat string, fn func(json.RawMessage)) error {
	return d.each(`SELECT event FROM raw_observations WHERE chat_id=? OR chat_id2=? ORDER BY id`, []any{chat, chat}, func(rows *sql.Rows) error {
		var ev string
		if err := rows.Scan(&ev); err != nil {
			return err
		}
		fn(json.RawMessage(ev))
		return nil
	})
}

// RawEvent is one stored observation as read back.
type RawEvent struct {
	ID    int64
	Hook  string
	Event json.RawMessage
}

// RawAfter returns stored observations with id > after, in receive order.
func (d *DB) RawAfter(after int64, limit int) ([]RawEvent, error) {
	rows, err := d.sql.Query(`SELECT id, hook, event FROM raw_observations WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawEvent
	for rows.Next() {
		var e RawEvent
		var ev string
		if err := rows.Scan(&e.ID, &e.Hook, &ev); err != nil {
			return nil, err
		}
		e.Event = json.RawMessage(ev)
		out = append(out, e)
	}
	return out, rows.Err()
}
