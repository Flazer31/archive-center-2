package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The chat list (user decisions 10-28): every chat the backend has a record
// of, named as the host names it, most recent first.

var chatSchema = []string{
	// a chat's names in the host, sent by the plugin with its snapshots
	`CREATE TABLE IF NOT EXISTS chat_labels (
		session_key VARCHAR(191) NOT NULL PRIMARY KEY,
		char_name VARCHAR(500) NOT NULL DEFAULT '',
		chat_name VARCHAR(500) NOT NULL DEFAULT '',
		updated_at DATETIME(6) NOT NULL
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
}

func init() { schema = append(schema, chatSchema...) }

// SetChatLabel keeps a chat's names; an empty one keeps the name known.
func (d *DB) SetChatLabel(session, charName, chatName string) error {
	_, err := d.sql.Exec(`INSERT INTO chat_labels (session_key, char_name, chat_name, updated_at) VALUES (?,?,?,?)
		ON DUPLICATE KEY UPDATE char_name=IF(VALUES(char_name)='', char_name, VALUES(char_name)),
		chat_name=IF(VALUES(chat_name)='', chat_name, VALUES(chat_name)), updated_at=VALUES(updated_at)`,
		session, charName, chatName, time.Now().UTC())
	return err
}

// ChatInfo is one chat of the list.
type ChatInfo struct {
	Key      string
	CharName string
	ChatName string
	Turns    int            // turns the host still has (not inactive)
	Memories map[string]int // by kind (event, fact, view), as ChatMemories reads them
	Names    int
	LastAt   time.Time // the last observation of the chat
	// a branch: the chat it was made from and the parent turn it leaves at
	// (0 when that turn is not known)
	ParentKey  string
	ParentTurn int
}

// ChatList gives every chat with turns, the most recently observed first.
func (d *DB) ChatList() ([]ChatInfo, error) {
	byKey := map[string]*ChatInfo{}
	var order []*ChatInfo
	err := d.each(`SELECT session_key, SUM(inactive=0) FROM logical_turns GROUP BY session_key`, nil, func(rows *sql.Rows) error {
		c := &ChatInfo{Memories: map[string]int{}}
		if err := rows.Scan(&c.Key, &c.Turns); err != nil {
			return err
		}
		byKey[c.Key] = c
		order = append(order, c)
		return nil
	})
	if err != nil || len(order) == 0 {
		return nil, err
	}
	err = d.each(`SELECT session_key, char_name, chat_name FROM chat_labels`, nil, func(rows *sql.Rows) error {
		var k, ch, ct string
		if err := rows.Scan(&k, &ch, &ct); err != nil {
			return err
		}
		if c := byKey[k]; c != nil {
			c.CharName, c.ChatName = ch, ct
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, c := range order {
		c.Memories, err = d.chatMemoryCounts(c.Key)
		if err != nil {
			return nil, err
		}
	}
	err = d.each(`SELECT session_key, COUNT(*) FROM names GROUP BY session_key`, nil, func(rows *sql.Rows) error {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		if c := byKey[k]; c != nil {
			c.Names = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = d.each(`SELECT chat_id, MAX(received_at) FROM raw_observations WHERE chat_id IS NOT NULL GROUP BY chat_id`, nil, func(rows *sql.Rows) error {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return err
		}
		if c := byKey["chat:"+id]; c != nil && at.After(c.LastAt) {
			c.LastAt = at
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = d.each(`SELECT b.session_key, b.parent_chat, COALESCE(MIN(m.turn_id), 0) FROM session_branch b
		LEFT JOIN turn_members m ON m.session_key = b.parent_chat AND m.member_key = b.parent_message
		GROUP BY b.session_key, b.parent_chat`, nil, func(rows *sql.Rows) error {
		var k, parent string
		var turn int
		if err := rows.Scan(&k, &parent, &turn); err != nil {
			return err
		}
		if c := byKey[k]; c != nil {
			c.ParentKey, c.ParentTurn = parent, turn // parent_chat is a chat key, parent_message a member key
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]ChatInfo, 0, len(order))
	for _, c := range order {
		out = append(out, *c)
	}
	// most recent first; never observed (none) last, by key for a stable order
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && newer(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func newer(a, b ChatInfo) bool {
	if !a.LastAt.Equal(b.LastAt) {
		return a.LastAt.After(b.LastAt)
	}
	return a.Key < b.Key
}

// ComparePage controls only preview transport, never the read/selection scope.
type ComparePage struct {
	TurnAfter   int
	MemoryAfter int64
	Limit       int
}

type ChatComparison struct {
	SessionKey    string           `json:"session_key"`
	ChatID        string           `json:"chat_id"`
	CharName      string           `json:"char_name"`
	ChatName      string           `json:"chat_name"`
	Turns         int              `json:"turns"`
	Memories      map[string]int   `json:"memories"`
	Names         int              `json:"names"`
	LastAt        *time.Time       `json:"last_at"`
	BranchOf      map[string]any   `json:"branch_of"`
	Originals     []map[string]any `json:"originals"`
	MemoryPreview []map[string]any `json:"memory_preview"`
	NextTurn      *int             `json:"next_turn_after"`
	NextMemory    *int64           `json:"next_memory_after"`
}

// CompareChats reads exactly the two requested chats at one committed state.
// No pipeline/worker is involved. Counts stay in SQL; original/memory snippets
// are limited separately by PK pages and 500 Unicode characters per body.
func (d *DB) CompareChats(ctx context.Context, left, right string, pages [2]ComparePage) ([2]ChatComparison, error) {
	var out [2]ChatComparison
	r, err := d.beginChatRead(ctx, left)
	if err != nil {
		return out, err
	}
	defer r.tx.Rollback()
	for i, chat := range []string{left, right} {
		r.chat = chat
		out[i], err = r.compare(pages[i])
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func (r *chatRead) compare(p ComparePage) (ChatComparison, error) {
	v := ChatComparison{SessionKey: r.chat, ChatID: strings.TrimPrefix(r.chat, "chat:"), Memories: map[string]int{"event": 0, "fact": 0, "view": 0, "total": 0}, Originals: []map[string]any{}, MemoryPreview: []map[string]any{}}
	var exists int
	err := r.tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM logical_turns WHERE session_key=?) OR EXISTS(SELECT 1 FROM chat_labels WHERE session_key=?)`, r.chat, r.chat).Scan(&exists)
	if err != nil {
		return v, err
	}
	if exists == 0 {
		return v, ErrChatNotFound
	}
	err = r.tx.QueryRow(`SELECT COUNT(*) FROM logical_turns WHERE session_key=? AND NOT inactive`, r.chat).Scan(&v.Turns)
	if err != nil {
		return v, err
	}
	err = r.tx.QueryRow(`SELECT char_name,chat_name FROM chat_labels WHERE session_key=?`, r.chat).Scan(&v.CharName, &v.ChatName)
	if err != nil && err != sql.ErrNoRows {
		return v, err
	}
	var last sql.NullTime
	err = r.tx.QueryRow(`SELECT MAX(received_at) FROM raw_observations WHERE chat_id=? OR chat_id2=?`, v.ChatID, v.ChatID).Scan(&last)
	if err != nil {
		return v, err
	}
	if last.Valid {
		v.LastAt = &last.Time
	}
	var parent, message string
	var turn int
	err = r.tx.QueryRow(`SELECT b.parent_chat,b.parent_message,COALESCE((SELECT MIN(m.turn_id) FROM turn_members m WHERE m.session_key=b.parent_chat AND m.member_key=b.parent_message),(SELECT MIN(a.turn_id) FROM response_rows a WHERE a.session_key=b.parent_chat AND a.row_key=b.parent_message),0) FROM session_branch b WHERE b.session_key=?`, r.chat).Scan(&parent, &message, &turn)
	if err != nil && err != sql.ErrNoRows {
		return v, err
	}
	if err == nil {
		v.BranchOf = map[string]any{"session_key": parent, "chat_id": strings.TrimPrefix(parent, "chat:"), "message_key": message, "turn_id": turn}
	}
	jobs, local, err := r.jobs()
	if err != nil {
		return v, err
	}
	in, args := inJobs(jobs)
	err = eachOn(r.tx, `SELECT kind,COUNT(*) FROM memories WHERE job_id IN `+in+` GROUP BY kind`, args, func(rows *sql.Rows) error {
		var k string
		var n int
		if e := rows.Scan(&k, &n); e != nil {
			return e
		}
		v.Memories[k] = n
		v.Memories["total"] += n
		return nil
	})
	if err != nil {
		return v, err
	}
	err = r.tx.QueryRow(`SELECT COUNT(*) FROM names WHERE job_id IN `+in, args...).Scan(&v.Names)
	if err != nil {
		return v, err
	}
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 10
	}
	err = eachOn(r.tx, `SELECT t.turn_id,t.confirmed,t.off,
	 COALESCE((SELECT LEFT(m.data,500) FROM turn_members m WHERE m.session_key=t.session_key AND m.turn_id=t.turn_id AND m.state='active' ORDER BY m.ord LIMIT 1),''),
	 COALESCE((SELECT LEFT(v.data,500) FROM response_rows a JOIN revisions v ON v.session_key=a.session_key AND v.row_key=a.row_key AND v.seq=(SELECT MAX(x.seq) FROM revisions x WHERE x.session_key=a.session_key AND x.row_key=a.row_key) WHERE a.session_key=t.session_key AND a.turn_id=t.turn_id AND a.state='active' AND NOT a.off AND (a.generation_id='' OR v.generation_id='' OR a.generation_id=v.generation_id) ORDER BY a.ord LIMIT 1),'')
	 FROM logical_turns t WHERE t.session_key=? AND t.turn_id>? AND NOT t.inactive ORDER BY t.turn_id LIMIT ?`, []any{r.chat, p.TurnAfter, p.Limit + 1}, func(rows *sql.Rows) error {
		var id int
		var confirmed, off bool
		var input, response string
		if e := rows.Scan(&id, &confirmed, &off, &input, &response); e != nil {
			return e
		}
		if len(v.Originals) == p.Limit {
			last := int(v.Originals[len(v.Originals)-1]["turn_id"].(int))
			v.NextTurn = &last
			return nil
		}
		v.Originals = append(v.Originals, map[string]any{"turn_id": id, "confirmed": confirmed, "off": off, "input": input, "response": response})
		return nil
	})
	if err != nil {
		return v, err
	}
	err = eachOn(r.tx, `SELECT m.id,m.job_id,m.session_key,m.kind,LEFT(m.body,500),
	 EXISTS(SELECT 1 FROM awareness a WHERE a.job_id IN `+in+` AND a.kind IN ('unaware','hide_from') AND (a.target=CONCAT('M',m.id) OR a.target LIKE CONCAT('M',m.id,'.%')))
	 FROM memories m WHERE m.job_id IN `+in+` AND m.id>? ORDER BY m.id LIMIT ?`, append(append(append([]any{}, args...), args...), p.MemoryAfter, p.Limit+1), func(rows *sql.Rows) error {
		var id, job int64
		var session, kind, body string
		var secret bool
		if e := rows.Scan(&id, &job, &session, &kind, &body, &secret); e != nil {
			return e
		}
		if len(v.MemoryPreview) == p.Limit {
			last := v.MemoryPreview[len(v.MemoryPreview)-1]["pk"].(int64)
			v.NextMemory = &last
			return nil
		}
		v.MemoryPreview = append(v.MemoryPreview, map[string]any{"pk": id, "id": fmt.Sprintf("M%d", id), "job_id": job, "source_session": session, "turn_id": local[job], "kind": kind, "body": body, "secret": secret, "inherited": session != r.chat})
		return nil
	})
	return v, err
}
