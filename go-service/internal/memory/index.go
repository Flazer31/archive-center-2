package memory

import "strings"

// Words are the distinct words of a text as the reading range compares
// them: without their particles, two letters or more. Names are not left
// out here (which words are names depends on the names a read sees).
func Words(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range wordPattern.FindAllString(text, -1) {
		s := stem(w)
		if len([]rune(s)) < 2 || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// IndexEntry is one word or one reference of a stored item, with the field
// it is in.
type IndexEntry struct{ Field, Value string }

// MemoryWords are the words of a memory by field, for the search index.
func MemoryWords(m Memory) []IndexEntry {
	var out []IndexEntry
	add := func(field, text string) {
		for _, w := range Words(text) {
			out = append(out, IndexEntry{field, w})
		}
	}
	// a slot is also kept whole: it names what a value is ("돈", "호감"),
	// often one word too short to be a word here
	whole := func(field, text string) {
		if t := strings.TrimSpace(text); t != "" {
			out = append(out, IndexEntry{field, t})
		}
	}
	add("body", m.Body)
	add("q", m.Q)
	whole("slot", m.Slot)
	add("slot", m.Slot)
	add("setting", m.Setting)
	add("scope", m.Scope)
	add("terms", m.Terms)
	add("time", m.Time)
	for _, e := range m.Effects {
		whole("effect_slot", e.Slot)
		add("effect", e.Slot+" "+e.Before+" "+e.Value)
	}
	return out
}

// MemoryRefs are the ids a memory refers to by field (names, records,
// effects), for the reverse lookup of who and what involves it and of the
// later records that replaced an earlier one (was).
func MemoryRefs(m Memory) []IndexEntry {
	var out []IndexEntry
	add := func(field string, refs ...string) {
		for _, r := range refs {
			if r != "" {
				out = append(out, IndexEntry{field, r})
			}
		}
	}
	add("about", m.About)
	add("owner", m.Owner)
	add("to", m.To...)
	add("at", m.At)
	add("seen_by", m.SeenBy...)
	if m.Roles != nil {
		add("actor", m.Roles.Actor...)
		add("target", m.Roles.Target...)
		add("object", m.Roles.Object...)
		add("place", m.Roles.Place...)
		add("speaker", m.Roles.Speaker...)
		add("listener", m.Roles.Listener...)
	}
	add("was", m.Was...)
	for _, e := range m.Effects {
		add("effect", e.About)
		add("effect_was", e.Was...)
	}
	if m.Basis != nil {
		add("basis", m.Basis.Ref)
		add("basis_from", m.Basis.From)
	}
	return out
}
