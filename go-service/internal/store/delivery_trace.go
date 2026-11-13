package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// DeliveryLocator is an observation for browsing, never a lifecycle identity.
// A nil locator means the request's turn was not recorded (including old traces).
type DeliveryLocator struct {
	TurnID    int    `json:"turn_id"`
	Turn      int    `json:"turn"` // active chat display position at request time
	SourceKey string `json:"source_key"`
}

// deliveryTracesKept is how many latest deliveries a chat keeps.
const deliveryTracesKept = 50

// DeliveryStage is one step of a memory request and the milliseconds it
// took: observe, session (reading the chat, waiting for its lock), embed,
// body, select (Go's choice), preprocess, narrative, other (the rest).
type DeliveryStage struct {
	Name string `json:"name"`
	MS   int64  `json:"ms"`
}

// DeliverySummary is what a request asked and got, beside its candidates.
type DeliverySummary struct {
	Locator         *DeliveryLocator `json:"locator"`
	Input           string           `json:"input"`
	Window          int              `json:"window"`
	WindowFrom      string           `json:"window_from"`
	Ranked          string           `json:"ranked"`
	RecentQueryUsed *bool            `json:"recent_query_used,omitempty"`
	Candidates      int              `json:"candidates"`
	Chars           int              `json:"chars"`
	CharsMax        int              `json:"chars_max"`
	SecretChars     int              `json:"secret_chars"`
	SecretMax       int              `json:"secret_max"`
	IDs             []string         `json:"ids"`
	SecretIDs       []string         `json:"secret_ids"`
	Warning         string           `json:"warning,omitempty"`
	QueryError      string           `json:"query_error,omitempty"`
	TotalMS         int64            `json:"total_ms"`
	// Stages split TotalMS in the order the request ran them (1.1 A1);
	// absent in traces saved before 1.1.
	Stages []DeliveryStage `json:"stages,omitempty"`
	// Memories is the management view of the delivered records as they
	// were when sent (later edits do not change it).
	Memories []InjectedMemory `json:"memories"`
	// Preprocess is what a preprocessing mode did (nil when off)
	Preprocess *PreprocessRecord `json:"preprocess,omitempty"`
	// Narrative is what narrative support did (nil when off)
	Narrative *NarrativeRecord `json:"narrative,omitempty"`
	// Prefix compares the request with the chat's previous one (1.1 A2);
	// nil for the first request since the backend started
	Prefix *RequestPrefix `json:"prefix,omitempty"`
}

// DeliveryTrace is one stored delivery.
type DeliveryTrace struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	Summary DeliverySummary `json:"summary"`
	Block   string          `json:"block"`
	Trace   []TraceRow      `json:"trace"`
}

// SaveDeliveryTrace keeps one delivery of a chat and drops the chat's older
// ones beyond deliveryTracesKept.
func (d *DB) SaveDeliveryTrace(session string, at time.Time, sum DeliverySummary, block string, trace []TraceRow) (int64, error) {
	s, err := json.Marshal(sum)
	if err != nil {
		return 0, err
	}
	if trace == nil {
		trace = []TraceRow{}
	}
	t, err := json.Marshal(trace)
	if err != nil {
		return 0, err
	}
	result, err := d.sql.Exec(`INSERT INTO delivery_traces (session_key, at, summary, block, trace) VALUES (?,?,?,?,?)`, session, at.UTC(), string(s), block, string(t))
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, err = d.sql.Exec(`DELETE FROM delivery_traces WHERE session_key=? AND id <= (
		SELECT id FROM (SELECT id FROM delivery_traces WHERE session_key=? ORDER BY id DESC LIMIT 1 OFFSET ?) AS older)`, session, session, deliveryTracesKept)
	return id, err
}

// DeliveryTraceIndex contains only browsing metadata. Input, error text,
// snapshots and Block deliberately have no field in this projection.
type DeliveryTraceIndex struct {
	ID          int64            `json:"id"`
	At          time.Time        `json:"at"`
	Locator     *DeliveryLocator `json:"locator"`
	Candidates  int              `json:"candidates"`
	Items       int              `json:"items"`     // final IDs, not Candidates
	RowTotal    int              `json:"row_total"` // all stored TraceRows
	Chars       int              `json:"chars"`
	CharsMax    int              `json:"chars_max"`
	SecretChars int              `json:"secret_chars"`
	SecretMax   int              `json:"secret_max"`
}

// DeliveryTraceIndex reads a whitelist of scalar JSON paths in SQL. It never
// transfers the full summary (which includes snapshots) or trace to Go.
func (d *DB) DeliveryTraceIndex(session string, limit int) ([]DeliveryTraceIndex, error) {
	rows, err := d.sql.Query(`SELECT id, at, JSON_OBJECT(
		'locator', JSON_EXTRACT(summary, '$.locator'),
		'candidates', JSON_EXTRACT(summary, '$.candidates'),
		'items', COALESCE(JSON_LENGTH(JSON_QUERY(summary, '$.ids')), 0),
		'row_total', JSON_LENGTH(trace),
		'chars', JSON_EXTRACT(summary, '$.chars'),
		'chars_max', JSON_EXTRACT(summary, '$.chars_max'),
		'secret_chars', JSON_EXTRACT(summary, '$.secret_chars'),
		'secret_max', JSON_EXTRACT(summary, '$.secret_max'))
		FROM delivery_traces WHERE session_key=? ORDER BY id DESC LIMIT ?`, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeliveryTraceIndex{}
	for rows.Next() {
		var item DeliveryTraceIndex
		var metadata string
		if err := rows.Scan(&item.ID, &item.At, &metadata); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(metadata), &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

const DeliveryTracePageMax = 100

type DeliveryTracePage struct {
	DeliveryTrace
	RowCursor  int  `json:"row_cursor"` // zero-based stored array offset
	RowLimit   int  `json:"row_limit"`
	RowTotal   int  `json:"row_total"`
	NextCursor *int `json:"next_cursor"` // null at the end
}

// DeliveryTraceRows reads one chat-owned delivery and only the requested
// candidate JSON elements. DB JSON processing may scan the array; Go receives
// at most one page, plus this delivery's existing snapshot and Block.
func (d *DB) DeliveryTraceRows(session string, id int64, cursor, limit int) (*DeliveryTracePage, error) {
	if cursor < 0 {
		cursor = 0
	}
	if limit <= 0 || limit > DeliveryTracePageMax {
		limit = DeliveryTracePageMax
	}
	page := &DeliveryTracePage{RowCursor: cursor, RowLimit: limit}
	var summary string
	err := d.sql.QueryRow(`SELECT id, at, summary, block, JSON_LENGTH(trace)
		FROM delivery_traces WHERE session_key=? AND id=?`, session, id).
		Scan(&page.ID, &page.At, &summary, &page.Block, &page.RowTotal)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(summary), &page.Summary); err != nil {
		return nil, err
	}
	// Bound the arithmetic by the stored array length, including huge cursors.
	end := page.RowTotal
	if cursor < end && limit < end-cursor {
		end = cursor + limit
	}
	rows, err := d.sql.Query(`SELECT JSON_EXTRACT(d.trace, CONCAT('$[', j.n-1, ']'))
		FROM delivery_traces AS d,
		JSON_TABLE(d.trace, '$[*]' COLUMNS (n FOR ORDINALITY)) AS j
		WHERE d.session_key=? AND d.id=? AND j.n>? AND j.n<=? ORDER BY j.n`, session, id, cursor, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page.Trace = []TraceRow{}
	for rows.Next() {
		var raw string
		var row TraceRow
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, err
		}
		page.Trace = append(page.Trace, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if end < page.RowTotal {
		page.NextCursor = &end
	}
	return page, nil
}

// DeliveryTraces reads a chat's latest deliveries, the newest first.
func (d *DB) DeliveryTraces(session string, limit int) ([]DeliveryTrace, error) {
	out := []DeliveryTrace{}
	rows, err := d.sql.Query(`SELECT id, at, summary, block, trace FROM delivery_traces WHERE session_key=? ORDER BY id DESC LIMIT ?`, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t DeliveryTrace
		var s, tr string
		if err := rows.Scan(&t.ID, &t.At, &s, &t.Block, &tr); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(s), &t.Summary); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tr), &t.Trace); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
