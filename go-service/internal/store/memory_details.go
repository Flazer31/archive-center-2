package store

import (
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// Only name references are expanded. Memory, effect and scene references
// remain references; related records and identity relations are not exposed.
func listedNameRefs(m memory.Memory) []string {
	refs := []string{m.About, m.Owner}
	refs = append(refs, m.To...)
	refs = append(refs, m.SeenBy...)
	if m.Roles != nil {
		for _, ids := range [][]string{m.Roles.Actor, m.Roles.Target, m.Roles.Object, m.Roles.Place, m.Roles.Speaker, m.Roles.Listener} {
			refs = append(refs, ids...)
		}
	}
	if m.Basis != nil {
		refs = append(refs, memory.SourceParts(m.Basis.From)...)
	}
	for _, e := range m.Effects {
		refs = append(refs, e.About)
	}
	return refs
}

func effectAbouts(m memory.Memory) []string {
	var out []string
	for _, e := range m.Effects {
		out = append(out, e.About)
	}
	return out
}

// listedMemory projects a stored record for the existing user-only management
// list. Disclosure flags and inherited ownership are assigned by ChatMemories.
// recordRef reports a reference to a record (M12) or one of its effects
// (M12.1), not to a name.
func recordRef(ref string) bool {
	if len(ref) < 2 || ref[0] != 'M' {
		return false
	}
	return strings.Trim(ref[1:], "0123456789.") == "" && ref[1] != '.'
}

func listedMemory(m memory.Memory, name func(string) string) ListedMemory {
	names := func(ids []string) []string {
		if len(ids) == 0 {
			return nil
		}
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = name(id)
		}
		return out
	}
	v := ListedMemory{
		Kind: m.Kind, Body: m.Body, Imp: m.Imp, About: name(m.About), Owner: name(m.Owner), Slot: m.Slot,
		Q: m.Q, Ref: m.Ref, At: m.At, Was: m.Was, Scene: m.Scene, Time: m.Time, Outcome: m.Outcome, Setting: m.Setting,
		SeenBy: names(m.SeenBy), Mode: m.Mode, To: names(m.To), Scope: m.Scope, Terms: m.Terms, Status: m.Status,
		Aspect: m.Aspect, Affect: m.Affect, Expressed: m.Expressed, Differs: m.Differs,
	}
	// whom the by-person view files it under (C14), from the stored
	// reference: a fact's about, a view's owner; a record (M12) is nobody,
	// whatever a name may look like
	if ref := m.About; m.Kind == "fact" || m.Kind == "view" {
		if m.Kind == "view" {
			ref = m.Owner
		}
		if ref != "" && !recordRef(ref) {
			v.Person, v.PersonRef = strings.TrimSpace(name(ref)), ref
		}
	}
	if m.Roles != nil {
		r := m.Roles
		v.Roles = &memory.Roles{Actor: names(r.Actor), Target: names(r.Target), Object: names(r.Object),
			Place: names(r.Place), Speaker: names(r.Speaker), Listener: names(r.Listener)}
	}
	if m.Basis != nil {
		b := *m.Basis
		b.From = memory.MapSource(b.From, name)
		v.Basis = &b
	}
	for _, e := range m.Effects {
		e.About = name(e.About)
		v.Effects = append(v.Effects, e)
	}
	return v
}
