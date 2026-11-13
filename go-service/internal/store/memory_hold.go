package store

import (
	"database/sql"
	"errors"
	"strconv"
)

// C18 pin and suppress: a
// flag on a memory row. An edit keeps it (the row stays); a remade turn
// writes new rows, so its flags are gone; a copy carries the column.
//   - pin: delivered first, without Select, ranking or scene contrast, within
//     its own lane's ceiling (secret or general); a value a later record
//     replaced, or a memory of the turns the request holds, is not delivered
//   - mute: never delivered
const (
	HoldPin  = "pin"
	HoldMute = "mute"
)

// ErrBadHold is a hold other than pin, mute or "" (none).
var ErrBadHold = errors.New("bad_hold")

// holdsOf reads the flags of the jobs' memories: id (M<n>) -> pin | mute.
func (d *DB) holdsOf(jobs []int64) (map[string]string, error) {
	out := map[string]string{}
	if len(jobs) == 0 {
		return out, nil
	}
	in, args := inJobs(jobs)
	err := d.each(`SELECT id, hold FROM memories WHERE hold<>'' AND job_id IN `+in, args, func(rows *sql.Rows) error {
		var n int64
		var h string
		if err := rows.Scan(&n, &h); err != nil {
			return err
		}
		out["M"+strconv.FormatInt(n, 10)] = h
		return nil
	})
	return out, err
}

// SetMemoryHold sets a memory's flag ("" clears it); the chat's own memories
// only (ownMemory). It reports whether the flag changed.
func (d *DB) SetMemoryHold(chatKey, id, hold string) (bool, error) {
	if hold != "" && hold != HoldPin && hold != HoldMute {
		return false, ErrBadHold
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	n, _, _, err := ownMemory(tx, chatKey, id)
	if err != nil {
		return false, err
	}
	res, err := tx.Exec(`UPDATE memories SET hold=? WHERE id=? AND hold<>?`, hold, n, hold)
	if err != nil {
		return false, err
	}
	changed, _ := res.RowsAffected()
	return changed > 0, tx.Commit()
}
