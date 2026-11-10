package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// ChatOpResult is also the read-only preview. Counts describe the operation,
// not the amount of available space in the destination.
type ChatOpResult struct {
	CopiedTurns     int      `json:"copied_turns"`
	UnmatchedTurns  int      `json:"unmatched_turns"`
	ExistingTurns   int      `json:"existing_turns"`
	CopiedMemories  int      `json:"copied_memories"`
	DeletedTurns    int      `json:"deleted_turns"`
	DeletedMemories int      `json:"deleted_memories"`
	Branches        []string `json:"branches"`
	// PastReaders: chats that attached the deleted or moved chat as their
	// past (C22); that attachment is removed with it.
	PastReaders []string `json:"past_readers"`
	// Invalidations are used only by the local owners after commit.
	Changed          []string `json:"-"`
	ForgottenSources []string `json:"-"`
}

type chatCopy struct{ from, to memory.Source }

// ChatOperation runs under the pipeline observation lock and the generation
// worker's run lock. All copies, branch materialization and deletion commit
// together. Preview executes only reads (including no auto-increment writes).
func (d *DB) ChatOperation(op, from, to string, dry bool) (ChatOpResult, error) {
	out := ChatOpResult{Branches: []string{}, PastReaders: []string{}}
	if op != "delete" && op != "copy" && op != "move" {
		return out, fmt.Errorf("unknown_chat_operation")
	}
	if from == "" || (op != "delete" && (to == "" || from == to)) {
		return out, fmt.Errorf("invalid_chats")
	}
	s, found, err := d.LoadSession(from)
	if err != nil {
		return out, err
	}
	if !found {
		s = lifecycle.NewSession(from)
	}
	var pairs []chatCopy
	if op != "delete" {
		dest, ok, err := d.LoadSession(to)
		if err != nil {
			return out, err
		}
		var target []memory.ScopedTurn
		if ok {
			target = memory.Own(dest)
		}
		for i, t := range memory.Own(s) {
			if i >= len(target) || !sameChatText(t.OwnSource(), target[i].OwnSource()) {
				out.UnmatchedTurns++
				continue
			}
			// A copied parent turn reads the parent's current memory; an
			// unmade parent can instead have the branch's own generation.
			src, err := d.chatReadSource(t)
			if err != nil {
				return out, err
			}
			p, err := d.chatReadSource(target[i])
			if err != nil {
				return out, err
			}
			var n int
			if err := d.sql.QueryRow(`SELECT COUNT(*) FROM memories m JOIN gen_jobs g ON g.job_id=m.job_id WHERE g.session_key=? AND g.turn_id=? AND g.source_key=?`, p.Session, p.Turn, p.Key()).Scan(&n); err != nil {
				return out, err
			}
			if n > 0 {
				out.ExistingTurns++
				continue
			}
			pairs = append(pairs, chatCopy{src, target[i].OwnSource()})
		}
	}
	// Include descendants whose recorded source points straight to this
	// ancestor, even when their immediate parent is another branch.
	children := map[string][]chatCopy{}
	if op != "copy" {
		keys, err := d.SessionKeys()
		if err != nil {
			return out, err
		}
		for _, key := range keys {
			if key == from {
				continue
			}
			child, ok, err := d.LoadSession(key)
			if err != nil {
				return out, err
			}
			if !ok {
				continue
			}
			for _, t := range memory.Own(child) {
				if t.Inherited && t.Source.Session == from {
					children[key] = append(children[key], chatCopy{t.Source, t.OwnSource()})
				}
			}
			if child.BranchOf[0] == from || len(children[key]) > 0 {
				out.Branches = append(out.Branches, strings.TrimPrefix(key, "chat:"))
			}
		}
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if op != "delete" {
		turns, memories, r, err := copyChatJobs(tx, pairs, dry, false)
		if err != nil {
			return out, err
		}
		if !dry {
			// what the user set in the chat's body screen goes with its memory
			if err := copyBodyOwned(tx, from, to, r, op == "move"); err != nil {
				return out, err
			}
		}
		out.CopiedTurns, out.CopiedMemories = turns, memories
		out.Changed = append(out.Changed, to)
	}
	if op != "copy" {
		// Work in a deterministic order; remap the child's later references
		// too, so its own views/links do not point at the removed ancestor.
		var keys []string
		for key := range children {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, _, _, err := copyChatJobs(tx, children[key], dry, true); err != nil {
				return out, err
			}
			if !dry {
				for _, p := range children[key] {
					if _, err := tx.Exec(`DELETE FROM branch_copies WHERE session_key=? AND turn_id=?`, key, p.to.Turn); err != nil {
						return out, err
					}
				}
			}
			out.Changed = append(out.Changed, key)
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM logical_turns WHERE session_key=? AND NOT greeting`, from).Scan(&out.DeletedTurns); err != nil {
			return out, err
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM memories WHERE session_key=?`, from).Scan(&out.DeletedMemories); err != nil {
			return out, err
		}
		if err := eachOn(tx, `SELECT source_key FROM gen_jobs WHERE session_key=?`, []any{from}, func(r *sql.Rows) error {
			var k string
			if err := r.Scan(&k); err != nil {
				return err
			}
			out.ForgottenSources = append(out.ForgottenSources, k)
			return nil
		}); err != nil {
			return out, err
		}
		// A chat whose attached past reads this chat's memory (attached from
		// it, or through another chat's past): that memory goes with it, so
		// those past turns go too (the reader's own turns and the past it
		// reads from other chats stay).
		if err := eachOn(tx, `SELECT DISTINCT session_key FROM session_past WHERE (source_chat=? OR own_chat=?) AND session_key<>? ORDER BY session_key`, []any{from, from, from}, func(r *sql.Rows) error {
			var k string
			if err := r.Scan(&k); err != nil {
				return err
			}
			out.PastReaders = append(out.PastReaders, strings.TrimPrefix(k, "chat:"))
			out.Changed = append(out.Changed, k)
			return nil
		}); err != nil {
			return out, err
		}
		if !dry {
			if _, err := tx.Exec(`DELETE FROM session_past WHERE source_chat=? AND session_key<>?`, from, from); err != nil {
				return out, err
			}
			// a copy that chat made as its own: the source it copies is still read
			if _, err := tx.Exec(`UPDATE session_past SET own_source=NULL, own_chat=NULL WHERE own_chat=? AND session_key<>?`, from, from); err != nil {
				return out, err
			}
			if err := deleteChatRows(tx, from); err != nil {
				return out, err
			}
		}
		out.Changed = append(out.Changed, from)
	}
	if dry {
		return out, nil
	}
	return out, tx.Commit()
}

// Matching is exactly the user-selected order + input/reply body contract;
// row IDs, timestamps, confirmation and revision numbers do not participate.
func sameChatText(a, b memory.Source) bool {
	if len(a.Members) != len(b.Members) || len(a.Responses) != len(b.Responses) {
		return false
	}
	for i, m := range a.Members {
		if m.Data != b.Members[i].Data {
			return false
		}
	}
	for i, r := range a.Responses {
		if r.Data != b.Responses[i].Data {
			return false
		}
	}
	return true
}

func (d *DB) chatReadSource(t memory.ScopedTurn) (memory.Source, error) {
	ids, err := d.CurrentJobs([]memory.Source{t.Source})
	if err != nil {
		return memory.Source{}, err
	}
	if t.Inherited && len(ids) == 0 {
		return t.OwnSource(), nil
	}
	return t.Source, nil
}

// These are all session-owned tables in schema.go, memory_schema.go,
// search_index.go and chats.go. Child rows without session_key go first.
var chatSessionTables = []string{"name_merges", "name_ref_changes", "search_words", "search_refs", "names", "name_relations", "memories", "mentions", "awareness", "scenes", "scene_marks", "links", "gen_grants", "gen_jobs", "turn_members", "response_rows", "revisions", "logical_turns", "session_branch", "branch_copies", "session_past", "session_progress", "lifecycle_events", "chat_labels", "delivery_traces", "body_events", "body_settings"}

func deleteChatRows(tx *sql.Tx, key string) error {
	var hashes []string
	if err := eachOn(tx, `SELECT DISTINCT doc_hash FROM memories WHERE session_key=? AND doc_hash<>''`, []any{key}, func(r *sql.Rows) error {
		var hash string
		if err := r.Scan(&hash); err != nil {
			return err
		}
		hashes = append(hashes, hash)
		return nil
	}); err != nil {
		return err
	}
	for _, table := range []string{"effects", "memory_was"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE memory_id IN (SELECT id FROM memories WHERE session_key=?)`, key); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM gen_items WHERE job_id IN (SELECT job_id FROM gen_jobs WHERE session_key=?)`, key); err != nil {
		return err
	}
	for _, table := range chatSessionTables {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE session_key=?`, key); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	// chat_id2 alone is another chat's observation, retained byte-for-byte.
	if _, err := tx.Exec(`DELETE FROM raw_observations WHERE chat_id=?`, strings.TrimPrefix(key, "chat:")); err != nil {
		return err
	}
	var dims []int
	if err := eachOn(tx, `SELECT DISTINCT dim FROM embedding_models`, nil, func(r *sql.Rows) error {
		var n int
		if err := r.Scan(&n); err != nil {
			return err
		}
		dims = append(dims, n)
		return nil
	}); err != nil {
		return err
	}
	for _, dim := range dims {
		for _, hash := range hashes {
			if _, err := tx.Exec(fmt.Sprintf(`DELETE v FROM embedding_vectors_%d v WHERE v.text_hash=? AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.doc_hash=v.text_hash)`, dim), hash); err != nil {
				return err
			}
		}
	}
	return nil
}

// A raw row clone keeps job diagnostics, optional fields and exact quotes.
// Only typed reference columns are remapped; prose that happens to contain
// an ID is never rewritten.
type chatRow map[string]any

func chatRows(tx *sql.Tx, table, where string, args ...any) ([]chatRow, error) {
	r, err := tx.Query(`SELECT * FROM `+table+` WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	cols, err := r.Columns()
	if err != nil {
		return nil, err
	}
	var out []chatRow
	for r.Next() {
		v := make([]any, len(cols))
		p := make([]any, len(cols))
		for i := range v {
			p[i] = &v[i]
		}
		if err := r.Scan(p...); err != nil {
			return nil, err
		}
		row := chatRow{}
		for i, c := range cols {
			if b, ok := v[i].([]byte); ok {
				v[i] = string(b)
			}
			row[c] = v[i]
		}
		out = append(out, row)
	}
	return out, r.Err()
}
func rowString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func rowInt(v any) int64 { n, _ := strconv.ParseInt(rowString(v), 10, 64); return n }
func insertChatRow(tx *sql.Tx, table string, row chatRow, omit string) (int64, error) {
	var cols []string
	for k := range row {
		if k != omit {
			cols = append(cols, k)
		}
	}
	sort.Strings(cols)
	args := make([]any, len(cols))
	for i, c := range cols {
		args[i] = row[c]
	}
	r, err := tx.Exec(`INSERT INTO `+table+` (`+strings.Join(cols, ",")+`) VALUES (`+strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("copy %s: %w", table, err)
	}
	return r.LastInsertId()
}

type chatTable struct {
	name, prefix string
	refs         []string
}

var chatItemTables = []chatTable{
	{"names", "N", nil}, {"name_relations", "R", []string{"name_ref", "same_as"}},
	{"memories", "M", []string{"about", "owner", "scene"}}, {"scenes", "S", nil},
	{"links", "L", []string{"from_ref", "to_ref"}},
	{"mentions", "", []string{"target"}}, {"awareness", "", []string{"target", "who", "from_who", "ref"}},
	{"scene_marks", "", []string{"scene"}},
	{"search_words", "", []string{"item"}}, {"search_refs", "", []string{"item", "ref"}},
	{"gen_items", "", []string{"item_id"}},
	{"body_events", "", []string{"who"}},
}

func copyChatJobs(tx *sql.Tx, pairs []chatCopy, dry, materialize bool) (int, int, *resolver, error) {
	jobs := map[int64]int64{}
	ids := map[string]string{}
	effects := map[string]int{}
	var sourceJobs []int64
	turns, mems := 0, 0
	var destination string
	for _, p := range pairs {
		rows, err := chatRows(tx, "gen_jobs", `session_key=? AND turn_id=? AND source_key=?`, p.from.Session, p.from.Turn, p.from.Key())
		if err != nil {
			return 0, 0, nil, err
		}
		if len(rows) == 0 {
			continue
		}
		row := rows[0]
		old := rowInt(row["job_id"])
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM memories WHERE job_id=?`, old).Scan(&n); err != nil {
			return 0, 0, nil, err
		}
		// Explicit copy leaves existing memory alone. Materialization instead
		// preserves the parent's currently visible result over a hidden child
		// fallback made while the parent was still unmade.
		existing, err := chatRows(tx, "gen_jobs", `session_key=? AND turn_id=? AND source_key=?`, p.to.Session, p.to.Turn, p.to.Key())
		if err != nil {
			return 0, 0, nil, err
		}
		if len(existing) > 0 {
			var have int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM memories WHERE job_id=?`, existing[0]["job_id"]).Scan(&have); err != nil {
				return 0, 0, nil, err
			}
			if have > 0 && !materialize {
				continue
			}
		}
		turns++
		mems += n
		if dry {
			continue
		}
		if len(existing) > 0 {
			if err := deleteChatJob(tx, rowInt(existing[0]["job_id"])); err != nil {
				return 0, 0, nil, err
			}
		}
		b, _ := json.Marshal(p.to)
		row["session_key"], row["turn_id"], row["source_key"], row["source"] = p.to.Session, p.to.Turn, p.to.Key(), string(b)
		id, err := insertChatRow(tx, "gen_jobs", row, "job_id")
		if err != nil {
			return 0, 0, nil, err
		}
		jobs[old] = id
		sourceJobs = append(sourceJobs, old)
		destination = p.to.Session
	}
	if dry || len(sourceJobs) == 0 {
		return turns, mems, nil, nil
	}
	in, args := inJobs(sourceJobs)
	// Allocate every ID first, across every copied turn, before rewriting
	// cross-turn references. Same-named source entities stay distinct.
	rowsByTable := map[string][]chatRow{}
	rowIDs := map[string]int64{}
	for _, spec := range chatItemTables {
		order := ""
		if spec.name != "gen_items" && spec.name != "search_words" && spec.name != "search_refs" {
			order = " ORDER BY id"
		}
		rows, err := chatRows(tx, spec.name, `job_id IN `+in+order, args...)
		if err != nil {
			return 0, 0, nil, err
		}
		for _, row := range rows {
			row["job_id"] = jobs[rowInt(row["job_id"])]
			if _, ok := row["session_key"]; ok {
				row["session_key"] = destination
			}
			if spec.prefix != "" {
				oldRow := rowString(row["id"])
				old := spec.prefix + rowString(row["id"])
				id, err := insertChatRow(tx, spec.name, row, "id")
				if err != nil {
					return 0, 0, nil, err
				}
				ids[old] = fmt.Sprintf("%s%d", spec.prefix, id)
				row["id"] = id
				rowIDs[spec.name+"/"+oldRow] = id
			}
		}
		rowsByTable[spec.name] = rows
	}
	// Names referenced from an unpaired source turn still need their own
	// distinct destination IDs. Do not copy that turn's unrelated memories.
	needed := map[string]bool{}
	journal, err := chatRows(tx, "name_ref_changes", `job_id IN `+in+` ORDER BY id`, args...)
	if err != nil {
		return 0, 0, nil, err
	}
	operations, err := chatRows(tx, "name_merges", `job_id IN `+in+` OR id IN (SELECT merge_id FROM name_ref_changes WHERE job_id IN `+in+`) ORDER BY id`, append(append([]any{}, args...), args...)...)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, row := range journal {
		for _, col := range []string{"origin_ref", "before_ref", "after_ref"} {
			if v := rowString(row[col]); strings.HasPrefix(v, "N") {
				needed[v] = true
			}
		}
	}
	for _, row := range operations {
		needed[rowString(row["keep_ref"])], needed[rowString(row["merge_ref"])] = true, true
	}
	for _, spec := range chatItemTables {
		for _, row := range rowsByTable[spec.name] {
			for _, col := range spec.refs {
				v := rowString(row[col])
				if strings.HasPrefix(v, "N") {
					needed[v] = true
				}
			}
			if spec.name == "memories" {
				var m memory.Memory
				if err := json.Unmarshal([]byte(rowString(row["detail"])), &m); err != nil {
					return 0, 0, nil, err
				}
				for _, ref := range memory.MemoryRefs(m) {
					if strings.HasPrefix(ref.Value, "N") {
						needed[ref.Value] = true
					}
				}
			}
			if spec.name == "body_events" {
				for _, ref := range bodyEventRefs(rowString(row["detail"])) {
					if strings.HasPrefix(ref, "N") {
						needed[ref] = true
					}
				}
			}
		}
	}
	journal, operations, err = copyNameDependencies(tx, destination, jobs, sourceJobs, ids, rowIDs, rowsByTable, needed, journal, operations)
	if err != nil {
		return 0, 0, nil, err
	}
	var nameRefs []string
	for ref := range needed {
		nameRefs = append(nameRefs, ref)
	}
	sort.Strings(nameRefs)
	for len(nameRefs) > 0 {
		ref := nameRefs[0]
		nameRefs = nameRefs[1:]
		if ids[ref] != "" {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(ref, "N"), 10, 64)
		if err != nil {
			continue
		}
		rows, err := chatRows(tx, "names", `id=?`, n)
		if err != nil {
			return 0, 0, nil, err
		}
		if len(rows) == 0 {
			continue
		}
		row := rows[0]
		originalJob := row["job_id"]
		row["session_key"], row["job_id"] = destination, jobs[sourceJobs[0]]
		id, err := insertChatRow(tx, "names", row, "id")
		if err != nil {
			return 0, 0, nil, err
		}
		ids[ref] = fmt.Sprintf("N%d", id)
		// A dependency name carries its own disclosure/knowledge rows too.
		// Their people can in turn need distinct name definitions.
		aware, err := chatRows(tx, "awareness", `job_id=? AND target=? ORDER BY id`, originalJob, ref)
		if err != nil {
			return 0, 0, nil, err
		}
		for _, a := range aware {
			a["session_key"], a["job_id"] = destination, jobs[sourceJobs[0]]
			rowsByTable["awareness"] = append(rowsByTable["awareness"], a)
			for _, col := range []string{"who", "from_who", "ref"} {
				v := rowString(a[col])
				if strings.HasPrefix(v, "N") && !needed[v] {
					needed[v] = true
					nameRefs = append(nameRefs, v)
				}
			}
		}
	}
	for _, row := range rowsByTable["memories"] {
		var m memory.Memory
		if err := json.Unmarshal([]byte(rowString(row["detail"])), &m); err != nil {
			return 0, 0, nil, err
		}
		effects[m.ID] = len(m.Effects)
	}
	r := resolver{ids: ids, effects: effects}
	for _, spec := range chatItemTables {
		for _, row := range rowsByTable[spec.name] {
			for _, col := range spec.refs {
				row[col] = r.ref(rowString(row[col]))
			}
			if spec.name == "memories" {
				var m memory.Memory
				json.Unmarshal([]byte(rowString(row["detail"])), &m)
				m = r.memory(m)
				m.ID = "M" + rowString(row["id"])
				b, _ := json.Marshal(m)
				row["detail"] = string(b)
			}
			if spec.name == "body_events" {
				row["detail"] = remapBodyEvent(rowString(row["detail"]), &r)
			}
			if spec.prefix != "" {
				if err := updateChatRow(tx, spec.name, row, "id"); err != nil {
					return 0, 0, nil, err
				}
			} else {
				omit := "id"
				if spec.name == "gen_items" || spec.name == "search_words" || spec.name == "search_refs" {
					omit = ""
				}
				id, err := insertChatRow(tx, spec.name, row, omit)
				if err != nil {
					return 0, 0, nil, err
				}
				if omit == "id" {
					rowIDs[spec.name+"/"+rowString(row["id"])] = id
				}
			}
		}
	}
	for _, table := range []string{"memory_was", "effects"} {
		rows, err := chatRows(tx, table, `memory_id IN (SELECT id FROM memories WHERE job_id IN `+in+`)`, args...)
		if err != nil {
			return 0, 0, nil, err
		}
		for _, row := range rows {
			row["memory_id"] = strings.TrimPrefix(ids["M"+rowString(row["memory_id"])], "M")
			if table == "memory_was" {
				row["target"] = r.ref(rowString(row["target"]))
			} else {
				row["about"] = r.ref(rowString(row["about"]))
				var was []string
				json.Unmarshal([]byte(rowString(row["was"])), &was)
				row["was"] = jsonList(r.list(was))
			}
			if _, err := insertChatRow(tx, table, row, ""); err != nil {
				return 0, 0, nil, err
			}
		}
	}
	if err := copyNameReferenceRecords(tx, destination, jobs, sourceJobs, rowIDs, &r, journal, operations); err != nil {
		return 0, 0, nil, err
	}
	// Existing later child records can refer to the newly owned past.
	if materialize {
		if err := remapChatReferences(tx, destination, &r); err != nil {
			return 0, 0, nil, err
		}
	}
	return turns, mems, &r, nil
}

func updateChatRow(tx *sql.Tx, table string, row chatRow, key string) error {
	var cols []string
	for c := range row {
		if c != key {
			cols = append(cols, c)
		}
	}
	sort.Strings(cols)
	var sets []string
	var args []any
	for _, c := range cols {
		sets = append(sets, c+"=?")
		args = append(args, row[c])
	}
	args = append(args, row[key])
	_, err := tx.Exec(`UPDATE `+table+` SET `+strings.Join(sets, ",")+` WHERE `+key+`=?`, args...)
	return err
}

func deleteChatJob(tx *sql.Tx, job int64) error {
	if _, err := tx.Exec(`DELETE FROM name_ref_changes WHERE job_id=?`, job); err != nil {
		return err
	}
	// Remove only operation edges owned by this discarded job.
	if _, err := tx.Exec(`DELETE FROM name_relations WHERE id IN (SELECT relation_id FROM name_merges WHERE job_id=?)`, job); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM name_merges WHERE job_id=?`, job); err != nil {
		return err
	}
	for _, table := range []string{"effects", "memory_was"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE memory_id IN (SELECT id FROM memories WHERE job_id=?)`, job); err != nil {
			return err
		}
	}
	for _, spec := range chatItemTables {
		if _, err := tx.Exec(`DELETE FROM `+spec.name+` WHERE job_id=?`, job); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`DELETE FROM gen_jobs WHERE job_id=?`, job)
	return err
}

func remapChatReferences(tx *sql.Tx, key string, r *resolver) error {
	if err := remapNameReferenceRecords(tx, key, r); err != nil {
		return err
	}
	for _, spec := range chatItemTables {
		if len(spec.refs) == 0 || spec.name == "gen_items" {
			continue
		}
		rows, err := chatRows(tx, spec.name, `session_key=?`, key)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if spec.name == "search_words" || spec.name == "search_refs" {
				continue
			} // copied index rows were remapped above
			for _, col := range spec.refs {
				row[col] = r.ref(rowString(row[col]))
			}
			if spec.name == "memories" {
				var m memory.Memory
				if err := json.Unmarshal([]byte(rowString(row["detail"])), &m); err != nil {
					return err
				}
				m = r.memory(m)
				b, _ := json.Marshal(m)
				row["detail"] = string(b)
			}
			if spec.name == "body_events" {
				row["detail"] = remapBodyEvent(rowString(row["detail"]), r)
			}
			if err := updateChatRow(tx, spec.name, row, "id"); err != nil {
				return err
			}
		}
	}
	// Rebuild derived rows of existing records from their remapped detail.
	rows, err := chatRows(tx, "memories", `session_key=?`, key)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var m memory.Memory
		json.Unmarshal([]byte(rowString(row["detail"])), &m)
		id := row["id"]
		for _, table := range []string{"effects", "memory_was"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE memory_id=?`, id); err != nil {
				return err
			}
		}
		for i, w := range m.Was {
			if _, err := tx.Exec(`INSERT INTO memory_was (memory_id,ord,target) VALUES (?,?,?)`, id, i, w); err != nil {
				return err
			}
		}
		for i, e := range m.Effects {
			if _, err := tx.Exec(`INSERT INTO effects (memory_id,k,about,slot,before_value,value,delta,was,status) VALUES (?,?,?,?,?,?,?,?,?)`, id, i+1, e.About, e.Slot, e.Before, e.Value, e.Delta, jsonList(e.Was), e.Status); err != nil {
				return err
			}
		}
		for _, table := range []string{"search_words", "search_refs"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE session_key=? AND item=? AND field<>'mention'`, key, m.ID); err != nil {
				return err
			}
		}
		if err := indexMemory(tx, key, rowInt(row["job_id"]), m.ID, m); err != nil {
			return err
		}
	}
	for old, newID := range r.ids {
		for _, q := range []string{`UPDATE search_words SET item=? WHERE session_key=? AND item=?`, `UPDATE search_refs SET ref=? WHERE session_key=? AND ref=?`} {
			if _, err := tx.Exec(q, newID, key, old); err != nil {
				return err
			}
		}
	}
	return nil
}

// bodyEventRefs are the name ids a stored body event's detail uses.
func bodyEventRefs(detail string) []string {
	var e memory.BodyEvent
	json.Unmarshal([]byte(detail), &e)
	refs := append([]string{e.Who}, e.Partners...)
	if e.Paternity != nil {
		refs = append(refs, e.Paternity.Candidates...)
	}
	return refs
}

// remapBodyEvent maps a copied body event's name ids to the copy's.
func remapBodyEvent(detail string, r *resolver) string {
	var e memory.BodyEvent
	if json.Unmarshal([]byte(detail), &e) != nil {
		return detail
	}
	e.Who, e.Partners = r.ref(e.Who), r.list(e.Partners)
	if e.Paternity != nil {
		p := *e.Paternity
		p.Candidates = r.list(p.Candidates)
		e.Paternity = &p
	}
	b, _ := json.Marshal(e)
	return string(b)
}

// copyBodyOwned copies what the user set on the body screen of chat from to
// chat to: the facts recorded there, the model seed (kept when to has one)
// and each person's settings (kept when to has them), with the copy's
// person ids. On a move a fact or setting of a person no copied turn names
// is left out: its person goes with the source.
func copyBodyOwned(tx *sql.Tx, from, to string, r *resolver, move bool) error {
	mapped := func(who string) (string, bool) {
		if r != nil {
			if v, ok := r.ids[who]; ok && v != "" {
				return v, true
			}
		}
		return who, !move
	}
	facts, err := chatRows(tx, "body_events", `session_key=? AND job_id=0 ORDER BY id`, from)
	if err != nil {
		return err
	}
	for _, row := range facts {
		who, ok := mapped(rowString(row["who"]))
		if !ok {
			continue
		}
		row["session_key"], row["who"] = to, who
		if r != nil {
			row["detail"] = remapBodyEvent(rowString(row["detail"]), r)
		}
		if _, err := insertChatRow(tx, "body_events", row, "id"); err != nil {
			return err
		}
	}
	settings, err := chatRows(tx, "body_settings", `session_key=?`, from)
	if err != nil {
		return err
	}
	for _, row := range settings {
		who := rowString(row["who"])
		if who != "" {
			var ok bool
			if who, ok = mapped(who); !ok {
				continue
			}
		}
		if _, err := tx.Exec(`INSERT IGNORE INTO body_settings (session_key, who, spec) VALUES (?,?,?)`, to, who, rowString(row["spec"])); err != nil {
			return err
		}
	}
	return nil
}
