package store

import (
	"context"
	"fmt"
	"regexp"
)

// ForgetVectors invalidates only the current model's selected document keys.
// Vectors are a shared text cache; other models are retained.
func (d *DB) ForgetVectors(model string, hashes []string) error {
	dim, err := d.embeddingDimension(model)
	if err != nil || dim == 0 {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, h := range hashes {
		if _, err := tx.Exec(fmt.Sprintf("DELETE FROM embedding_vectors_%d WHERE model=? AND text_hash=?", dim), model, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResetData deletes application data in one transaction. Settings and the
// schema remain; access tokens and the sealing key are files, never touched.
func (d *DB) ResetData(ctx context.Context) error {
	tables := []string{"raw_observations", "logical_turns", "turn_members", "response_rows", "revisions", "session_branch", "branch_copies", "session_past", "session_progress", "lifecycle_events",
		"name_merges", "name_ref_changes", "gen_jobs", "names", "name_relations", "memories", "memory_was", "effects", "mentions", "awareness", "scenes", "scene_marks", "links", "embedding_models", "gen_grants", "gen_items", "search_words", "search_refs", "chat_labels", "delivery_traces", "body_events", "body_settings"}
	// Dimension tables are created on first use, rather than in the schema.
	rows, err := d.sql.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE()`)
	if err != nil {
		return err
	}
	vectorTable := regexp.MustCompile(`^embedding_vectors_[0-9]+$`)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if vectorTable.MatchString(name) {
			tables = append(tables, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range tables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM `"+table+"`"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
