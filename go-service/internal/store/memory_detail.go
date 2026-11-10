package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

var ErrDetailCursor = errors.New("bad_cursor")

// DetailLocator keeps the local logical ID, displayed ordinal and original
// job source turn separate. It is a read locator, never a lifecycle identity.
type DetailLocator struct {
	TurnID        int    `json:"turn_id"`
	Turn          int    `json:"turn"`
	Job           int64  `json:"job_id"`
	SourceSession string `json:"source_session"`
	SourceTurn    int    `json:"source_turn"`
	SourceKey     string `json:"source_key"`
	Inherited     bool   `json:"inherited"`
}

type MemoryDetail struct {
	ID string `json:"id"`
	DetailLocator
	Memory     memory.Memory     `json:"memory"` // persistent raw IDs, not labels
	Labels     map[string]string `json:"labels"`
	Source     memory.Source     `json:"source"` // only gen_jobs.source, never request/response/q
	Secret     bool              `json:"secret"`
	Related    []DetailRelated   `json:"related"`
	NextCursor *string           `json:"next_cursor"`
}

// Related nodes carry their own local locator. Unavailable nodes expose only
// their reference/status, never an out-of-scope body or source metadata.
type DetailRelated struct {
	Kind      string           `json:"kind"`
	Field     string           `json:"field,omitempty"`
	ID        string           `json:"id"`
	Status    string           `json:"status"` // available | missing | deleted | out_of_scope
	Secret    bool             `json:"secret"`
	Locator   *DetailLocator   `json:"locator,omitempty"`
	Memory    *memory.Memory   `json:"memory,omitempty"`
	Effect    *memory.Effect   `json:"effect,omitempty"`
	Link      *memory.Link     `json:"link,omitempty"`
	Endpoints []DetailEndpoint `json:"endpoints,omitempty"`
	Awareness *DetailAwareness `json:"awareness,omitempty"`
}

// Link source provenance and endpoint locators are different: a link written
// on a later turn may refer to an earlier memory. Endpoints are locators only;
// opening that memory is a separate detail request, not recursive traversal.
type DetailEndpoint struct {
	Field   string         `json:"field"`
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Locator *DetailLocator `json:"locator,omitempty"`
}

type DetailAwareness struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Job    int64  `json:"job_id"`
	memory.Knows
}

type detailRef struct{ field, id string }

func detailRefs(id string, m memory.Memory) []detailRef {
	var refs []detailRef
	add := func(field, ref string) {
		if ref != "" {
			refs = append(refs, detailRef{field, ref})
		}
	}
	add("at", m.At)
	if m.Basis != nil {
		add("basis.ref", m.Basis.Ref)
	}
	for i, ref := range m.Was {
		add(fmt.Sprintf("was.%d", i), ref)
	}
	add("scene", m.Scene)
	for k, e := range m.Effects {
		add("effects", fmt.Sprintf("%s.%d", id, k+1))
		for i, ref := range e.Was {
			add(fmt.Sprintf("effects.%d.was.%d", k+1, i), ref)
		}
	}
	return refs
}

// MemoryDetail reads one memory and one page of its direct evidence. There is
// no pipeline/session warmup, generation, vector work or transitive traversal.
func (d *DB) MemoryDetail(chat string, turnID int, id, cursor string, limit int) (MemoryDetail, error) {
	var out MemoryDetail
	stage, after, err := parseDetailCursor(cursor)
	if err != nil {
		return out, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	t, place, err := d.detailTurn(chat, turnID)
	if err != nil {
		return out, err
	}
	if t.Off {
		return out, ErrMemoryNotFound
	}
	jobs, _, err := d.turnJobs(t)
	if err != nil {
		return out, err
	}
	n, ok := detailID(id, 'M')
	if !ok {
		return out, ErrMemoryNotFound
	}
	var job int64
	var raw string
	err = d.sql.QueryRow(`SELECT job_id, detail FROM memories WHERE id=?`, n).Scan(&job, &raw)
	if err == sql.ErrNoRows {
		return out, ErrMemoryNotFound
	}
	if err != nil {
		return out, err
	}
	allowed := false
	for _, j := range jobs {
		if j == job {
			allowed = true
		}
	}
	if !allowed {
		return out, ErrMemoryNotFound
	}
	if err = json.Unmarshal([]byte(raw), &out.Memory); err != nil {
		return out, err
	}
	sources, err := d.JobSources([]int64{job})
	if err != nil {
		return out, err
	}
	out.Source = sources[job]
	out.ID = id
	out.DetailLocator = DetailLocator{TurnID: turnID, Turn: place, Job: job, SourceSession: out.Source.Session, SourceTurn: out.Source.Turn, SourceKey: out.Source.Key(), Inherited: out.Source.Session != chat}
	scope := &detailScope{db: d, chat: chat, jobs: map[int64]*DetailLocator{job: &out.DetailLocator}}
	out.Related = []DetailRelated{}
	// The management list's rule: unaware/hide_from on a memory or its
	// effects marks that memory protected. Read only kind/job metadata here;
	// the full awareness records themselves are paged below.
	out.Secret, err = scope.protected(id)
	if err != nil {
		return out, err
	}
	refs := detailRefs(id, out.Memory)
	// A cursor is the next position in refs, or the last PK visited in a
	// link/awareness table. Look ahead by one *visible* row; skipped jobs do
	// not cause duplicates, missing rows or an invented final page.
	appendRow := func(row DetailRelated, next string) bool {
		if len(out.Related) == limit {
			out.NextCursor = &next
			return false
		}
		out.Related = append(out.Related, row)
		return true
	}
	if stage == "refs" {
		for i := after; i < int64(len(refs)); i++ {
			row, err := scope.node(refs[i].id)
			if err != nil {
				return out, err
			}
			row.Field = refs[i].field
			if !appendRow(row, fmt.Sprintf("refs:%d", i)) {
				return out, d.detailLabels(&out)
			}
		}
		stage, after = "links", 0
	}
	if stage == "links" {
		for {
			var pk, j int64
			var l memory.Link
			err = d.sql.QueryRow(`SELECT id,job_id FROM links WHERE id>? AND (from_ref=? OR to_ref=? OR from_ref LIKE ? OR to_ref LIKE ?) ORDER BY id LIMIT 1`, after, id, id, id+".%", id+".%").Scan(&pk, &j)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return out, err
			}
			loc, err := scope.locate(j)
			if err != nil {
				return out, err
			}
			before := after
			after = pk
			if loc == nil {
				continue
			}
			if err = d.sql.QueryRow(`SELECT from_ref,rel,to_ref,q FROM links WHERE id=?`, pk).Scan(&l.From, &l.Rel, &l.To, &l.Q); err != nil {
				return out, err
			}
			l.ID = fmt.Sprintf("L%d", pk)
			row := DetailRelated{Kind: "link", ID: l.ID, Status: "available", Locator: loc, Link: &l}
			for _, ref := range []detailRef{{"from", l.From}, {"to", l.To}} {
				if ref.id == id {
					row.Endpoints = append(row.Endpoints, DetailEndpoint{Field: ref.field, ID: ref.id, Status: "available", Locator: &out.DetailLocator})
					continue
				}
				node, err := scope.node(ref.id)
				if err != nil {
					return out, err
				}
				row.Endpoints = append(row.Endpoints, DetailEndpoint{Field: ref.field, ID: ref.id, Status: node.Status, Locator: node.Locator})
			}
			if row.Secret, err = scope.protected(l.ID); err != nil {
				return out, err
			}
			if !appendRow(row, fmt.Sprintf("links:%d", before)) {
				return out, d.detailLabels(&out)
			}
		}
		stage, after = "awareness", 0
	}
	if stage == "awareness" {
		for {
			var pk int64
			var a DetailAwareness
			err = d.sql.QueryRow(`SELECT id,job_id FROM awareness WHERE id>? AND (target=? OR target LIKE ?) ORDER BY id LIMIT 1`, after, id, id+".%").Scan(&pk, &a.Job)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return out, err
			}
			loc, err := scope.locate(a.Job)
			if err != nil {
				return out, err
			}
			before := after
			after = pk
			if loc == nil {
				continue
			}
			if err = d.sql.QueryRow(`SELECT target,kind,who,via,from_who,extent,part,ref,q FROM awareness WHERE id=?`, pk).Scan(&a.Target, &a.Kind, &a.Who, &a.Via, &a.From, &a.Extent, &a.Part, &a.Ref, &a.Q); err != nil {
				return out, err
			}
			row := DetailRelated{Kind: "awareness", ID: strconv.FormatInt(pk, 10), Status: "available", Locator: loc, Awareness: &a}
			if !appendRow(row, fmt.Sprintf("awareness:%d", before)) {
				return out, d.detailLabels(&out)
			}
		}
	}
	return out, d.detailLabels(&out)
}

func parseDetailCursor(cursor string) (string, int64, error) {
	if cursor == "" {
		return "refs", 0, nil
	}
	s := strings.Split(cursor, ":")
	if len(s) != 2 {
		return "", 0, ErrDetailCursor
	}
	n, err := strconv.ParseInt(s[1], 10, 64)
	if err != nil || n < 0 {
		return "", 0, ErrDetailCursor
	}
	switch s[0] {
	case "refs", "links", "awareness":
		return s[0], n, nil
	}
	return "", 0, ErrDetailCursor
}

func detailID(id string, prefix byte) (int64, bool) {
	if len(id) < 2 || id[0] != prefix {
		return 0, false
	}
	n, err := strconv.ParseInt(id[1:], 10, 64)
	return n, err == nil && n > 0
}

func (s *detailScope) node(ref string) (DetailRelated, error) {
	r := DetailRelated{Kind: "reference", ID: ref, Status: "missing"}
	base, tail, hasEffect := strings.Cut(ref, ".")
	if n, ok := detailID(base, 'M'); ok {
		var j int64
		// Scope is checked before reading a related node's detail.
		err := s.db.sql.QueryRow(`SELECT job_id FROM memories WHERE id=?`, n).Scan(&j)
		if err == sql.ErrNoRows {
			return s.absent(r, base)
		}
		if err != nil {
			return r, err
		}
		loc, err := s.locate(j)
		if err != nil {
			return r, err
		}
		if loc == nil {
			r.Status = "out_of_scope"
			return r, nil
		}
		var raw string
		var m memory.Memory
		if err = s.db.sql.QueryRow(`SELECT detail FROM memories WHERE id=?`, n).Scan(&raw); err != nil {
			return r, err
		}
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return r, err
		}
		if hasEffect {
			k, err := strconv.Atoi(tail)
			if err != nil {
				return r, nil
			}
			for i, e := range m.Effects {
				if i+1 == k {
					r.Status, r.Locator, r.Effect = "available", loc, &e
					break
				}
			}
		} else {
			r.Status, r.Locator, r.Memory = "available", loc, &m
		}
		if r.Status == "available" {
			r.Secret, err = s.protected(base)
		}
		return r, err
	}
	// Names/scenes/identity and meaning links have globally issued IDs but
	// their records still belong to a scoped generation job.
	for _, kind := range []struct {
		p     byte
		table string
	}{{'N', "names"}, {'S', "scenes"}, {'R', "name_relations"}, {'L', "links"}} {
		if n, ok := detailID(ref, kind.p); ok {
			var j int64
			err := s.db.sql.QueryRow(`SELECT job_id FROM `+kind.table+` WHERE id=?`, n).Scan(&j)
			if err == sql.ErrNoRows {
				return s.absent(r, ref)
			}
			if err != nil {
				return r, err
			}
			loc, err := s.locate(j)
			if err != nil {
				return r, err
			}
			if loc == nil {
				r.Status = "out_of_scope"
			} else {
				r.Status, r.Locator = "available", loc
			}
			return r, nil
		}
	}
	return r, nil
}

func (s *detailScope) protected(id string) (bool, error) {
	secret := false
	err := s.db.each(`SELECT job_id FROM awareness WHERE (target=? OR target LIKE ?) AND kind IN ('unaware','hide_from')`, []any{id, id + ".%"}, func(rows *sql.Rows) error {
		var j int64
		if err := rows.Scan(&j); err != nil {
			return err
		}
		loc, err := s.locate(j)
		if loc != nil {
			secret = true
		}
		return err
	})
	return secret, err
}

func (s *detailScope) absent(r DetailRelated, base string) (DetailRelated, error) {
	var exists int
	err := s.db.sql.QueryRow(`SELECT 1 FROM gen_items WHERE item_id=? LIMIT 1`, base).Scan(&exists)
	if err == nil {
		r.Status = "deleted"
	} else if err != sql.ErrNoRows {
		return r, err
	}
	return r, nil
}

func (d *DB) detailLabels(out *MemoryDetail) error {
	refs := detailNameRefs(out.Memory)
	for _, r := range out.Related {
		if r.Memory != nil {
			refs = append(refs, detailNameRefs(*r.Memory)...)
		}
		if r.Effect != nil {
			refs = append(refs, r.Effect.About)
		}
		if r.Awareness != nil {
			refs = append(refs, r.Awareness.Who, r.Awareness.From)
		}
	}
	names, err := d.NamesByID(refs)
	out.Labels = names
	return err
}

func detailNameRefs(m memory.Memory) []string {
	refs := listedNameRefs(m)
	refs = append(refs, m.Unaware...)
	refs = append(refs, m.HideFrom...)
	refs = append(refs, m.AllowTo...)
	for _, k := range m.Knows {
		refs = append(refs, k.Who, k.From)
	}
	return refs
}
