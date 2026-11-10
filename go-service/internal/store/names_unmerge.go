package store

import (
	"database/sql"
	"errors"
	"time"
)

var ErrMergeNoRecord = errors.New("no_record")

type NameMerge struct {
	ID    int64  `json:"merge_id"`
	Keep  string `json:"keep"`
	Merge string `json:"merge"`
}

func (d *DB) NameMerges(chat string) ([]NameMerge, error) {
	out := []NameMerge{}
	err := d.each(`SELECT id,keep_ref,merge_ref FROM name_merges WHERE session_key=? AND undone_at IS NULL ORDER BY id`, []any{chat}, func(rows *sql.Rows) error {
		var v NameMerge
		if err := rows.Scan(&v.ID, &v.Keep, &v.Merge); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	return out, err
}

// UnmergeNames removes this operation's exact edge. Current fields are
// rematerialized from their most recent explicitly supplied reference, with
// all surviving user aliases applied. No old detail/body is restored.
func (d *DB) UnmergeNames(chat string, mergeID int64) ([]string, error) {
	return againOnConflict(func() ([]string, error) { return d.unmergeNames(chat, mergeID) })
}

func (d *DB) unmergeNames(chat string, mergeID int64) ([]string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var relation int64
	var undone sql.NullTime
	err = tx.QueryRow(`SELECT relation_id,undone_at FROM name_merges WHERE id=? AND session_key=? FOR UPDATE`, mergeID, chat).Scan(&relation, &undone)
	if err == sql.ErrNoRows {
		return nil, ErrMergeNoRecord
	}
	if err != nil {
		return nil, err
	}
	if undone.Valid {
		return []string{}, tx.Commit()
	}
	if _, err = tx.Exec(`DELETE FROM name_relations WHERE id=?`, relation); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE name_merges SET undone_at=? WHERE id=?`, time.Now().UTC(), mergeID); err != nil {
		return nil, err
	}
	r, err := userNameResolver(tx)
	if err != nil {
		return nil, err
	}
	sessions, err := applyNameReferences(tx, 0, mergeID, r)
	if err != nil {
		return nil, err
	}
	return sessions, tx.Commit()
}
