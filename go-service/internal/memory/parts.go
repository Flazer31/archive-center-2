package memory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PartChars is the most reply text the critic reads in one call: a longer
// reply is read in parts, one call each, in order (a long reply's details
// were lost read in one call: a reply of 25,020 chars).
const PartChars = 20000

// ReplyParts is the turn's reply text (as TurnText writes it) in parts of
// about equal size, none over PartChars, each cut at a line end where one
// is near; a reply of at most PartChars chars is one part.
func ReplyParts(src Source) []string {
	var b strings.Builder
	chars := 0
	for _, r := range src.Responses {
		b.WriteString("REPLY: " + r.Data + "\n")
		chars += len([]rune(r.Data))
	}
	text := []rune(b.String())
	if chars <= PartChars {
		return []string{string(text)}
	}
	var parts []string
	for start := 0; start < len(text); {
		// what is left, in equal parts of at most PartChars
		left := len(text) - start
		n := (left + PartChars - 1) / PartChars
		size := (left + n - 1) / n
		end := start + size
		if n > 1 {
			// back to the last line end in the part's last fifth
			for i := end; i > start+size*4/5; i-- {
				if text[i-1] == '\n' {
					end = i
					break
				}
			}
		}
		parts = append(parts, string(text[start:end]))
		start = end
	}
	return parts
}

// PartText is what the critic reads as THIS TURN for part k (from 0) of
// parts: the first part with the user's input, a later one with what the
// earlier parts recorded (earlier: their merged answer).
func PartText(src Source, parts []string, k int, earlier Output) string {
	var b strings.Builder
	fmt.Fprintf(&b, "(This turn's reply is long and is read in %d parts. This is part %d of %d.", len(parts), k+1, len(parts))
	if k == 0 {
		b.WriteString(" Record what this part shows; the rest of the reply is read next.)\n")
		for _, m := range src.Members {
			b.WriteString("USER: " + m.Data + "\n")
		}
		b.WriteString(parts[0])
		return b.String()
	}
	e, _ := json.Marshal(earlier)
	b.WriteString(" The user's input and the earlier parts were read before; EARLIER PARTS is what they recorded." +
		" Use its ids for the same names, memories and scenes, do not write its items again, and give new items new temporary ids." +
		" elapsed_days: only the days that pass after the end of the previous part.)\n")
	b.WriteString("EARLIER PARTS: " + string(e) + "\n")
	b.WriteString(parts[k])
	return b.String()
}

var partID = regexp.MustCompile(`^([a-z]+)(\d+)$`)

// Renumber gives the temporary ids an answer defines (names, memories,
// scenes, links) the part's own range, k*1000 + n (part k from 1), and
// rewrites its references to them, so the parts' answers merge without one
// part's n1 meeting another's. References to anything else (KNOWN ids, an
// earlier part's renumbered ids) are kept.
func Renumber(out Output, k int) Output {
	ids := map[string]string{}
	def := func(id string) {
		if m := partID.FindStringSubmatch(id); m != nil {
			n, _ := strconv.Atoi(m[2])
			if n < 1000 { // an earlier part's id written as new: kept as is
				ids[id] = m[1] + strconv.Itoa(k*1000+n)
			}
		}
	}
	for _, e := range out.Entities {
		def(e.ID)
		def(e.Relation)
	}
	for _, m := range out.Memories {
		def(m.ID)
	}
	for _, s := range out.Scenes {
		def(s.ID)
	}
	for _, l := range out.Links {
		def(l.ID)
	}
	if len(ids) == 0 {
		return out
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return out
	}
	v = renameRefs(v, ids)
	raw, err = json.Marshal(v)
	if err != nil {
		return out
	}
	var got Output
	if json.Unmarshal(raw, &got) != nil {
		return out
	}
	return got
}

// refKeys are the fields that hold ids (memoryReferenceSlots, the identity
// relation, links, body events); names, bodies, quotes and values are text
// and stay as written.
var refKeys = map[string]bool{
	"id": true, "ref": true, "at": true, "scene": true, "about": true, "owner": true, "to": true, "was": true,
	"seen_by": true, "actor": true, "target": true, "object": true, "place": true, "speaker": true, "listener": true,
	"from": true, "who": true, "same_as": true, "relation": true, "partners": true, "candidates": true,
	"unaware": true, "hide_from": true, "allow_to": true, "child": true, "learns": true,
}

func renameRefs(v any, ids map[string]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if refKeys[k] {
				x[k] = renameIDs(e, ids)
			} else {
				x[k] = renameRefs(e, ids)
			}
		}
	case []any:
		for i, e := range x {
			x[i] = renameRefs(e, ids)
		}
	}
	return v
}

// renameIDs rewrites an id field's value: one id or a list of ids.
func renameIDs(v any, ids map[string]string) any {
	switch x := v.(type) {
	case []any:
		for i, e := range x {
			x[i] = renameIDs(e, ids)
		}
	case map[string]any: // an object under an id-named key (none known): its own fields
		return renameRefs(x, ids)
	case string:
		// several sources of a basis, kept joined: each is an id
		if strings.Contains(x, ",") {
			return MapSource(x, func(p string) string { return renameIDs(p, ids).(string) })
		}
		if to, ok := ids[x]; ok {
			return to
		}
		// an effect of a memory: m3.2
		if i := strings.IndexByte(x, '.'); i > 0 {
			if to, ok := ids[x[:i]]; ok {
				return to + x[i:]
			}
		}
	}
	return v
}

// MergeParts joins the parts' answers in order: their items one after
// another, the days that passed added up.
func MergeParts(outs []Output) Output {
	var m Output
	for _, o := range outs {
		m.ElapsedDays += o.ElapsedDays
		if d := StoryDateOf(o.StoryDate); d != "" {
			m.StoryDate = d // the last date that reads: the one at the end of the reply
		}
		m.Entities = append(m.Entities, o.Entities...)
		m.Memories = append(m.Memories, o.Memories...)
		m.Scenes = append(m.Scenes, o.Scenes...)
		m.Links = append(m.Links, o.Links...)
		m.BodyEvents = append(m.BodyEvents, o.BodyEvents...)
	}
	return m
}
