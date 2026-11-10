package store

import (
	"database/sql"
	"encoding/json"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// detailTurn projects one local logical turn, then uses the same Own and
// turnJobs rules as the list/delivery readers. No parent session is loaded:
// branch_copies is the source the child actually recorded, even after rerolls.
func (d *DB) detailTurn(chat string, id int) (memory.ScopedTurn, int, error) {
	t := &lifecycle.Turn{ID: id}
	var dates sql.NullString
	err := d.sql.QueryRow(`SELECT greeting, confirmed, inactive, off, dates FROM logical_turns WHERE session_key=? AND turn_id=?`, chat, id).
		Scan(&t.Greeting, &t.Confirmed, &t.Inactive, &t.Off, &dates)
	if err == sql.ErrNoRows {
		return memory.ScopedTurn{}, 0, ErrMemoryNotFound
	}
	if err != nil {
		return memory.ScopedTurn{}, 0, err
	}
	if t.Greeting || t.Inactive {
		return memory.ScopedTurn{}, 0, ErrMemoryNotFound
	}
	if dates.Valid {
		if err = json.Unmarshal([]byte(dates.String), &t.Dates); err != nil {
			return memory.ScopedTurn{}, 0, err
		}
	}
	err = d.each(`SELECT member_key, data, state FROM turn_members WHERE session_key=? AND turn_id=? ORDER BY ord`, []any{chat, id}, func(rows *sql.Rows) error {
		m := &lifecycle.Member{}
		if err := rows.Scan(&m.Key, &m.Data, &m.State); err != nil {
			return err
		}
		t.Members = append(t.Members, m)
		return nil
	})
	if err != nil {
		return memory.ScopedTurn{}, 0, err
	}
	// Only the newest revision is needed by SourceOf/WholeSourceOf. Older
	// revisions and other turns' bodies never cross this read boundary.
	err = d.each(`SELECT r.row_key, r.state, r.generation_id, r.off, v.seq, v.data, v.generation_id
		FROM response_rows r LEFT JOIN revisions v ON v.session_key=r.session_key AND v.row_key=r.row_key
		AND v.seq=(SELECT MAX(x.seq) FROM revisions x WHERE x.session_key=r.session_key AND x.row_key=r.row_key)
		WHERE r.session_key=? AND r.turn_id=? ORDER BY r.ord`, []any{chat, id}, func(rows *sql.Rows) error {
		r := &lifecycle.ResponseRow{}
		var seq sql.NullInt64
		var data, gen sql.NullString
		if err := rows.Scan(&r.Key, &r.State, &r.Gen, &r.Off, &seq, &data, &gen); err != nil {
			return err
		}
		if seq.Valid {
			r.Revisions = []lifecycle.Revision{{Seq: int(seq.Int64), Data: data.String, GenerationID: gen.String}}
		}
		t.Responses = append(t.Responses, r)
		return nil
	})
	if err != nil {
		return memory.ScopedTurn{}, 0, err
	}
	s := lifecycle.Restore(chat, []*lifecycle.Turn{t}, nil)
	err = d.sql.QueryRow(`SELECT parent_chat, parent_message, COALESCE(copies_chat,''), COALESCE(copies_message,'') FROM session_branch WHERE session_key=?`, chat).
		Scan(&s.BranchOf[0], &s.BranchOf[1], &s.CopiesOf[0], &s.CopiesOf[1])
	if err != nil && err != sql.ErrNoRows {
		return memory.ScopedTurn{}, 0, err
	}
	var c lifecycle.Copy
	err = d.sql.QueryRow(`SELECT own_key, source FROM branch_copies WHERE session_key=? AND turn_id=?`, chat, id).Scan(&c.Own, &c.Source)
	if err != nil && err != sql.ErrNoRows {
		return memory.ScopedTurn{}, 0, err
	}
	if err == nil {
		s.Copies = map[int]lifecycle.Copy{id: c}
	}
	var place int
	if err = d.sql.QueryRow(`SELECT COUNT(*) FROM logical_turns WHERE session_key=? AND NOT greeting AND NOT inactive AND turn_id<=?`, chat, id).Scan(&place); err != nil {
		return memory.ScopedTurn{}, 0, err
	}
	return memory.Own(s)[0], place, nil
}

type detailScope struct {
	db   *DB
	chat string
	jobs map[int64]*DetailLocator
}

// locate checks a job against the chat's existing read scope, reading bodies
// only for the candidate local turn. The cache holds small locators, not text.
func (s *detailScope) locate(job int64) (*DetailLocator, error) {
	if loc, ok := s.jobs[job]; ok {
		return loc, nil
	}
	var session, key string
	var turn int
	err := s.db.sql.QueryRow(`SELECT session_key, turn_id, source_key FROM gen_jobs WHERE job_id=?`, job).Scan(&session, &turn, &key)
	if err == sql.ErrNoRows {
		s.jobs[job] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var locals []int
	if session == s.chat {
		locals = append(locals, turn)
	}
	err = s.db.each(`SELECT turn_id FROM branch_copies WHERE session_key=? AND JSON_UNQUOTE(JSON_EXTRACT(source,'$.session'))=? AND JSON_EXTRACT(source,'$.turn')=? ORDER BY turn_id`, []any{s.chat, session, turn}, func(rows *sql.Rows) error {
		var id int
		if err := rows.Scan(&id); err != nil {
			return err
		}
		locals = append(locals, id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, id := range locals {
		t, place, err := s.db.detailTurn(s.chat, id)
		if err == ErrMemoryNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		if t.Off {
			continue
		}
		jobs, _, err := s.db.turnJobs(t)
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if j == job {
				loc := &DetailLocator{TurnID: id, Turn: place, SourceSession: session, SourceTurn: turn, SourceKey: key, Job: job, Inherited: session != s.chat}
				s.jobs[job] = loc
				return loc, nil
			}
		}
	}
	s.jobs[job] = nil
	return nil, nil
}
