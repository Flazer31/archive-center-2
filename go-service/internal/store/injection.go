package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// InjectedMemory is a stored record at delivery time, projected as the
// memory list projects it; Secret records delivery's existing lane decision.
// It is not persisted.
type InjectedMemory struct {
	ListedMemory
}

// InjectedMemories reads only the delivered IDs, preserving delivery order.
// Scope positions are the chat's story turn numbers, including inherited turns.
func (d *DB) InjectedMemories(chat string, ids []string, scope []memory.ScopedTurn, secrets map[string]bool) ([]InjectedMemory, error) {
	out := []InjectedMemory{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		n, err := strconv.ParseInt(strings.TrimPrefix(id, "M"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("memory id: %w", err)
		}
		args = append(args, n)
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	type read struct {
		m         memory.Memory
		turn      int
		inherited bool
	}
	byID := map[string]read{}
	refs := []string{}
	positions := map[string]int{}
	for i, t := range scope {
		positions[t.Source.Key()] = i + 1
		positions[t.OwnSource().Key()] = i + 1
	}
	err := d.each(`SELECT m.id, m.detail, j.session_key, j.turn_id, j.source_key
		FROM memories m JOIN gen_jobs j ON j.job_id=m.job_id WHERE m.id IN `+in, args, func(rows *sql.Rows) error {
		var id int64
		var raw, session, source string
		var turn int
		if err := rows.Scan(&id, &raw, &session, &turn, &source); err != nil {
			return err
		}
		var m memory.Memory
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		if pos, ok := positions[source]; ok {
			turn = pos
		}
		byID[fmt.Sprintf("M%d", id)] = read{m, turn, session != chat}
		refs = append(refs, listedNameRefs(m)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	names, err := d.NamesByID(refs)
	if err != nil {
		return nil, err
	}
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}
	for _, id := range ids {
		r, ok := byID[id]
		if !ok {
			continue
		}
		v := listedMemory(r.m, name)
		v.ID, v.Turn, v.Inherited, v.Secret = id, r.turn, r.inherited, secrets[id]
		out = append(out, InjectedMemory{v})
	}
	return out, nil
}
