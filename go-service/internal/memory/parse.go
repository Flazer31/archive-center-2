package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Parse reads a model response. The raw response is always kept by the
// caller. It takes the last complete response object in the text: an object
// holding one of the response arrays (the model may think or draft one
// first; text around it, such as code fences, and a complete object that is
// not a response, such as an item written on its own, are skipped; text
// inside the object is read as written). If that object does not decode as
// a whole, or the response is cut off, it keeps the complete items and
// notes what was lost. It never fills in missing content.
func Parse(raw string) (Output, []string) {
	var notes []string
	var obj []byte
	end := 0         // where the last complete response object ended
	unfinished := -1 // the first object after it that does not close
	readsTo := 0     // how far the unfinished object reads as JSON
	for i := 0; i < len(raw); {
		next := strings.IndexByte(raw[i:], '{')
		if next < 0 {
			break
		}
		i += next
		dec := json.NewDecoder(strings.NewReader(raw[i:]))
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			// read on inside it: a complete response may still follow (a
			// draft cut off before the answer)
			if unfinished < end && holdsArray(raw[i:]) {
				unfinished, readsTo = i, i+jsonPrefix(raw[i:])
			}
			i++
			continue
		}
		// an empty object written as a key's value that the unfinished
		// response still reads through (a basis written as {} inside a
		// cut-off item) is part of it, not a later response; any other
		// object, such as an answer ({} too) written again after an
		// unfinished draft, is taken as before
		inside := unfinished >= end && i+int(dec.InputOffset()) <= readsTo && emptyObject(v) &&
			strings.HasSuffix(strings.TrimRight(raw[:i], " \t\r\n"), ":")
		if isOutput(v) && !inside {
			obj, end = v, i+int(dec.InputOffset())
		}
		i += int(dec.InputOffset())
	}
	switch {
	case obj != nil && unfinished < end:
		var out Output
		var moved int
		obj, moved = moveMemories(obj)
		if moved > 0 {
			notes = append(notes, fmt.Sprintf("entities: %d memory item(s) were read as memories", moved))
		}
		obj, moved = moveLinks(obj)
		if moved > 0 {
			notes = append(notes, "a link written in memories was read as a link")
		}
		obj, moved = movePlaces(obj)
		if moved > 0 {
			notes = append(notes, fmt.Sprintf("memories: %d place(s) written beside roles were read as roles.place", moved))
		}
		err := json.Unmarshal(obj, &out)
		if strings.Contains(raw[end:], "{") {
			// e.g. an item written after the object closed
			notes = append(notes, "text after the response object was not read")
		}
		if err == nil {
			return out, append(notes, unknownFields(obj)...)
		}
		notes = append(notes, "response object does not match the shape: "+err.Error())
		out, notes = salvage(string(obj), notes)
		return out, append(notes, unknownFields(obj)...)
	case unfinished >= 0:
		// the last response is cut off (after a complete draft, if any)
		notes = append(notes, "response JSON is incomplete")
		return salvage(raw[unfinished:], notes)
	}
	return Output{}, []string{NoResponse}
}

// NoResponse is the note of an answer with no response object at all (only
// prose, or keys the contract does not have); "{}" is a response (nothing
// new this turn).
const NoResponse = "no JSON object in the response"

// Unreadable reports an answer in which Parse found no response object.
func Unreadable(notes []string) bool {
	return len(notes) == 1 && notes[0] == NoResponse
}

var responseArrays = []string{"entities", "memories", "scenes", "links", "body_events"}

// isOutput reports an object holding one of the response arrays, or an
// empty one.
func isOutput(v json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(v, &fields) != nil {
		return false
	}
	if len(fields) == 0 {
		return true // {}: every array is optional, nothing new this turn
	}
	if _, ok := fields["elapsed_days"]; ok {
		return true
	}
	for _, k := range responseArrays {
		if _, ok := fields[k]; ok {
			return true
		}
	}
	return false
}

func emptyObject(v json.RawMessage) bool {
	var fields map[string]json.RawMessage
	return json.Unmarshal(v, &fields) == nil && len(fields) == 0
}

// jsonPrefix reports how much of s reads as JSON from its start: up to its
// first syntax error, or all of it when it is only cut off.
func jsonPrefix(s string) int {
	dec := json.NewDecoder(strings.NewReader(s))
	for {
		if _, err := dec.Token(); err != nil {
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				return int(syntax.Offset) - 1
			}
			return len(s)
		}
	}
}

// arrayKey finds a response array opened as a key ("memories": [): a key
// comes right after "{" or ","; the same word inside a string value comes
// after an escaped quote (\") and is not a key. A key whose closing quote is
// missing ("memories:[) is still the key. Group 1 is the array name.
var arrayKey = regexp.MustCompile(`[{,]\s*"(entities|memories|scenes|links|body_events)"?\s*:\s*\[`)

// holdsArray reports text that opens one of the response arrays.
func holdsArray(s string) bool { return arrayKey.MatchString(s) }

func salvage(text string, notes []string) (Output, []string) {
	var out Output
	// Read complete top-level fields up to any cut-off. An unrelated item
	// shape error must not lose the turn's already readable day delta.
	dec := json.NewDecoder(strings.NewReader(text))
	if tok, err := dec.Token(); err == nil && tok == json.Delim('{') {
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				break
			}
			var value json.RawMessage
			if dec.Decode(&value) != nil {
				break
			}
			if key == "elapsed_days" {
				_ = json.Unmarshal(value, &out.ElapsedDays)
			}
		}
	}
	var misplacedLinks []Link
	kept, placed := 0, 0
	// a memory's place reads as its roles.place, as in a whole response
	placeOf := func(item json.RawMessage) json.RawMessage {
		var m map[string]json.RawMessage
		if json.Unmarshal(item, &m) != nil || !movePlace(m) {
			return item
		}
		placed++
		out, _ := json.Marshal(m)
		return out
	}
	for _, name := range responseArrays {
		var at []int
		for _, m := range arrayKey.FindAllStringSubmatchIndex(text, -1) {
			if text[m[2]:m[3]] == name {
				at = m
				break
			}
		}
		if at == nil {
			continue
		}
		// the array runs to the next response array at most
		end := len(text)
		for _, m := range arrayKey.FindAllStringIndex(text[at[1]:], -1) {
			end = at[1] + m[0]
			break
		}
		items, broken, cut := arrayItems(text[at[1]:end])
		if broken > 0 {
			notes = append(notes, fmt.Sprintf("%s: %d item(s) with broken JSON were skipped", name, broken))
		}
		if cut {
			notes = append(notes, fmt.Sprintf("%s: an incomplete item was dropped", name))
		}
		for _, item := range items {
			var dropped []string
			var ok bool
			switch name {
			case "entities":
				if memoryItem(item) {
					var m Memory
					dropped, ok = decodeItem(placeOf(item), &m)
					if ok {
						out.Memories = append(out.Memories, m)
						notes = append(notes, "entities: a memory item was read as a memory")
					}
					break
				}
				var e Entity
				dropped, ok = decodeItem(item, &e)
				if ok {
					out.Entities = append(out.Entities, e)
				}
			case "memories":
				if linkItem(item) {
					var l Link
					dropped, ok = decodeItem(item, &l)
					if ok {
						misplacedLinks = append(misplacedLinks, l)
					}
					break
				}
				var m Memory
				dropped, ok = decodeItem(placeOf(item), &m)
				if ok {
					out.Memories = append(out.Memories, m)
				}
			case "scenes":
				var s Scene
				dropped, ok = decodeItem(item, &s)
				if ok {
					out.Scenes = append(out.Scenes, s)
				}
			case "links":
				var l Link
				dropped, ok = decodeItem(item, &l)
				if ok {
					out.Links = append(out.Links, l)
				}
			case "body_events":
				var e BodyEvent
				dropped, ok = decodeItem(item, &e)
				if ok {
					out.BodyEvents = append(out.BodyEvents, e)
				}
			}
			if !ok {
				notes = append(notes, fmt.Sprintf("%s: a value that is not an item was not read: %s", name, string(item)))
				continue
			}
			if len(dropped) > 0 {
				notes = append(notes, fmt.Sprintf("%s: fields of the wrong shape were left out of an item: %s", name, strings.Join(dropped, ", ")))
			}
			kept++
		}
	}
	if len(misplacedLinks) > 0 {
		out.Links = append(out.Links, misplacedLinks...)
		notes = append(notes, "a link written in memories was read as a link")
	}
	if placed > 0 {
		notes = append(notes, fmt.Sprintf("memories: %d place(s) written beside roles were read as roles.place", placed))
	}
	notes = append(notes, fmt.Sprintf("salvaged %d complete items", kept))
	return out, notes
}

// linkItem recognizes a link written in memories, without interpreting or
// requiring any other field. A kind or body keeps the item a memory.
func linkItem(item json.RawMessage) bool {
	var fields map[string]json.RawMessage
	json.Unmarshal(item, &fields)
	return fields["from"] != nil && fields["rel"] != nil && fields["kind"] == nil && fields["body"] == nil
}

// moveLinks preserves links written in memories after the existing links,
// keeping the order within each array and all fields as written.
func moveLinks(obj []byte) ([]byte, int) {
	var fields map[string]json.RawMessage
	var memories, links []json.RawMessage
	if json.Unmarshal(obj, &fields) != nil || json.Unmarshal(fields["memories"], &memories) != nil {
		return obj, 0
	}
	var kept, moved []json.RawMessage
	for _, item := range memories {
		if linkItem(item) {
			moved = append(moved, item)
		} else {
			kept = append(kept, item)
		}
	}
	if len(moved) == 0 {
		return obj, 0
	}
	if l, ok := fields["links"]; ok && json.Unmarshal(l, &links) != nil {
		return obj, 0 // leave the existing shape-error handling to the caller
	}
	fields["memories"], _ = json.Marshal(append([]json.RawMessage{}, kept...))
	fields["links"], _ = json.Marshal(append(links, moved...))
	out, _ := json.Marshal(fields)
	return out, len(moved)
}

// movePlaces reads a place written on the memory itself ("place": "N12",
// "N12,N13" or a list) as its roles.place, as the contract places it
// (2026-10-30: 4 of 34 turns wrote it so and their events lost the place).
// An item that already has roles.place keeps it.
func movePlaces(obj []byte) ([]byte, int) {
	var fields map[string]json.RawMessage
	var memories []map[string]json.RawMessage
	if json.Unmarshal(obj, &fields) != nil || json.Unmarshal(fields["memories"], &memories) != nil {
		return obj, 0
	}
	moved := 0
	for _, m := range memories {
		if movePlace(m) {
			moved++
		}
	}
	if moved == 0 {
		return obj, 0
	}
	fields["memories"], _ = json.Marshal(memories)
	out, _ := json.Marshal(fields)
	return out, moved
}

// movePlace moves one memory's place to its roles.place (see movePlaces);
// it reports whether the item changed. salvage uses it for the complete
// items of a cut-off response, so an item reads the same either way.
func movePlace(m map[string]json.RawMessage) bool {
	raw, ok := m["place"]
	if !ok {
		return false
	}
	var places Who
	if json.Unmarshal(raw, &places) != nil {
		return false
	}
	var list []string
	for _, p := range places {
		for _, one := range strings.Split(p, ",") {
			if one = strings.TrimSpace(one); one != "" {
				list = append(list, one)
			}
		}
	}
	roles := map[string]json.RawMessage{}
	if r, ok := m["roles"]; ok && json.Unmarshal(r, &roles) != nil {
		return false
	}
	if roles == nil {
		roles = map[string]json.RawMessage{} // "roles": null is no roles
	}
	delete(m, "place")
	if _, has := roles["place"]; !has && len(list) > 0 {
		roles["place"], _ = json.Marshal(list)
		m["roles"], _ = json.Marshal(roles)
	}
	return true
}

// memoryItem reports an item that says it is a memory (kind event, fact
// or view, or a re-mention of a stored memory M12); written in the entities
// array it is still a memory.
func memoryItem(item json.RawMessage) bool {
	var v struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	}
	json.Unmarshal(item, &v)
	return v.Kind == "event" || v.Kind == "fact" || v.Kind == "view" || memoryRef.MatchString(v.Ref)
}

var memoryRef = regexp.MustCompile(`^M\d+(\.\d+)?$`)

// moveMemories moves memory items written in the entities array to the
// memories array (after the memories already there).
func moveMemories(obj []byte) ([]byte, int) {
	var fields map[string]json.RawMessage
	var entities, memories []json.RawMessage
	if json.Unmarshal(obj, &fields) != nil || json.Unmarshal(fields["entities"], &entities) != nil {
		return obj, 0
	}
	var names []json.RawMessage
	var moved []json.RawMessage
	for _, it := range entities {
		if memoryItem(it) {
			moved = append(moved, it)
		} else {
			names = append(names, it)
		}
	}
	if len(moved) == 0 {
		return obj, 0
	}
	if m, ok := fields["memories"]; ok && json.Unmarshal(m, &memories) != nil {
		return obj, 0 // memories is not an array: leave the shape error to the caller
	}
	fields["entities"], _ = json.Marshal(append([]json.RawMessage{}, names...))
	fields["memories"], _ = json.Marshal(append(memories, moved...))
	out, _ := json.Marshal(fields)
	return out, len(moved)
}

// itemStart finds where an item of a response array begins (a link may
// begin with "from").
var itemStart = regexp.MustCompile(`\{\s*"(id|ref|kind|from)"\s*:`)

// arrayItems reads the items of an array (text just after its '['). An
// item whose JSON is broken (an unclosed string, a stray quote) is skipped
// and reading goes on from the next item start; the items after it are
// kept. cut reports an item cut off at the end (nothing after it).
func arrayItems(text string) (items []json.RawMessage, broken int, cut bool) {
	for p := 0; p < len(text); {
		c := text[p]
		if c <= ' ' || c == ',' { // spaces, line ends and commas between items
			p++
			continue
		}
		if c == ']' {
			return
		}
		dec := json.NewDecoder(strings.NewReader(text[p:]))
		var item json.RawMessage
		if err := dec.Decode(&item); err == nil {
			items = append(items, item)
			p += int(dec.InputOffset())
			continue
		}
		next := itemStart.FindStringIndex(text[p+1:])
		if next == nil {
			cut = true
			return
		}
		broken++
		p += 1 + next[0]
	}
	return
}

// decodeItem reads an item; a field whose value has the wrong shape is left
// out (it stays in the raw response) instead of losing the whole item. It
// returns the fields left out, and false for a value that is not an object.
// In a list only the elements of the wrong shape are left out ("hide_from[1]"):
// the well-formed ones, such as a hide_from target, are kept.
func decodeItem(item json.RawMessage, v any) ([]string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil || fields == nil {
		return nil, false // not an object: there is no item to read
	}
	if json.Unmarshal(item, v) == nil {
		return nil, true
	}
	reads := func(k string, val json.RawMessage) bool {
		one, _ := json.Marshal(map[string]json.RawMessage{k: val})
		probe := reflect.New(reflect.TypeOf(v).Elem()).Interface()
		return json.Unmarshal(one, probe) == nil
	}
	var dropped []string
	for k, val := range fields {
		if reads(k, val) {
			continue
		}
		var list, kept []json.RawMessage
		var left []string
		// effects are referred to by position (m1.2): after a malformed one
		// the next would take its number, so they are left out with it
		positional := k == "effects"
		if json.Unmarshal(val, &list) == nil {
			for i, e := range list {
				one, _ := json.Marshal([]json.RawMessage{e})
				if !(positional && len(left) > 0) && reads(k, one) {
					kept = append(kept, e)
				} else {
					left = append(left, fmt.Sprintf("%s[%d]", k, i))
				}
			}
		}
		if len(kept) == 0 {
			dropped = append(dropped, k)
			delete(fields, k)
			continue
		}
		dropped = append(dropped, left...)
		fields[k], _ = json.Marshal(kept)
	}
	sort.Strings(dropped)
	rest, _ := json.Marshal(fields)
	// the failed first read may have set part of v
	reflect.ValueOf(v).Elem().Set(reflect.Zero(reflect.TypeOf(v).Elem()))
	json.Unmarshal(rest, v)
	return dropped, true
}

// unknownFields notes fields the contract does not have (they stay in the
// raw response; nothing is dropped from the items that were read).
func unknownFields(obj []byte) []string {
	var v any
	if json.Unmarshal(obj, &v) != nil {
		return nil
	}
	found := map[string]bool{}
	walkFields(v, reflect.TypeOf(Output{}), "", found)
	var notes []string
	for f := range found {
		notes = append(notes, "unknown field "+f)
	}
	sort.Strings(notes)
	return notes
}

func walkFields(v any, t reflect.Type, path string, found map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch x := v.(type) {
	case []any:
		if t.Kind() == reflect.Slice {
			for _, e := range x {
				walkFields(e, t.Elem(), path, found)
			}
		}
	case map[string]any:
		if t.Kind() != reflect.Struct {
			return
		}
		fields := jsonFields(t)
		for k, e := range x {
			ft, ok := fields[k]
			if !ok {
				found[strings.TrimPrefix(path+"."+k, ".")] = true
				continue
			}
			walkFields(e, ft, path+"."+k, found)
		}
	}
}

// jsonFields maps a struct's json names (embedded structs included) to
// their types.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			for k, v := range jsonFields(f.Type) {
				out[k] = v
			}
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			out[name] = f.Type
		}
	}
	return out
}
