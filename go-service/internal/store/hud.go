package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"archivecenter2/goservice/internal/memory"
)

// HUDSettings shares the existing backend settings table. It never changes
// generation, capture or delivery settings.
type HUDSettings struct {
	Enabled bool `json:"enabled"`
	Compact bool `json:"compact"`
}

func (d *DB) HUDSettings() (HUDSettings, error) {
	v := HUDSettings{Enabled: true, Compact: true}
	var raw string
	err := d.sql.QueryRow(`SELECT value FROM settings WHERE name='hud'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}

func (d *DB) SetHUDSettings(v HUDSettings) error {
	b, _ := json.Marshal(v)
	_, err := d.sql.Exec(`INSERT INTO settings (name,value) VALUES ('hud',?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, string(b))
	return err
}

// HUDJob reads only the current source's scalar diagnostics, never the model
// request, response or keys. No job is created by a HUD read.
type HUDJob struct {
	State                     string
	Attempts, Made            int
	NextAt, CreatedAt, DoneAt *time.Time
	Reason                    string
}

func (d *DB) HUDSource(t memory.ScopedTurn) (memory.Source, error) {
	_, src, err := d.turnJobs(t) // same inherited/own owner as delivery
	return src, err
}

func (d *DB) HUDJob(src memory.Source) (HUDJob, error) {
	var j HUDJob
	var next, created, done sql.NullTime
	var notes sql.NullString
	err := d.sql.QueryRow(`SELECT state, attempts, next_at, created_at, done_at, notes,
		(SELECT COUNT(*) FROM memories m WHERE m.job_id=j.job_id)
		FROM gen_jobs j WHERE session_key=? AND turn_id=? AND source_key=?`, src.Session, src.Turn, src.Key()).Scan(&j.State, &j.Attempts, &next, &created, &done, &notes, &j.Made)
	if errors.Is(err, sql.ErrNoRows) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if next.Valid {
		j.NextAt = &next.Time
	}
	if created.Valid {
		j.CreatedAt = &created.Time
	}
	if done.Valid {
		j.DoneAt = &done.Time
	}
	if j.State == "failed" || j.State == "unreadable" {
		var list []string
		if json.Unmarshal([]byte(notes.String), &list) == nil && len(list) > 0 {
			j.Reason = list[0]
		}
	}
	return j, nil
}

// RearmHUDTurn is the turn-scoped form of RearmFailedJobs. The worker invokes
// it between runs, so it cannot reset the attempt of an in-flight call.
func (d *DB) RearmHUDTurn(src memory.Source) error {
	_, err := d.sql.Exec(`UPDATE gen_jobs SET attempts=0, next_at=NULL WHERE session_key=? AND turn_id=? AND source_key=? AND state IN ('requested','failed','unreadable') AND attempts>0`, src.Session, src.Turn, src.Key())
	return err
}

type HUDNotice struct {
	ID     int64  `json:"id"`
	Turn   int    `json:"turn"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type HUDJobSource struct {
	Turn   int
	Source string
}

func (d *DB) HUDJobSources(jobs []int64) ([]HUDJobSource, error) {
	out := []HUDJobSource{}
	in, args := inJobs(jobs)
	err := d.each(`SELECT turn_id,source_key FROM gen_jobs WHERE job_id IN `+in, args, func(rows *sql.Rows) error {
		var s HUDJobSource
		if err := rows.Scan(&s.Turn, &s.Source); err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

// HUDVectors checks scalar/vector-cache evidence after a backend restart,
// without reading memory bodies or treating a saved job as an indexed job.
func (d *DB) HUDVectors(src memory.Source, model string) (ready, total int, err error) {
	hashes := []string{}
	err = d.each(`SELECT m.doc_hash FROM memories m JOIN gen_jobs j ON j.job_id=m.job_id WHERE j.session_key=? AND j.turn_id=? AND j.source_key=?`, []any{src.Session, src.Turn, src.Key()}, func(rows *sql.Rows) error {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return err
		}
		hashes = append(hashes, hash)
		return nil
	})
	if err != nil {
		return
	}
	total = len(hashes)
	have, err := d.HaveEmbeddings(model, hashes)
	for _, hash := range hashes {
		if hash != "" && have[hash] {
			ready++
		}
	}
	return ready, total, err
}

func (d *DB) HUDNotices(session string) ([]HUDNotice, error) {
	out := []HUDNotice{}
	err := d.each(`SELECT id,turn_id,kind,detail FROM lifecycle_events WHERE session_key=? AND kind IN ('response_deleted','member_deleted','turn_deleted','response_replaced','response_new_generation','member_edited','response_revised','turn_off','reply_off') ORDER BY id DESC LIMIT 20`, []any{session}, func(rows *sql.Rows) error {
		var n HUDNotice
		if err := rows.Scan(&n.ID, &n.Turn, &n.Kind, &n.Detail); err != nil {
			return err
		}
		out = append(out, n)
		return nil
	})
	return out, err
}

// HUDFailedJob is a chat's generation that failed and is not done: the HUD
// tells it even when its turn is no longer among the latest two. All are
// read: the HUD filters them against the session, so a limit here could
// leave out one it would show. Notes give the first reason only.
type HUDFailedJob struct {
	Turn     int
	Source   string
	Attempts int
	NextAt   *time.Time
	Reason   string
}

func (d *DB) HUDFailedJobs(session string) ([]HUDFailedJob, error) {
	out := []HUDFailedJob{}
	err := d.each(`SELECT turn_id, source_key, attempts, next_at, notes FROM gen_jobs WHERE session_key=? AND state IN ('failed','unreadable') ORDER BY turn_id DESC`, []any{session}, func(rows *sql.Rows) error {
		var j HUDFailedJob
		var next sql.NullTime
		var notes sql.NullString
		if err := rows.Scan(&j.Turn, &j.Source, &j.Attempts, &next, &notes); err != nil {
			return err
		}
		if next.Valid {
			j.NextAt = &next.Time
		}
		var list []string
		if json.Unmarshal([]byte(notes.String), &list) == nil && len(list) > 0 {
			j.Reason = list[0]
		}
		out = append(out, j)
		return nil
	})
	return out, err
}
