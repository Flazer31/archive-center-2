package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// SaveSession writes the current state of a session and its new events in
// one transaction. Turns, members and rows are kept with a state; the text
// of a reply a reroll replaced is emptied and a reply's earlier texts are
// deleted (only the newest value is kept), as the session reports them.
func (d *DB) SaveSession(s *lifecycle.Session, events []lifecycle.Event, renames []lifecycle.Rename) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) error {
		_, err := tx.Exec(q, args...)
		return err
	}
	// a row first stored without chatId keeps its history under the new key
	for _, r := range renames {
		for _, q := range []string{
			`UPDATE turn_members SET member_key=? WHERE session_key=? AND member_key=?`,
			`UPDATE response_rows SET row_key=? WHERE session_key=? AND row_key=?`,
			`UPDATE revisions SET row_key=? WHERE session_key=? AND row_key=?`,
		} {
			if err := exec(q, r.New, s.Key, r.Old); err != nil {
				return fmt.Errorf("rename: %w", err)
			}
		}
	}
	for _, t := range s.Turns {
		if err := exec(`INSERT INTO logical_turns (session_key, turn_id, greeting, confirmed, inactive, off, dates) VALUES (?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE confirmed=VALUES(confirmed), inactive=VALUES(inactive), off=VALUES(off), dates=VALUES(dates)`,
			s.Key, t.ID, t.Greeting, t.Confirmed, t.Inactive, t.Off, datesJSON(t.Dates)); err != nil {
			return fmt.Errorf("turn: %w", err)
		}
		for i, m := range t.Members {
			if err := exec(`INSERT INTO turn_members (session_key, member_key, turn_id, ord, data, state, cut) VALUES (?,?,?,?,?,?,?)
				ON DUPLICATE KEY UPDATE data=VALUES(data), state=VALUES(state), cut=VALUES(cut)`,
				s.Key, m.Key, t.ID, i, m.Data, string(m.State), m.Cut); err != nil {
				return fmt.Errorf("member: %w", err)
			}
		}
		for i, r := range t.Responses {
			if err := exec(`INSERT INTO response_rows (session_key, row_key, turn_id, ord, state, generation_id, awaiting_replacement, off, cut) VALUES (?,?,?,?,?,?,?,?,?)
				ON DUPLICATE KEY UPDATE ord=VALUES(ord), state=VALUES(state), generation_id=VALUES(generation_id), awaiting_replacement=VALUES(awaiting_replacement), off=VALUES(off), cut=VALUES(cut)`,
				s.Key, r.Key, t.ID, i, string(r.State), r.Gen, r.AwaitingReplacement, r.Off, r.Cut); err != nil {
				return fmt.Errorf("response: %w", err)
			}
			for _, rev := range r.Revisions {
				if err := exec(`INSERT INTO revisions (session_key, row_key, seq, data, generation_id, source, confirmed) VALUES (?,?,?,?,?,?,?)
					ON DUPLICATE KEY UPDATE confirmed=VALUES(confirmed), generation_id=VALUES(generation_id)`,
					s.Key, r.Key, rev.Seq, rev.Data, rev.GenerationID, string(rev.Source), rev.Confirmed); err != nil {
					return fmt.Errorf("revision: %w", err)
				}
			}
		}
	}
	// what the session forgot (a failed save drops the session; the reload
	// reads the last good state, so nothing is lost twice)
	pruned := s.TakePruned()
	for key, seq := range pruned.Cleared {
		if err := exec(`DELETE FROM revisions WHERE session_key=? AND row_key=? AND seq<?`, s.Key, key, seq); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
		if err := exec(`UPDATE revisions SET data='', confirmed=FALSE WHERE session_key=? AND row_key=? AND seq=?`, s.Key, key, seq); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	for key, seq := range pruned.Trimmed {
		if err := exec(`DELETE FROM revisions WHERE session_key=? AND row_key=? AND seq<?`, s.Key, key, seq); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	if err := scrubDropped(tx, s, pruned.Texts); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	if s.BranchOf[0] != "" {
		if err := exec(`INSERT INTO session_branch (session_key, parent_chat, parent_message, markers, copies_chat, copies_message) VALUES (?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE parent_chat=VALUES(parent_chat), parent_message=VALUES(parent_message), markers=VALUES(markers), copies_chat=VALUES(copies_chat), copies_message=VALUES(copies_message)`, s.Key, s.BranchOf[0], s.BranchOf[1], markersJSON(s.Markers), s.CopiesOf[0], s.CopiesOf[1]); err != nil {
			return fmt.Errorf("branch: %w", err)
		}
	}
	if s.CopiesOf[0] != "" {
		// the recorded copies (they replace any recorded against another parent)
		if err := exec(`DELETE FROM branch_copies WHERE session_key=?`, s.Key); err != nil {
			return fmt.Errorf("copies: %w", err)
		}
		for id, c := range s.Copies {
			if err := exec(`INSERT INTO branch_copies (session_key, turn_id, own_key, source) VALUES (?,?,?,?)`, s.Key, id, c.Own, c.Source); err != nil {
				return fmt.Errorf("copies: %w", err)
			}
		}
	}
	for b, q := range s.Progress {
		if err := exec(`INSERT INTO session_progress (session_key, boot_id, seq) VALUES (?,?,?) ON DUPLICATE KEY UPDATE seq=VALUES(seq)`, s.Key, b, q); err != nil {
			return fmt.Errorf("progress: %w", err)
		}
	}
	for _, e := range events {
		if err := exec(`INSERT INTO lifecycle_events (session_key, turn_id, kind, detail) VALUES (?,?,?,?)`, s.Key, e.TurnID, e.Kind, e.Detail); err != nil {
			return fmt.Errorf("event: %w", err)
		}
	}
	if err := regrant(tx, s); err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	return tx.Commit()
}

// regrant keeps a turn asked for (gen_grants) asking for its text as
// observed now: an edit of the turn after it was asked replaces what is to
// be made, so the grant goes when the edited text is made (it kept the
// earlier text's key and was never dropped).
func regrant(tx *sql.Tx, s *lifecycle.Session) error {
	asked, err := grantsToMove(tx, s.Key)
	if err != nil || len(asked) == 0 {
		return err
	}
	return moveGrants(tx, s, asked)
}

// grantsToMove reads a chat's grants, locked until the save commits: a run
// dropping a grant of the earlier text waits, then finds the grant asking
// for the edited one (read unlocked, the drop came between this read and
// the update).
func grantsToMove(tx *sql.Tx, session string) (map[int]string, error) {
	rows, err := tx.Query(`SELECT turn_id, COALESCE(source_key,'') FROM gen_grants WHERE session_key=? FOR UPDATE`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	asked := map[int]string{}
	for rows.Next() {
		var turn int
		var key string
		if err := rows.Scan(&turn, &key); err != nil {
			return nil, err
		}
		asked[turn] = key
	}
	return asked, rows.Err()
}

// moveGrants points each grant read at its turn's text now.
func moveGrants(tx *sql.Tx, s *lifecycle.Session, asked map[int]string) error {
	for _, t := range s.ActiveTurns() {
		key, ok := asked[t.ID]
		if !ok {
			continue
		}
		if now := memory.SourceOf(s.Key, t).Key(); now != key {
			if _, err := tx.Exec(`UPDATE gen_grants SET source_key=? WHERE session_key=? AND turn_id=?`, now, s.Key, t.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadSession reads a stored session; ok is false when none is stored.
func (d *DB) LoadSession(key string) (*lifecycle.Session, bool, error) {
	// one read-only transaction: every table is read at the same committed
	// state, also when the observation path saves the chat meanwhile
	tx, err := d.sql.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	turns := []*lifecycle.Turn{}
	byID := map[int]*lifecycle.Turn{}
	err = eachOn(tx, `SELECT turn_id, greeting, confirmed, inactive, off, dates FROM logical_turns WHERE session_key=? ORDER BY greeting DESC, turn_id`,
		[]any{key}, func(rows *sql.Rows) error {
			t := &lifecycle.Turn{}
			var dates sql.NullString
			if err := rows.Scan(&t.ID, &t.Greeting, &t.Confirmed, &t.Inactive, &t.Off, &dates); err != nil {
				return err
			}
			if dates.Valid {
				json.Unmarshal([]byte(dates.String), &t.Dates)
			}
			turns = append(turns, t)
			byID[t.ID] = t
			return nil
		})
	if err != nil {
		return nil, false, err
	}
	var past []lifecycle.PastTurn
	err = eachOn(tx, `SELECT from_chat, source, COALESCE(own_source,'') FROM session_past WHERE session_key=? ORDER BY pos`, []any{key}, func(rows *sql.Rows) error {
		var p lifecycle.PastTurn
		if err := rows.Scan(&p.From, &p.Source, &p.Own); err != nil {
			return err
		}
		past = append(past, p)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if len(turns) == 0 {
		if len(past) == 0 {
			return nil, false, nil
		}
		// attached before the host showed any turn of this chat
		s := lifecycle.NewSession(key)
		s.Past = past
		return s, true, nil
	}
	err = eachOn(tx, `SELECT turn_id, member_key, data, state, cut FROM turn_members WHERE session_key=? ORDER BY turn_id, ord`,
		[]any{key}, func(rows *sql.Rows) error {
			var id int
			m := &lifecycle.Member{}
			var st string
			if err := rows.Scan(&id, &m.Key, &m.Data, &st, &m.Cut); err != nil {
				return err
			}
			m.State = lifecycle.RowState(st)
			byID[id].Members = append(byID[id].Members, m)
			return nil
		})
	if err != nil {
		return nil, false, err
	}
	rowByKey := map[string]*lifecycle.ResponseRow{}
	err = eachOn(tx, `SELECT turn_id, row_key, state, generation_id, awaiting_replacement, off, cut FROM response_rows WHERE session_key=? ORDER BY turn_id, ord`,
		[]any{key}, func(rows *sql.Rows) error {
			var id int
			r := &lifecycle.ResponseRow{}
			var st string
			if err := rows.Scan(&id, &r.Key, &st, &r.Gen, &r.AwaitingReplacement, &r.Off, &r.Cut); err != nil {
				return err
			}
			r.State = lifecycle.RowState(st)
			byID[id].Responses = append(byID[id].Responses, r)
			rowByKey[r.Key] = r
			return nil
		})
	if err != nil {
		return nil, false, err
	}
	err = eachOn(tx, `SELECT row_key, seq, data, generation_id, source, confirmed FROM revisions WHERE session_key=? ORDER BY row_key, seq`,
		[]any{key}, func(rows *sql.Rows) error {
			var k, src string
			var rev lifecycle.Revision
			if err := rows.Scan(&k, &rev.Seq, &rev.Data, &rev.GenerationID, &src, &rev.Confirmed); err != nil {
				return err
			}
			rev.Source = lifecycle.Source(src)
			rowByKey[k].Revisions = append(rowByKey[k].Revisions, rev)
			return nil
		})
	if err != nil {
		return nil, false, err
	}
	progress := map[string]int64{}
	err = eachOn(tx, `SELECT boot_id, seq FROM session_progress WHERE session_key=?`, []any{key}, func(rows *sql.Rows) error {
		var b string
		var q int64
		if err := rows.Scan(&b, &q); err != nil {
			return err
		}
		progress[b] = q
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	restored := lifecycle.Restore(key, turns, progress)
	err = eachOn(tx, `SELECT parent_chat, parent_message, markers, copies_chat, copies_message FROM session_branch WHERE session_key=?`, []any{key}, func(rows *sql.Rows) error {
		var markers, cc, cm sql.NullString
		if err := rows.Scan(&restored.BranchOf[0], &restored.BranchOf[1], &markers, &cc, &cm); err != nil {
			return err
		}
		if markers.Valid {
			json.Unmarshal([]byte(markers.String), &restored.Markers)
		}
		restored.CopiesOf = [2]string{cc.String, cm.String}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	err = eachOn(tx, `SELECT turn_id, own_key, source FROM branch_copies WHERE session_key=?`, []any{key}, func(rows *sql.Rows) error {
		var id int
		var c lifecycle.Copy
		if err := rows.Scan(&id, &c.Own, &c.Source); err != nil {
			return err
		}
		if restored.Copies == nil {
			restored.Copies = map[int]lifecycle.Copy{}
		}
		restored.Copies[id] = c
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	restored.Past = past
	return restored, true, nil
}

// ScopeMemoryCount counts the memories a read scope reads (the turns not
// turned off), as CurrentKnown reads them.
func (d *DB) ScopeMemoryCount(scope []memory.ScopedTurn) (int, error) {
	k, _, err := d.knownScope(memory.On(scope), false)
	return len(k.Memories), err
}

// SetPast replaces a chat's attached past (C22, C25) with past, in story
// order (each turn names the stored chat it came from); an empty past
// detaches. It is not a host observation and changes no turn, memory or job.
func (d *DB) SetPast(key string, past []lifecycle.PastTurn) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM session_past WHERE session_key=?`, key); err != nil {
		return err
	}
	for i, p := range past {
		// the chats whose memory the row reads: a deletion or move of one
		// of them finds the rows (also a past attached through another chat)
		var src, ownSrc memory.Source
		json.Unmarshal([]byte(p.Source), &src)
		var own, ownChat any
		if p.Own != "" {
			json.Unmarshal([]byte(p.Own), &ownSrc)
			own, ownChat = p.Own, ownSrc.Session
		}
		if _, err := tx.Exec(`INSERT INTO session_past (session_key, pos, from_chat, source, own_source, source_chat, own_chat) VALUES (?,?,?,?,?,?,?)`, key, i, p.From, p.Source, own, src.Session, ownChat); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) each(q string, args []any, fn func(*sql.Rows) error) error {
	return eachOn(d.sql, q, args, fn)
}

type querier interface {
	Query(q string, args ...any) (*sql.Rows, error)
}

func eachOn(db querier, q string, args []any, fn func(*sql.Rows) error) error {
	rows, err := db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func datesJSON(d map[string]string) any {
	if len(d) == 0 {
		return nil
	}
	b, _ := json.Marshal(d)
	return string(b)
}

func markersJSON(m [][2]string) any {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return string(b)
}
