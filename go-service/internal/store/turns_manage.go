package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"archivecenter2/goservice/internal/memory"
)

// RemakeGeneration retires the source's job and grants the same turn a new
// job. Source text and revision identity stay intact. The retired job and
// its rows remain history, outside CurrentJobs; the new job uses the usual
// generation, received-answer recovery and retry path.
func (d *DB) RemakeGeneration(src memory.Source) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old int64
	err = tx.QueryRow(`SELECT job_id FROM gen_jobs WHERE session_key=? AND turn_id=? AND source_key=? FOR UPDATE`, src.Session, src.Turn, src.Key()).Scan(&old)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if _, err = tx.Exec(`UPDATE gen_jobs SET source_key=CONCAT('remade:',job_id) WHERE job_id=?`, old); err != nil {
			return err
		}
	}
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO gen_jobs (session_key,turn_id,source_key,source,state,created_at) VALUES (?,?,?,?,'requested',?)`, src.Session, src.Turn, src.Key(), string(b), time.Now().UTC()); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO gen_grants (session_key,turn_id,source_key,created_at) VALUES (?,?,?,?)
		ON DUPLICATE KEY UPDATE source_key=VALUES(source_key), created_at=VALUES(created_at)`, src.Session, src.Turn, src.Key(), time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// SourceJobState reads without creating a job (unlike JobFor).
func (d *DB) SourceJobState(src memory.Source) (string, error) {
	var state string
	err := d.sql.QueryRow(`SELECT state FROM gen_jobs WHERE session_key=? AND turn_id=? AND source_key=?`, src.Session, src.Turn, src.Key()).Scan(&state)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return state, err
}
