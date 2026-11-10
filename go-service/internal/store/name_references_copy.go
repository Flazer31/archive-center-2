package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// A copied later job can use an alias created in an unpaired earlier turn.
// Carry only connected user-alias metadata and its origins, never that turn's
// memories or model-declared identities. This matches hydrateUserAliases.
func copyNameDependencies(tx *sql.Tx, destination string, jobs map[int64]int64, sourceJobs []int64, ids map[string]string, rowIDs map[string]int64, rowsByTable map[string][]chatRow, needed map[string]bool, journal, operations []chatRow) ([]chatRow, []chatRow, error) {
	seenNames, seenEdges, seenOps := map[string]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, op := range operations {
		seenOps[rowInt(op["id"])] = true
	}
	for {
		todo := []string{}
		for name := range needed {
			if strings.HasPrefix(name, "N") && !seenNames[name] {
				todo = append(todo, name)
				seenNames[name] = true
			}
		}
		if len(todo) == 0 {
			break
		}
		for start := 0; start < len(todo); start += 250 {
			part := todo[start:min(start+250, len(todo))]
			in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + ")"
			args := []any{destination}
			for i := 0; i < 2; i++ {
				for _, name := range part {
					args = append(args, name)
				}
			}
			edges, err := chatRows(tx, "name_relations", `how='user_alias' AND session_key<>? AND (name_ref IN `+in+` OR same_as IN `+in+`) ORDER BY id`, args...)
			if err != nil {
				return nil, nil, err
			}
			for _, edge := range edges {
				old := rowInt(edge["id"])
				if seenEdges[old] {
					continue
				}
				seenEdges[old] = true
				needed[rowString(edge["name_ref"])], needed[rowString(edge["same_as"])] = true, true
				ops, err := chatRows(tx, "name_merges", `relation_id=?`, old)
				if err != nil {
					return nil, nil, err
				}
				for _, op := range ops {
					if !seenOps[rowInt(op["id"])] {
						operations = append(operations, op)
						seenOps[rowInt(op["id"])] = true
					}
				}
				if ids[fmt.Sprintf("R%d", old)] != "" {
					continue
				}
				history, err := chatRows(tx, "name_ref_changes", `row_table='name_relations' AND row_id=? ORDER BY id`, old)
				if err != nil {
					return nil, nil, err
				}
				for _, entry := range history {
					for _, col := range []string{"origin_ref", "before_ref", "after_ref"} {
						if v := rowString(entry[col]); strings.HasPrefix(v, "N") {
							needed[v] = true
						}
					}
				}
				if jobs[rowInt(edge["job_id"])] == 0 {
					journal = append(journal, history...)
				}
				edge["job_id"], edge["session_key"] = jobs[sourceJobs[0]], destination
				id, err := insertChatRow(tx, "name_relations", edge, "id")
				if err != nil {
					return nil, nil, err
				}
				ids[fmt.Sprintf("R%d", old)], rowIDs[fmt.Sprintf("name_relations/%d", old)] = fmt.Sprintf("R%d", id), id
				edge["id"] = id
				rowsByTable["name_relations"] = append(rowsByTable["name_relations"], edge)
			}
		}
	}
	return journal, operations, nil
}

// Chat copies issue new operation IDs and real row PKs, just as they issue new
// memory/name IDs. They never let a destination undo a source operation.
func copyNameReferenceRecords(tx *sql.Tx, destination string, jobs map[int64]int64, sourceJobs []int64, rowIDs map[string]int64, r *resolver, journal, operations []chatRow) error {
	ops := map[int64]int64{}
	for _, source := range operations {
		row := chatRow{}
		for k, v := range source {
			row[k] = v
		}
		old := rowInt(row["id"])
		oldRel := rowInt(row["relation_id"])
		row["session_key"] = destination
		job := jobs[rowInt(row["job_id"])]
		if job == 0 {
			job = jobs[sourceJobs[0]]
		}
		row["job_id"] = job
		row["keep_ref"], row["merge_ref"] = r.ref(rowString(row["keep_ref"])), r.ref(rowString(row["merge_ref"]))
		rel := rowIDs[fmt.Sprintf("name_relations/%d", oldRel)]
		if rel == 0 && row["undone_at"] == nil {
			relations, err := chatRows(tx, "name_relations", `id=?`, oldRel)
			if err != nil {
				return err
			}
			if len(relations) > 0 {
				edge := relations[0]
				edge["session_key"], edge["job_id"] = destination, job
				edge["name_ref"], edge["same_as"] = r.ref(rowString(edge["name_ref"])), r.ref(rowString(edge["same_as"]))
				rel, err = insertChatRow(tx, "name_relations", edge, "id")
				if err != nil {
					return err
				}
			}
		}
		row["relation_id"] = rel
		id, err := insertChatRow(tx, "name_merges", row, "id")
		if err != nil {
			return err
		}
		ops[old] = id
	}
	for _, source := range journal {
		table := rowString(source["row_table"])
		row := rowIDs[table+"/"+rowString(source["row_id"])]
		if row == 0 {
			continue
		} // a deleted row is not recreated by copying its history
		job := jobs[rowInt(source["job_id"])]
		if job == 0 {
			job = jobs[sourceJobs[0]]
		}
		if err := recordNameRef(tx, ops[rowInt(source["merge_id"])], destination, job, table, row, rowString(source["path"]), r.ref(rowString(source["origin_ref"])), r.ref(rowString(source["before_ref"])), r.ref(rowString(source["after_ref"]))); err != nil {
			return err
		}
	}
	for old, id := range ops {
		if _, err := tx.Exec(`UPDATE name_ref_changes SET merge_id=? WHERE session_key=? AND merge_id=?`, id, destination, old); err != nil {
			return err
		}
	}
	return nil
}

func remapNameReferenceRecords(tx *sql.Tx, key string, r *resolver) error {
	for _, spec := range []struct {
		table string
		cols  []string
	}{{"name_merges", []string{"keep_ref", "merge_ref"}}, {"name_ref_changes", []string{"origin_ref", "before_ref", "after_ref"}}} {
		var after int64
		for {
			rows, err := chatRows(tx, spec.table, `session_key=? AND id>? ORDER BY id LIMIT 100`, key, after)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				after = rowInt(row["id"])
				sets := []string{}
				args := []any{}
				for _, col := range spec.cols {
					sets = append(sets, col+"=?")
					args = append(args, r.ref(rowString(row[col])))
				}
				args = append(args, after)
				if _, err := tx.Exec(`UPDATE `+spec.table+` SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
