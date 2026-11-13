package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/observe"
)

type DB struct{ sql *sql.DB }

// Open connects and applies the schema. Every connection reads with
// snapshot isolation (innodb_snapshot_isolation, ON by default from
// MariaDB 11.6.2): a write to rows another write changed since this
// transaction read them fails with 1020 and is run again (againOnConflict)
// instead of overwriting that change; 1.1 serves older servers that have
// the setting off.
func Open(dsn string) (*DB, error) {
	cfg, err := connConfig(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		var my *mysql.MySQLError
		if errors.As(err, &my) && my.Number == 1193 {
			// no such variable: a server older than the setting
			plain, perr := sql.Open("mysql", dsn)
			if perr == nil {
				var version string
				plain.QueryRow(`SELECT VERSION()`).Scan(&version)
				plain.Close()
				return nil, VersionError{version}
			}
		}
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

// connConfig is the connection every backend connection is made with:
// the DSN given, read with snapshot isolation (see Open).
func connConfig(dsn string) (*mysql.Config, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["innodb_snapshot_isolation"] = "ON"
	return cfg, nil
}

// MinimumMariaDB names the oldest release of each MariaDB series served:
// the first with innodb_snapshot_isolation (MariaDB documentation), which
// also has JSON_TABLE reading a JSON object (10.6.9).
const MinimumMariaDB = "10.6.18, 10.11.8, 11.4.2"

// snapshotSince is the first patch of each series with
// innodb_snapshot_isolation (11.5.2: its source declares it; 11.5.1 not
// checked); 11.6 and later have it.
var snapshotSince = map[[2]int]int{{10, 6}: 18, {10, 11}: 8, {11, 0}: 6, {11, 1}: 5, {11, 2}: 4, {11, 4}: 2, {11, 5}: 2}

// VersionError is a MariaDB server older than MinimumMariaDB. It holds no
// credentials, so it may be shown as it is.
type VersionError struct{ Version string }

func (e VersionError) Error() string {
	return fmt.Sprintf("MariaDB %s or newer required; actual server version: %q", MinimumMariaDB, e.Version)
}

func checkMariaDBVersion(version string) error {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) >= 2 {
		major, majorErr := strconv.Atoi(parts[0])
		minor, minorErr := strconv.Atoi(parts[1])
		patch, patchErr := 0, error(nil) // "X.Y" alone: patch 0
		if len(parts) == 3 {
			digits := len(parts[2]) - len(strings.TrimLeft(parts[2], "0123456789"))
			patch, patchErr = strconv.Atoi(parts[2][:digits])
		}
		if majorErr == nil && minorErr == nil && patchErr == nil {
			if since, ok := snapshotSince[[2]int{major, minor}]; ok && patch >= since {
				return nil
			}
			if major > 11 || major == 11 && minor >= 6 {
				return nil
			}
		}
	}
	return VersionError{version}
}

// CheckMariaDBVersion is checkMariaDBVersion for a version read elsewhere
// (the start file reads the system server's "mariadbd --version").
func CheckMariaDBVersion(version string) error { return checkMariaDBVersion(version) }

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
