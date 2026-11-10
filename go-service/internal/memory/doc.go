package memory

import (
	"regexp"
	"strings"
)

// recordID is a stored record id (memory, effect, relation, link, scene),
// not a name written as text ("Mina", "Sarah").
var recordID = regexp.MustCompile(`^[MRLS]\d+(\.\d+)?$`)

// DocText is the text a memory is embedded as: its kind, the people and
// things it involves (ids read as names), its content and what tells it
// from a record with the same body (time, setting, conditions, scope,
// result, status, the changes it made), without ids or quotes. names must
// hold every name id the memory uses, so the same memory gives the same
// text however it is read.
func DocText(m KnownMemory, names map[string]string) string {
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		if recordID.MatchString(id) {
			return "" // another record: its own text, not this one's
		}
		return id
	}
	var b []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			b = append(b, s)
		}
	}
	list := func(label string, ids []string) {
		var ns []string
		for _, id := range ids {
			if n := name(id); n != "" {
				ns = append(ns, n)
			}
		}
		if len(ns) > 0 {
			add(label + " " + strings.Join(ns, ","))
		}
	}
	head := m.Kind
	if m.Mode != "" {
		head += "/" + m.Mode
	}
	if m.Aspect != "" {
		head += "/" + m.Aspect
	}
	add(head)
	if n := name(m.Owner); n != "" {
		add(n + ":")
	}
	add(name(m.About))
	if m.Slot != "" {
		add(m.Slot + ":")
	}
	add(m.Body)
	list("to", m.To)
	if r := m.Roles; r != nil {
		list("actor", r.Actor)
		list("target", r.Target)
		list("object", r.Object)
		list("place", r.Place)
		list("speaker", r.Speaker)
		list("listener", r.Listener)
	}
	list("seen by", m.SeenBy)
	for _, v := range [][2]string{{"time", m.Time}, {"setting", m.Setting}, {"scope", m.Scope}, {"terms", m.Terms}, {"outcome", m.Outcome}, {"status", m.Status}} {
		if v[1] != "" {
			add(v[0] + " " + v[1])
		}
	}
	for _, e := range m.Effects {
		var parts []string
		if n := name(e.About); n != "" {
			parts = append(parts, n)
		}
		if e.Slot != "" {
			parts = append(parts, e.Slot)
		}
		if e.Before != "" || e.Value != "" {
			parts = append(parts, strings.TrimSpace(e.Before+" -> "+e.Value))
		}
		if e.Status != "" {
			parts = append(parts, e.Status)
		}
		if len(parts) > 0 {
			add("[" + strings.Join(parts, " ") + "]")
		}
	}
	return strings.Join(b, " ")
}

// NameRefs lists the ids a memory uses (DocText reads the name ids).
func NameRefs(m Memory) []string { return refsOf(m) }
