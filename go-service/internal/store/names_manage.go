package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"archivecenter2/goservice/internal/memory"
)

var ErrNameNotFound = errors.New("name_not_found")
var ErrEmptyName = errors.New("empty_name")

type ListedName struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Memories  int      `json:"memories"`
	Aliases   []string `json:"aliases"`
	AliasIDs  []string `json:"alias_ids"`
	Inherited bool     `json:"inherited"`
}

// ChatNames reads the same current scope as injection. Alias names remain
// real names and relations, so Select resolves the old and new surfaces.
func (d *DB) ChatNames(chat string, scope []memory.ScopedTurn) ([]ListedName, error) {
	k, _, err := d.knownScope(memory.On(scope), false)
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{}
	err = d.each(`SELECT id FROM names WHERE session_key=?`, []any{chat}, func(rows *sql.Rows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		owned[fmt.Sprintf("N%d", id)] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	names := map[string]memory.KnownName{}
	for _, n := range k.Names {
		names[n.ID] = n
	}
	alias := map[string]string{}
	for _, r := range k.Relations {
		if (r.How == "alias" || r.How == "user_alias") && r.Name != r.SameAs && names[r.SameAs].ID != "" {
			alias[r.Name] = r.SameAs
		}
	}
	canonical := func(id string) string {
		for i := 0; i < len(alias); i++ {
			next, ok := alias[id]
			if !ok {
				break
			}
			id = next
		}
		return id
	}
	uses := map[string]map[string]bool{}
	memoryIDs := map[string]bool{}
	addUse := func(id, item string) {
		id = canonical(id)
		if uses[id] == nil {
			uses[id] = map[string]bool{}
		}
		uses[id][item] = true
	}
	for _, m := range k.Memories {
		memoryIDs[m.ID] = true
		seen := map[string]bool{}
		for _, ref := range memory.MemoryRefs(m.Memory) {
			seen[canonical(ref.Value)] = true
		}
		for _, who := range m.Knows {
			seen[canonical(who.Who)], seen[canonical(who.From)] = true, true
		}
		for _, list := range []memory.Who{m.Unaware, m.HideFrom, m.AllowTo} {
			for _, who := range list {
				seen[canonical(who)] = true
			}
		}
		for id := range seen {
			addUse(id, m.ID)
		}
	}
	for _, a := range k.Awareness {
		target, _, _ := strings.Cut(a.Target, ".")
		if memoryIDs[target] {
			addUse(a.Who, target)
			addUse(a.From, target)
		}
	}
	out := []ListedName{}
	for _, n := range k.Names {
		if canonical(n.ID) != n.ID {
			continue
		}
		v := ListedName{ID: n.ID, Name: n.Name, Type: n.Type, Memories: len(uses[n.ID]), Aliases: []string{}, AliasIDs: []string{}, Inherited: !owned[n.ID]}
		for _, a := range k.Names {
			if a.ID != n.ID && canonical(a.ID) == n.ID {
				v.Aliases = append(v.Aliases, a.Name)
				v.AliasIDs = append(v.AliasIDs, a.ID)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

func ownName(tx *sql.Tx, chat, id string) (StoredName, error) {
	var n StoredName
	num, err := strconv.ParseInt(strings.TrimPrefix(id, "N"), 10, 64)
	if err != nil || !strings.HasPrefix(id, "N") {
		return n, ErrNameNotFound
	}
	var session string
	err = tx.QueryRow(`SELECT session_key,job_id,name,type,q FROM names WHERE id=? FOR UPDATE`, num).Scan(&session, &n.Job, &n.Name, &n.Type, &n.Q)
	if err == sql.ErrNoRows || (err == nil && session != chat) {
		return n, ErrNameNotFound
	}
	n.ID = id
	return n, err
}

// EditName keeps the former surface as an ordinary alias. Original prose
// and quotes remain source evidence; labels and DocText use the new name.
func (d *DB) EditName(chat, id, name string) ([]string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrEmptyName
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	n, err := ownName(tx, chat, id)
	if err != nil {
		return nil, err
	}
	if n.Name == name {
		return []string{}, tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE names SET name=? WHERE id=?`, name, strings.TrimPrefix(id, "N")); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO names (session_key,job_id,name,type,q) VALUES (?,?,?,?,?)`, chat, n.Job, n.Name, n.Type, n.Q)
	if err != nil {
		return nil, err
	}
	alias, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO name_relations (session_key,job_id,name_ref,same_as,how,q) VALUES (?,?,?,?,'user_alias',?)`, chat, n.Job, fmt.Sprintf("N%d", alias), id, n.Q); err != nil {
		return nil, err
	}
	sessions, err := rewriteName(tx, id, id, n.Name)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return sessions, nil
}

// MergeNames moves structured references to keep, including references in
// child-owned jobs. Each name id is global, so unrelated chats are untouched.
// The merged name is retained as an alias, with its provenance and quote.
func (d *DB) MergeNames(chat, keep, merge string) ([]string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = ownName(tx, chat, keep)
	if err != nil {
		return nil, err
	}
	n, err := ownName(tx, chat, merge)
	if err != nil {
		return nil, err
	}
	if keep == merge {
		return []string{}, tx.Commit()
	}
	res, err := tx.Exec(`INSERT INTO name_relations (session_key,job_id,name_ref,same_as,how,q) VALUES (?,?,?,?,'user_alias',?)`, chat, n.Job, merge, keep, n.Q)
	if err != nil {
		return nil, err
	}
	relation, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	res, err = tx.Exec(`INSERT INTO name_merges (session_key,job_id,keep_ref,merge_ref,relation_id,created_at) VALUES (?,?,?,?,?,?)`, chat, n.Job, keep, merge, relation, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	op, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	r, err := userNameResolver(tx)
	if err != nil {
		return nil, err
	}
	sessions, err := applyNameReferences(tx, 0, op, r)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return sessions, nil
}

// Generation may still refer to a stored alias id. Only an explicit user
// alias redirects it; a model's disguise/identity assertion remains a
// separate, possibly private relation as before.
func resolveUserAliases(tx *sql.Tx, r *resolver, out memory.Output) error {
	// Collect references without appending resolution diagnostics twice.
	probe := *r
	probe.notes = nil
	var refs []string
	aware := func(a memory.Awareness) {
		refs = append(refs, a.Unaware...)
		refs = append(refs, a.HideFrom...)
		refs = append(refs, a.AllowTo...)
		for _, k := range a.Knows {
			refs = append(refs, k.Who, k.From, k.Ref)
		}
	}
	for _, m := range out.Memories {
		m = probe.memory(m)
		for _, e := range memory.MemoryRefs(m) {
			refs = append(refs, e.Value)
		}
		refs = append(refs, m.Ref, m.Scene)
		aware(m.Awareness)
	}
	for _, e := range out.Entities {
		refs = append(refs, probe.ref(e.SameAs))
		aware(probe.awareness(e.Awareness))
	}
	for _, l := range out.Links {
		refs = append(refs, probe.ref(l.From), probe.ref(l.To))
		aware(probe.awareness(l.Awareness))
	}
	seen := map[string]bool{}
	aliases := map[string]string{}
	for len(refs) > 0 {
		var args []any
		for _, id := range refs {
			if strings.HasPrefix(id, "N") && !seen[id] {
				seen[id] = true
				args = append(args, id)
			}
		}
		if len(args) == 0 {
			break
		}
		refs = nil
		rows, err := tx.Query(`SELECT name_ref,same_as FROM name_relations WHERE how='user_alias' AND name_ref IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`) ORDER BY id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var from, to string
			if err = rows.Scan(&from, &to); err != nil {
				rows.Close()
				return err
			}
			aliases[from] = to
			refs = append(refs, to)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	for from, to := range aliases {
		for i := 0; i < len(aliases); i++ {
			next, ok := aliases[to]
			if !ok {
				break
			}
			to = next
		}
		r.ids[from] = to
	}
	return nil
}

// A user-managed alias is name metadata, not a story event. A branch can
// already refer to a merged id even when the kept name's creation job is
// outside its read scope. Hydrate only the connected user-managed names
// and aliases, never that job's memories or model identity assertions.
func (d *DB) hydrateUserAliases(k *memory.Known) error {
	refs := []string{}
	names, relations := map[string]bool{}, map[string]bool{}
	for _, n := range k.Names {
		refs = append(refs, n.ID)
		names[n.ID] = true
	}
	for _, r := range k.Relations {
		relations[r.ID] = true
	}
	for _, m := range k.Memories {
		refs = append(refs, memory.NameRefs(m.Memory)...)
	}
	for _, a := range k.Awareness {
		refs = append(refs, a.Who, a.From)
	}
	seen, endpoints := map[string]bool{}, map[string]bool{}
	for len(refs) > 0 {
		args := []any{}
		for _, id := range refs {
			if strings.HasPrefix(id, "N") && !seen[id] {
				seen[id] = true
				args = append(args, id)
			}
		}
		refs = nil
		for start := 0; start < len(args); start += 250 {
			part := args[start:min(start+250, len(args))]
			in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + ")"
			both := append(append([]any{}, part...), part...)
			err := d.each(`SELECT id,name_ref,same_as FROM name_relations WHERE how='user_alias' AND (name_ref IN `+in+` OR same_as IN `+in+`) ORDER BY id`, both, func(rows *sql.Rows) error {
				var n int64
				var from, to string
				if err := rows.Scan(&n, &from, &to); err != nil {
					return err
				}
				id := fmt.Sprintf("R%d", n)
				if !relations[id] {
					relations[id] = true
					k.Relations = append(k.Relations, memory.KnownRelation{ID: id, Name: from, SameAs: to, How: "alias"})
				}
				endpoints[from], endpoints[to] = true, true
				refs = append(refs, from, to)
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	var ids []int64
	for id := range endpoints {
		if !names[id] {
			if n, e := strconv.ParseInt(strings.TrimPrefix(id, "N"), 10, 64); e == nil {
				ids = append(ids, n)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for start := 0; start < len(ids); start += 500 {
		in, args := inJobs(ids[start:min(start+500, len(ids))])
		if err := d.each(`SELECT id,name,type FROM names WHERE id IN `+in+` ORDER BY id`, args, func(rows *sql.Rows) error {
			var id int64
			var n memory.KnownName
			if err := rows.Scan(&id, &n.Name, &n.Type); err != nil {
				return err
			}
			n.ID = fmt.Sprintf("N%d", id)
			k.Names = append(k.Names, n)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// rewriteName reuses generation's typed resolver, never a prose replace.
// Both the lossless detail and its query mirrors/index entries change in
// the same transaction. Vectors are detached until the worker reindexes.
func rewriteName(tx *sql.Tx, from, to string, oldLabel ...string) ([]string, error) {
	var after int64
	sessions := map[string]bool{}
	for {
		rows, err := tx.Query(`SELECT id,session_key,job_id,detail FROM memories WHERE id>? AND `+mergeMemoryRefs+` ORDER BY id LIMIT 100 FOR UPDATE`, after, from)
		if err != nil {
			return nil, err
		}
		type item struct {
			id, job      int64
			session, raw string
		}
		page := []item{}
		for rows.Next() {
			var v item
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
			var m memory.Memory
			if err := json.Unmarshal([]byte(v.raw), &m); err != nil {
				return nil, err
			}
			names, err := memoryNamesOn(tx, m)
			if err != nil {
				return nil, err
			}
			beforeNames := map[string]string{}
			for k, v := range names {
				beforeNames[k] = v
			}
			if len(oldLabel) > 0 {
				beforeNames[from] = oldLabel[0]
			}
			before := memory.DocText(memory.KnownMemory{Memory: m}, beforeNames)
			r := resolver{ids: map[string]string{from: to}}
			m = r.memory(m)
			if err := writeMemoryMirrors(tx, v.session, v.job, v.id, m); err != nil {
				return nil, err
			}
			if before != memory.DocText(memory.KnownMemory{Memory: m}, names) {
				if err := unlinkVectors(tx, v.job); err != nil {
					return nil, err
				}
				sessions[v.session] = true
			}
		}
	}
	out := []string{}
	for s := range sessions {
		out = append(out, s)
	}
	return out, nil
}
