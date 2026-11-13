package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// Reference paths address typed scalar slots, never prose. Effect positions are
// their immutable k; list positions are rebased whenever that list is edited.
func memoryReferenceSlots(m *memory.Memory, visit func(string, *string)) {
	for _, v := range []struct {
		path string
		p    *string
	}{
		{"ref", &m.Ref}, {"at", &m.At}, {"scene", &m.Scene}, {"about", &m.About}, {"owner", &m.Owner},
	} {
		visit(v.path, v.p)
	}
	list := func(path string, xs []string) {
		for i := range xs {
			visit(fmt.Sprintf("%s.%d", path, i), &xs[i])
		}
	}
	list("to", m.To)
	list("was", m.Was)
	list("seen_by", m.SeenBy)
	if r := m.Roles; r != nil {
		list("roles.actor", r.Actor)
		list("roles.target", r.Target)
		list("roles.object", r.Object)
		list("roles.place", r.Place)
		list("roles.speaker", r.Speaker)
		list("roles.listener", r.Listener)
	}
	for i := range m.Effects {
		e := &m.Effects[i]
		prefix := fmt.Sprintf("effects.%d", i+1)
		visit(prefix+".about", &e.About)
		list(prefix+".was", e.Was)
	}
	if b := m.Basis; b != nil {
		// several sources ("n1,n2") are one slot each; one keeps its path
		if parts := strings.Split(b.From, ","); len(parts) > 1 {
			for i, p := range parts {
				t := strings.TrimSpace(p)
				if t == "" {
					continue
				}
				v := t
				visit(fmt.Sprintf("basis.from.%d", i), &v)
				parts[i] = strings.Replace(p, t, v, 1)
			}
			b.From = strings.Join(parts, ",")
		} else {
			visit("basis.from", &b.From)
		}
		visit("basis.ref", &b.Ref)
	}
	list("unaware", m.Unaware)
	list("hide_from", m.HideFrom)
	list("allow_to", m.AllowTo)
	for i := range m.Knows {
		k := &m.Knows[i]
		prefix := fmt.Sprintf("knows.%d", i)
		visit(prefix+".who", &k.Who)
		visit(prefix+".from", &k.From)
		visit(prefix+".ref", &k.Ref)
	}
}

type refOrigin struct{ origin, after string }

func referenceOrigins(tx *sql.Tx, table string, row int64) (map[string]refOrigin, error) {
	out := map[string]refOrigin{}
	rows, err := tx.Query(`SELECT c.path,c.origin_ref,c.after_ref FROM name_ref_changes c WHERE c.row_table=? AND c.row_id=? AND NOT EXISTS (SELECT 1 FROM name_ref_changes n WHERE n.row_table=c.row_table AND n.row_id=c.row_id AND n.path=c.path AND n.id>c.id)`, table, row)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var v refOrigin
		if err := rows.Scan(&path, &v.origin, &v.after); err != nil {
			return nil, err
		}
		out[path] = v
	}
	return out, rows.Err()
}

func recordNameRef(tx *sql.Tx, op int64, session string, job int64, table string, row int64, path, origin, before, after string) error {
	_, err := tx.Exec(`INSERT INTO name_ref_changes (merge_id,session_key,job_id,row_table,row_id,path,origin_ref,before_ref,after_ref) VALUES (?,?,?,?,?,?,?,?,?)`, op, session, job, table, row, path, origin, before, after)
	return err
}

// The actual user relations are authoritative, including old rename aliases.
// A revoked merge deletes only its own relation, so existing readers need no
// second state predicate and model disguise relations never enter this map.
func userNameResolver(tx *sql.Tx) (*resolver, error) {
	aliases := map[string]string{}
	rows, err := tx.Query(`SELECT r.name_ref,COALESCE((SELECT c.origin_ref FROM name_ref_changes c WHERE c.row_table='name_relations' AND c.row_id=r.id AND c.path='same_as' ORDER BY c.id DESC LIMIT 1),r.same_as) FROM name_relations r WHERE r.how='user_alias' ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return nil, err
		}
		aliases[a] = b
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	r := &resolver{ids: map[string]string{}}
	for a, b := range aliases {
		for i := 0; i < len(aliases); i++ {
			next, ok := aliases[b]
			if !ok {
				break
			}
			b = next
		}
		r.ids[a] = b
	}
	return r, nil
}

func referenceValue(tx *sql.Tx, r *resolver, origins map[string]refOrigin, op int64, session string, job int64, table string, row int64, path, current string, reset bool) (string, error) {
	origin := current
	if old, ok := origins[path]; ok && !reset {
		origin = old.origin
	}
	after := r.ref(origin)
	if after != current || reset {
		if err := recordNameRef(tx, op, session, job, table, row, path, origin, current, after); err != nil {
			return "", err
		}
	}
	return after, nil
}

func memoryNamesOn(tx *sql.Tx, ms ...memory.Memory) (map[string]string, error) {
	out := map[string]string{}
	seen := map[string]bool{}
	for _, m := range ms {
		for _, ref := range memory.NameRefs(m) {
			if strings.HasPrefix(ref, "N") {
				seen[ref] = true
			}
		}
	}
	for ref := range seen {
		var name string
		err := tx.QueryRow(`SELECT name FROM names WHERE id=?`, strings.TrimPrefix(ref, "N")).Scan(&name)
		if err == nil {
			out[ref] = name
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	return out, nil
}

// writeMemoryMirrors is shared by generation-compatible edits and name replay.
// Awareness and semantic links have independent provenance and are left alone.
func writeMemoryMirrors(tx *sql.Tx, session string, job, id int64, m memory.Memory) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE memories SET body=?,imp=?,about=?,slot=?,mode=?,status=?,owner=?,aspect=?,scene=?,detail=? WHERE id=?`, m.Body, m.Imp, m.About, m.Slot, m.Mode, m.Status, m.Owner, m.Aspect, m.Scene, string(b), id); err != nil {
		return err
	}
	for _, table := range []string{"effects", "memory_was"} {
		if _, err = tx.Exec(`DELETE FROM `+table+` WHERE memory_id=?`, id); err != nil {
			return err
		}
	}
	for i, w := range m.Was {
		if _, err = tx.Exec(`INSERT INTO memory_was (memory_id,ord,target) VALUES (?,?,?)`, id, i, w); err != nil {
			return err
		}
	}
	for i, e := range m.Effects {
		if _, err = tx.Exec(`INSERT INTO effects (memory_id,k,about,slot,before_value,value,delta,was,status) VALUES (?,?,?,?,?,?,?,?,?)`, id, i+1, e.About, e.Slot, e.Before, e.Value, e.Delta, jsonList(e.Was), e.Status); err != nil {
			return err
		}
	}
	item := fmt.Sprintf("M%d", id)
	for _, q := range []string{`DELETE FROM search_words WHERE session_key=? AND job_id=? AND item=? AND field<>'mention'`, `DELETE FROM search_refs WHERE session_key=? AND job_id=? AND item=?`} {
		if _, err = tx.Exec(q, session, job, item); err != nil {
			return err
		}
	}
	return indexMemory(tx, session, job, item, m)
}

// Page by real PK. At most one page of bodies is retained; affected sessions
// and jobs are metadata only. job=0 replays journaled references globally.
func applyNameReferences(tx *sql.Tx, job, op int64, r *resolver) ([]string, error) {
	// Generation already reported unresolved temporary references once.
	probe := *r
	probe.notes = nil
	r = &probe
	changed := map[string]bool{}
	var after int64
	for {
		where := `id>?`
		args := []any{after}
		if job != 0 {
			where += ` AND job_id=?`
			args = append(args, job)
		} else {
			parts := []string{`EXISTS (SELECT 1 FROM name_ref_changes c WHERE c.row_table='memories' AND c.row_id=memories.id)`}
			for from := range r.ids {
				parts = append(parts, "("+mergeMemoryRefs+")")
				args = append(args, from)
			}
			where += " AND (" + strings.Join(parts, " OR ") + ")"
		}
		rows, err := tx.Query(`SELECT id,session_key,job_id,detail FROM memories WHERE `+where+` ORDER BY id LIMIT 100 FOR UPDATE`, args...)
		if err != nil {
			return nil, err
		}
		type row struct {
			id, job      int64
			session, raw string
		}
		page := []row{}
		for rows.Next() {
			var v row
			if err := rows.Scan(&v.id, &v.session, &v.job, &v.raw); err != nil {
				rows.Close()
				return nil, err
			}
			page = append(page, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, v := range page {
			after = v.id
			var before memory.Memory
			if err = json.Unmarshal([]byte(v.raw), &before); err != nil {
				return nil, err
			}
			// JSON round trip gives independently owned nested lists/pointers.
			var m memory.Memory
			json.Unmarshal([]byte(v.raw), &m)
			origins, err := referenceOrigins(tx, "memories", v.id)
			if err != nil {
				return nil, err
			}
			var refErr error
			memoryReferenceSlots(&m, func(path string, p *string) {
				if refErr != nil {
					return
				}
				*p, refErr = referenceValue(tx, r, origins, op, v.session, v.job, "memories", v.id, path, *p, false)
			})
			if refErr != nil {
				return nil, refErr
			}
			b, _ := json.Marshal(before)
			a, _ := json.Marshal(m)
			if string(b) == string(a) {
				continue
			}
			names, err := memoryNamesOn(tx, before, m)
			if err != nil {
				return nil, err
			}
			if memory.DocText(memory.KnownMemory{Memory: before}, names) != memory.DocText(memory.KnownMemory{Memory: m}, names) {
				if err = unlinkVectors(tx, v.job); err != nil {
					return nil, err
				}
				changed[v.session] = true
			}
			if err = writeMemoryMirrors(tx, v.session, v.job, v.id, m); err != nil {
				return nil, err
			}
		}
	}
	mentionJobs := map[int64]string{}
	for _, spec := range []struct {
		table string
		cols  []string
	}{
		{"name_relations", []string{"name_ref", "same_as", "how"}}, {"mentions", []string{"target"}},
		{"awareness", []string{"target", "who", "from_who", "ref"}}, {"links", []string{"from_ref", "to_ref"}},
	} {
		after = 0
		for {
			where := `id>?`
			args := []any{after}
			if job != 0 {
				where += ` AND job_id=?`
				args = append(args, job)
			}
			// A manual relation is the operation's edge, not a materialized use.
			if spec.table == "name_relations" {
				where += ` AND id NOT IN (SELECT relation_id FROM name_merges)`
			}
			rows, err := tx.Query(`SELECT id,session_key,job_id,`+strings.Join(spec.cols, ",")+` FROM `+spec.table+` WHERE `+where+` ORDER BY id LIMIT 100 FOR UPDATE`, args...)
			if err != nil {
				return nil, err
			}
			type row struct {
				id, job int64
				session string
				vals    []string
			}
			page := []row{}
			for rows.Next() {
				v := row{vals: make([]string, len(spec.cols))}
				scan := []any{&v.id, &v.session, &v.job}
				for i := range v.vals {
					scan = append(scan, &v.vals[i])
				}
				if err := rows.Scan(scan...); err != nil {
					rows.Close()
					return nil, err
				}
				page = append(page, v)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			if len(page) == 0 {
				break
			}
			for _, v := range page {
				after = v.id
				origins, err := referenceOrigins(tx, spec.table, v.id)
				if err != nil {
					return nil, err
				}
				for i, col := range spec.cols {
					if col == "how" || (spec.table == "name_relations" && col == "name_ref" && v.vals[2] == "user_alias") {
						continue
					}
					next, err := referenceValue(tx, r, origins, op, v.session, v.job, spec.table, v.id, col, v.vals[i], false)
					if err != nil {
						return nil, err
					}
					if next == v.vals[i] {
						continue
					}
					if _, err = tx.Exec(`UPDATE `+spec.table+` SET `+col+`=? WHERE id=?`, next, v.id); err != nil {
						return nil, err
					}
					if spec.table == "mentions" {
						mentionJobs[v.job] = v.session
					}
				}
			}
		}
	}
	// Mention word rows have no PK; rebuild their derived index from its owner.
	for j, session := range mentionJobs {
		if _, err := tx.Exec(`DELETE FROM search_words WHERE job_id=? AND field='mention'`, j); err != nil {
			return nil, err
		}
		var cursor int64
		for {
			rows, err := tx.Query(`SELECT id,target,q FROM mentions WHERE job_id=? AND id>? ORDER BY id LIMIT 100`, j, cursor)
			if err != nil {
				return nil, err
			}
			type mentionRow struct {
				id        int64
				target, q string
			}
			page := []mentionRow{}
			for rows.Next() {
				var v mentionRow
				if err := rows.Scan(&v.id, &v.target, &v.q); err != nil {
					rows.Close()
					return nil, err
				}
				page = append(page, v)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			if len(page) == 0 {
				break
			}
			for _, v := range page {
				cursor = v.id
				if err := indexMention(tx, session, j, v.target, v.q); err != nil {
					return nil, err
				}
			}
		}
	}
	out := []string{}
	for s := range changed {
		out = append(out, s)
	}
	return out, nil
}
