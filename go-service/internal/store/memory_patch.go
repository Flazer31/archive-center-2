package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

var ErrBadMemoryPatch = errors.New("bad_memory_patch")

// Patch fields have presence semantics: omission preserves, null/empty clears.
// Nested objects are partial too. effects is keyed by the existing 1-based k.
type MemoryPatch map[string]json.RawMessage

func patchObject(dst any, raw json.RawMessage, allowed string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return ErrBadMemoryPatch
	}
	old, _ := json.Marshal(dst)
	var values map[string]json.RawMessage
	json.Unmarshal(old, &values)
	if values == nil {
		values = map[string]json.RawMessage{}
	}
	for key, value := range fields {
		if !strings.Contains(" "+allowed+" ", " "+key+" ") {
			return ErrBadMemoryPatch
		}
		delete(values, key)
		if string(value) != "null" && string(value) != `""` {
			values[key] = value
		}
	}
	b, _ := json.Marshal(values)
	reflect.ValueOf(dst).Elem().SetZero()
	if json.Unmarshal(b, dst) != nil {
		return ErrBadMemoryPatch
	}
	return nil
}

func applyMemoryPatch(m *memory.Memory, patch MemoryPatch) error {
	common := "body imp at was scene"
	kind := ""
	switch m.Kind {
	case "fact":
		kind = "about slot mode to scope terms status"
	case "view":
		kind = "owner about aspect basis affect expressed differs"
	case "event":
		kind = "roles time setting seen_by outcome effects"
	}
	for key, raw := range patch {
		if !strings.Contains(" "+common+" "+kind+" ", " "+key+" ") {
			return ErrBadMemoryPatch
		}
		clear := string(raw) == "null" || string(raw) == `""`
		switch key {
		case "roles":
			if clear {
				m.Roles = nil
				continue
			}
			if m.Roles == nil {
				m.Roles = &memory.Roles{}
			}
			if err := patchObject(m.Roles, raw, "actor target object place speaker listener"); err != nil {
				return err
			}
		case "basis":
			if clear {
				m.Basis = nil
				continue
			}
			if m.Basis == nil {
				m.Basis = &memory.Basis{}
			}
			if err := patchObject(m.Basis, raw, "via from ref confidence"); err != nil {
				return err
			}
		case "affect":
			if clear {
				m.Affect = nil
				continue
			}
			if m.Affect == nil {
				m.Affect = &memory.Affect{}
			}
			if err := patchObject(m.Affect, raw, "intensity persistence"); err != nil {
				return err
			}
		case "effects":
			var es map[string]json.RawMessage
			if json.Unmarshal(raw, &es) != nil || es == nil {
				return ErrBadMemoryPatch
			}
			for k, part := range es {
				found := false
				for i := range m.Effects {
					if k == fmt.Sprint(i+1) {
						found = true
						if err := patchObject(&m.Effects[i], part, "about slot before value delta status was"); err != nil {
							return err
						}
					}
				}
				if !found {
					return ErrBadMemoryPatch
				}
			}
		default:
			// Decode a single selected field onto a copy; Go's JSON presence handling
			// clears scalars as well as lists, unlike unmarshalling null into a scalar.
			b, _ := json.Marshal(m)
			var fields map[string]json.RawMessage
			json.Unmarshal(b, &fields)
			delete(fields, key)
			if !clear {
				fields[key] = raw
			}
			b, _ = json.Marshal(fields)
			var next memory.Memory
			if json.Unmarshal(b, &next) != nil {
				return ErrBadMemoryPatch
			}
			*m = next
		}
	}
	if m.Imp != nil && (*m.Imp < 0 || *m.Imp > 10) {
		return ErrBadImportance
	}
	return nil
}

func patchSuppliesRef(patch MemoryPatch, path string) bool {
	for key, raw := range patch {
		if path == key || strings.HasPrefix(path, key+".") {
			if key == "effects" || key == "roles" || key == "basis" {
				var fields map[string]json.RawMessage
				if json.Unmarshal(raw, &fields) == nil && fields != nil {
					rest := strings.TrimPrefix(path, key+".")
					head, tail, has := strings.Cut(rest, ".")
					part, ok := fields[head]
					if !ok {
						continue
					}
					if key == "effects" && has {
						var ef map[string]json.RawMessage
						json.Unmarshal(part, &ef)
						field, _, _ := strings.Cut(tail, ".")
						_, ok = ef[field]
						if !ok {
							continue
						}
					}
				}
			}
			return true
		}
	}
	return false
}

// MemoryEditLocator shares C15's narrow scope owner. No ChatMemories,
// LoadSession or pipeline cache is used for edit ownership checks.
func (d *DB) MemoryEditLocator(chat, id string) (DetailLocator, error) {
	var zero DetailLocator
	n, ok := detailID(id, 'M')
	if !ok {
		return zero, ErrMemoryNotFound
	}
	var job int64
	if err := d.sql.QueryRow(`SELECT job_id FROM memories WHERE id=?`, n).Scan(&job); err != nil {
		if err == sql.ErrNoRows {
			return zero, ErrMemoryNotFound
		}
		return zero, err
	}
	scope := detailScope{db: d, chat: chat, jobs: map[int64]*DetailLocator{}}
	loc, err := scope.locate(job)
	if err != nil {
		return zero, err
	}
	if loc == nil {
		return zero, ErrMemoryNotFound
	}
	if loc.Inherited {
		return *loc, ErrInheritedOnly
	}
	return *loc, nil
}

func (d *DB) EditMemoryPatch(chat, id string, body *string, imp *float64, patch MemoryPatch) (bool, error) {
	return againOnConflict(func() (bool, error) { return d.editMemoryPatch(chat, id, body, imp, patch) })
}

func (d *DB) editMemoryPatch(chat, id string, body *string, imp *float64, patch MemoryPatch) (bool, error) {
	// Legacy callers keep their empty-body and nil-importance contract.
	if body != nil && strings.TrimSpace(*body) == "" {
		return false, ErrEmptyBody
	}
	if imp != nil && (*imp < 0 || *imp > 10) {
		return false, ErrBadImportance
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	n, job, m, err := ownMemory(tx, chat, id)
	if err != nil {
		return false, err
	}
	beforeBytes, _ := json.Marshal(m)
	var before memory.Memory
	json.Unmarshal(beforeBytes, &before)
	if body != nil {
		m.Body = strings.TrimSpace(*body)
	}
	if imp != nil {
		m.Imp = imp
	}
	if err := applyMemoryPatch(&m, patch); err != nil {
		return false, err
	}
	origins, err := referenceOrigins(tx, "memories", n)
	if err != nil {
		return false, err
	}
	r, err := userNameResolver(tx)
	if err != nil {
		return false, err
	}
	var refErr error
	memoryReferenceSlots(&m, func(path string, p *string) {
		if refErr != nil || !patchSuppliesRef(patch, path) {
			return
		}
		*p, refErr = referenceValue(tx, r, origins, 0, chat, job, "memories", n, path, *p, true)
	})
	if refErr != nil {
		return false, refErr
	}
	names, err := memoryNamesOn(tx, before, m)
	if err != nil {
		return false, err
	}
	changed := memory.DocText(memory.KnownMemory{Memory: before}, names) != memory.DocText(memory.KnownMemory{Memory: m}, names)
	if err := writeMemoryMirrors(tx, chat, job, n, m); err != nil {
		return false, err
	}
	if changed {
		if err := unlinkVectors(tx, job); err != nil {
			return false, err
		}
	}
	return changed, tx.Commit()
}
