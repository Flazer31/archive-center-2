package memory

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Known is the stored memory a generation can read: current items of the
// chat (current jobs only), each with the turn it came from.
type Known struct {
	Names     []KnownName
	Relations []KnownRelation
	Memories  []KnownMemory
	Awareness []KnownAwareness
	Links     []KnownLink
	Scenes    []KnownScene // in story order: by turn, then as stored
}

type KnownName struct {
	ID, Name, Type, Q string
	Turn              int
	// Body is the person's latest recorded profile ("female, 악마"), shown
	// so the model does not record it again (1.1)
	Body string
}

type KnownRelation struct {
	ID, Name, SameAs, How, Q string
	Turn                     int
}

type KnownMemory struct {
	ID   string
	Turn int
	Job  int64  // the generation job that wrote it
	Date string // the turn's story date (chat variables), when known
	Memory
	// BodyWords is the stored, name-independent Words(Body) projection.
	// nil means a legacy/in-memory record; names are still filtered per read.
	BodyWords []string `json:"-"`
	// Changed is set by a selection (and a delivery) for a fact later records
	// changed in part (PartlyChanged): the slots they changed, shown on its
	// line by RenderKnown and the delivery line.
	Changed []string `json:"-"`
}

type KnownAwareness struct {
	Target, Kind string
	Turn         int
	Knows
}

type KnownLink struct {
	ID, From, Rel, To string
	Turn              int
}

type KnownScene struct {
	ID, Change string
	Turn       int
}

// KnownEffect is one effect of an event shown without its event: the
// current value it set.
type KnownEffect struct {
	Ref  string // M<n>.<k>
	Turn int
	// Subject is whose value it is, when About names a record (M12) instead
	// of a name
	Subject string
	Effect
}

// Superseded returns the memories and effects a later record replaced: the
// targets of every was (a memory's was, or an effect's was); the effects of
// a replaced memory are replaced with it. A record nobody points to is the
// end of its chain.
//
// A was replaces a value, not everything the record it names holds (a
// turn-1 fact of a person's age, money and place was named by later changes
// of the money, the place and a skill, and the age nobody changed was never
// read again; an event that introduced a person was named
// by a later change of the money it had cost):
//   - a fact that records of two or more other slots name, and none of its
//     own slot, stays: it held several values and each record changed its
//     own. It is partly changed (PartlyChanged lists the slots). Named under
//     one other slot it is replaced as before: the model often writes the
//     same value's slot differently ("외형/상태" then "상태/표정"; keeping
//     those brought a replaced fact or its source event back in 6 of 21
//     stored scenes);
//   - an event named by an effect stays: the values it set for that slot are
//     replaced (all the values it set, when none has that slot).
func Superseded(k Known) map[string]bool {
	out, _ := replaced(k)
	return out
}

// PartlyChanged lists, for a fact that later records changed in part (they
// name it by was under two or more other slots), the slots they changed, in
// stored order.
func PartlyChanged(k Known) map[string][]string {
	_, partly := replaced(k)
	return partly
}

func replaced(k Known) (map[string]bool, map[string][]string) {
	out, partly := map[string]bool{}, map[string][]string{}
	byID := map[string]*KnownMemory{}
	for i := range k.Memories {
		byID[k.Memories[i].ID] = &k.Memories[i]
	}
	// otherSlot: the named record is a fact of a slot other than the one
	// the naming record changes
	otherSlot := func(t *KnownMemory, slot string) bool {
		return t != nil && t.Kind == "fact" && t.Slot != "" && slot != "" && t.Slot != slot
	}
	part := func(id, slot string) {
		for _, s := range partly[id] {
			if s == slot {
				return
			}
		}
		partly[id] = append(partly[id], slot)
	}
	for _, m := range k.Memories {
		for _, w := range m.Was {
			if m.Kind == "fact" && otherSlot(byID[w], m.Slot) {
				part(w, m.Slot)
				continue
			}
			out[w] = true
		}
		for _, e := range m.Effects {
			for _, w := range e.Was {
				t := byID[w]
				switch {
				case otherSlot(t, e.Slot):
					part(w, e.Slot)
				case t != nil && t.Kind == "event":
					same := false
					for i, te := range t.Effects {
						if te.Slot == e.Slot {
							out[w+"."+strconv.Itoa(i+1)], same = true, true
						}
					}
					for i := range t.Effects {
						if !same {
							out[w+"."+strconv.Itoa(i+1)] = true
						}
					}
				default:
					out[w] = true
				}
			}
		}
	}
	// a fact replaced whole, or named under one other slot only, is replaced
	for id, slots := range partly {
		if out[id] || len(slots) < 2 {
			delete(partly, id)
			out[id] = true
		}
	}
	for _, m := range k.Memories {
		if out[m.ID] {
			for i := range m.Effects {
				out[m.ID+"."+strconv.Itoa(i+1)] = true
			}
		}
	}
	return out, partly
}

// obligationMode reports a fact that is an obligation, question or shared
// goal; openObligation reports one still open (an empty status is open: not
// recorded as closed).
func obligationMode(m Memory) bool {
	return m.Kind == "fact" && (m.Mode == "의무" || m.Mode == "질문·위험" || m.Mode == "공동 목표")
}

func openObligation(m Memory) bool {
	if !obligationMode(m) {
		return false
	}
	switch m.Status {
	case "fulfilled", "missed", "cancelled", "impossible", "refused", "resolved":
		return false
	}
	return true
}

// CurrentFacts lists the facts eligible for current-state reading. An
// explicitly recalled old value still uses Select's separate recall path.
// Returned records keep their stored text; effects decide eligibility only.
func CurrentFacts(k Known) []KnownMemory {
	sup := Superseded(k)
	status := map[string]string{}
	byTurn := append([]KnownMemory(nil), k.Memories...)
	sort.SliceStable(byTurn, func(i, j int) bool { return byTurn[i].Turn < byTurn[j].Turn })
	for _, m := range byTurn {
		for _, e := range m.Effects {
			if e.Status != "" {
				status[e.About] = e.Status
			}
		}
	}
	var out []KnownMemory
	for _, m := range k.Memories {
		now := m.Memory
		if st, ok := status[m.ID]; ok {
			now.Status = st
		}
		if m.Kind == "fact" && !sup[m.ID] && (!obligationMode(now) || openObligation(now)) {
			out = append(out, m)
		}
	}
	return out
}

// Selection is the part of Known shown to the model for one turn.
type Selection struct {
	Names     []KnownName
	Relations []KnownRelation
	Memories  []KnownMemory
	Effects   []KnownEffect // current values of events not shown whole
	Awareness []KnownAwareness
	Links     []KnownLink
	Scene     string // the open scene, if any
}

// Select picks what the model needs to tell new from known for a turn.
// The starting points are the stored names written in the turn text (and
// names linked to them by a stated identity). For them it shows current
// facts and open obligations and the current values events set. Events and
// views of the open scene are shown. An earlier value, a closed obligation,
// or an event or view outside the scene is shown when the turn recalls it
// (shares words that are uncommon in the chat: earlier holds the text of the
// turns before this one). Shown records bring the records they point to;
// earlier values they replaced are not brought. There is no count limit.
// Generation reads with SelectWithin instead.
func Select(k Known, text string, earlier []string) Selection {
	sel, _ := selectKnown(k, text, earlier, nil, nil, true, false)
	return sel
}

// What a record is shown for, from what goes last when the reading range
// is over its ceiling to what always stays.
const (
	shownKept   = iota // an open obligation, a record kept from someone (hide_from, unaware)
	shownScene         // in the open scene
	shownByName        // a current fact of a name in the turn
	shownRecall        // recalled by the turn's words
	shownLinked        // pointed to by a shown record or a meaning link
)

// SelectWithin is what a generation call reads: Select under a ceiling on
// the rendered reading range (chars; 0 = none; the critic setting
// known_chars). Under the ceiling it is Select exactly. Over it, a fact
// first stops bringing the event it came from (its "at" stays on its line
// as an id; the event is still shown when it is in the open scene or
// recalled; 11-01: the source events were 17% of what the model read).
// Still over, whole records are left out, linked ones first, then recalled
// ones, then facts of named people, then records of the open scene (a
// scene that never closes would otherwise hold everything), the oldest
// first in each; open obligations, records kept from someone (hide_from,
// unaware: their protection), names and current values always stay. A
// record left out is only not shown to this call: it stays stored. It
// returns how many records were left out, against Select.
// (11-09: leaving the source events out under the ceiling too lost 4 of
// 41 references the critic made over 21 stored scenes, so a chat under its
// ceiling reads as before.)
func SelectWithin(k Known, text string, earlier []string, max int) (Selection, int) {
	all, _ := selectKnown(k, text, earlier, nil, nil, true, true)
	if max <= 0 {
		return all, 0
	}
	sup := Superseded(k)
	fits := func(sel Selection) bool { return len([]rune(RenderKnown(sel, sup))) <= max }
	if fits(all) {
		return all, 0
	}
	full, why := selectKnown(k, text, earlier, nil, nil, false, true)
	if fits(full) {
		return full, len(all.Memories) - len(full.Memories)
	}
	// what always stays is kept even when the record that brought it goes.
	// So is an event that leaving out would not make shorter: left out, an
	// event still gives the current values it set for a shown name (and the
	// status it gave a shown obligation) on lines of their own, each with
	// its turn; when those are as long as the event shown whole, the range
	// would grow as more is left out (one record out fitted, two did not),
	// so it stays.
	shown := map[string]bool{}
	for _, n := range full.Names {
		shown[n.ID] = true
	}
	for _, m := range full.Memories {
		shown[m.ID] = true
	}
	noShorter := func(m KnownMemory) bool {
		var alone Selection
		for i, e := range m.Effects {
			ref := m.ID + "." + strconv.Itoa(i+1)
			about := strings.SplitN(e.About, ".", 2)[0] // a name, or a shown record whose value it changes
			if !sup[ref] && (e.Value != "" || e.Status != "") && shown[about] {
				alone.Effects = append(alone.Effects, KnownEffect{Ref: ref, Turn: m.Turn, Effect: e})
			}
		}
		if len(alone.Effects) == 0 {
			return false
		}
		whole := Selection{Memories: []KnownMemory{m}}
		return len([]rune(RenderKnown(alone, sup))) >= len([]rune(RenderKnown(whole, sup)))
	}
	pin := map[string]bool{}
	var cand []KnownMemory
	for _, m := range full.Memories {
		if why[m.ID] == shownKept || (m.Kind == "event" && noShorter(m)) {
			pin[m.ID] = true
		} else {
			cand = append(cand, m)
		}
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if why[cand[i].ID] != why[cand[j].ID] {
			return why[cand[i].ID] > why[cand[j].ID]
		}
		return cand[i].Turn < cand[j].Turn
	})
	without := func(n int) Selection {
		exclude := map[string]bool{}
		for _, m := range cand[:n] {
			exclude[m.ID] = true
		}
		sel, _ := selectKnown(k, text, earlier, exclude, pin, false, true)
		return sel
	}
	// the fewest records, in that order, whose leaving out brings the range
	// under the ceiling, each count measured whole (the names, awareness
	// and values a record brought go with it): found by halving, so a long
	// chat is measured some ten times, not once per record. Leaving out more
	// is not always shorter (an event left out shows its values on lines
	// of their own), so a count that fits is taken wherever it is found, and
	// with none, the shortest measured
	if len(cand) == 0 { // what always stays is over by itself
		return full, len(all.Memories) - len(full.Memories)
	}
	var fit, short Selection
	found, shortSize := false, 0
	lo, hi := 1, len(cand)
	for lo <= hi {
		mid := (lo + hi) / 2
		sel := without(mid)
		size := len([]rune(RenderKnown(sel, sup)))
		if shortSize == 0 || size < shortSize {
			short, shortSize = sel, size
		}
		if size <= max {
			fit, found = sel, true
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	if !found {
		fit = short
	}
	return fit, len(all.Memories) - len(fit.Memories)
}

// selectKnown is Select without the records in exclude and with the ones in
// pin, following a fact's "at" when followAt, and with the awareness of
// what a shown record cites when cite (a generation call); it also gives
// what each shown record is shown for.
func selectKnown(k Known, text string, earlier []string, exclude, pin map[string]bool, followAt, cite bool) (Selection, map[string]int) {
	var sel Selection
	why := map[string]int{}
	ids := map[string]bool{}
	for _, n := range k.Names {
		if n.Name != "" && strings.Contains(text, n.Name) {
			ids[n.ID] = true
		}
	}
	// names linked by a stated identity relation
	for changed := true; changed; {
		changed = false
		for _, r := range k.Relations {
			if ids[r.Name] != ids[r.SameAs] {
				ids[r.Name], ids[r.SameAs] = true, true
				changed = true
			}
		}
	}
	for _, r := range k.Relations {
		if ids[r.Name] || ids[r.SameAs] {
			ids[r.ID] = true
		}
	}
	sup, partly := replaced(k)
	sceneID, sceneStart := openScene(k.Scenes)
	// a record names its scene when the model wrote one; otherwise it belongs
	// to the scene open when it was written
	inScene := func(m KnownMemory) bool {
		if sceneID == "" {
			return false
		}
		if m.Scene != "" {
			return m.Scene == sceneID
		}
		return m.Turn >= sceneStart
	}
	var names []string
	for _, n := range k.Names {
		names = append(names, n.Name)
	}
	byID := map[string]KnownMemory{}
	for _, m := range k.Memories {
		byID[m.ID] = m
	}
	// whose value an effect changes: an effect may name the record it
	// changes (M12, or an effect M12.1) instead of the name; that record's
	// subject is followed (a view's is its owner), up to a record already
	// passed
	subjects := map[string]string{}
	subject := func(about string) string {
		passed := map[string]bool{}
		for !passed[about] {
			if s, ok := subjects[about]; ok {
				about = s
				break
			}
			passed[about] = true
			p := strings.SplitN(about, ".", 2)
			m, ok := byID[p[0]]
			if !ok {
				break
			}
			next := m.About
			if len(p) == 2 {
				i, err := strconv.Atoi(p[1])
				if err != nil || i < 1 || i > len(m.Effects) {
					break
				}
				next = m.Effects[i-1].About
			} else if m.Kind == "view" && m.Owner != "" {
				next = m.Owner
			}
			if next == "" {
				break
			}
			about = next
		}
		// every record on the way has the same end
		for p := range passed {
			subjects[p] = about
		}
		return about
	}
	involves := func(m Memory) bool {
		if ids[m.About] || ids[m.Owner] {
			return true
		}
		for _, s := range m.To {
			if ids[s] {
				return true
			}
		}
		for _, s := range m.SeenBy {
			if ids[s] {
				return true
			}
		}
		if m.Roles != nil {
			for _, l := range [][]string{m.Roles.Actor, m.Roles.Target, m.Roles.Object, m.Roles.Place, m.Roles.Speaker, m.Roles.Listener} {
				for _, s := range l {
					if ids[s] {
						return true
					}
				}
			}
		}
		for _, e := range m.Effects {
			if ids[e.About] || ids[subject(e.About)] {
				return true
			}
		}
		return false
	}
	// how common each word is: in how many turns of the chat (this one and
	// the earlier ones; a word the chat uses every turn recalls nothing,
	// KNOWN-DIAG-73), and the words of the records that replaced each earlier
	// one
	words := map[string][]string{}
	var all []string
	for _, m := range k.Memories {
		ws := m.BodyWords
		if ws == nil {
			ws = Words(m.Body)
		}
		for _, w := range ws {
			if !isName(w, names) {
				words[m.ID] = append(words[m.ID], w)
			}
		}
		all = append(all, words[m.ID]...)
	}
	// which of the records' words are in this text: the text is read once
	// for all of them, not once per word (CAND-91: per-word reading of a
	// long request took 91% of the time)
	inText := map[string]int{}
	countTurns(all, []string{text}, inText)
	// the earlier turns are read once each, for all the words of the stored
	// records that are in this turn together (not each word through every
	// turn): a word is in a turn's text where a turn position starts it
	var df map[string]int
	turnsWith := func(w string) int {
		if df == nil {
			df = map[string]int{}
			var asked []string
			for _, ws := range words {
				for _, x := range ws {
					if _, ok := df[x]; !ok && inText[x] > 0 {
						df[x] = 0
						asked = append(asked, x)
					}
				}
			}
			countTurns(asked, earlier, df)
		}
		return 1 + df[w] // this turn and the earlier ones that have the word
	}
	later := map[string]map[string]bool{}
	for _, m := range k.Memories {
		var replaced []string
		replaced = append(replaced, m.Was...)
		for _, e := range m.Effects {
			replaced = append(replaced, e.Was...)
		}
		for _, w := range replaced {
			id := strings.SplitN(w, ".", 2)[0]
			if later[id] == nil {
				later[id] = map[string]bool{}
			}
			for _, x := range words[m.ID] {
				later[id][x] = true
			}
		}
	}
	recalls := func(m KnownMemory) bool {
		score := 0.0
		for _, w := range words[m.ID] {
			// an earlier value is recalled by what only it has, not by what
			// the current value repeats
			if later[m.ID][w] || inText[w] == 0 {
				continue
			}
			score += 1 / float64(turnsWith(w))
		}
		return score >= 1
	}
	currentFacts := map[string]bool{}
	for _, m := range CurrentFacts(k) {
		currentFacts[m.ID] = true
	}
	// an obligation still open, and a record someone must not be told of
	// or does not know (its protection), stay whatever brought them
	guarded := map[string]bool{}
	for _, a := range k.Awareness {
		if a.Kind == "hide_from" || a.Kind == "unaware" {
			guarded[strings.SplitN(a.Target, ".", 2)[0]] = true
		}
	}
	openNow := func(m KnownMemory) bool {
		return m.Kind == "fact" && obligationMode(m.Memory) && currentFacts[m.ID]
	}
	picked := map[string]bool{}
	for _, m := range k.Memories {
		current := !sup[m.ID]
		var show bool
		switch m.Kind {
		case "fact":
			show = currentFacts[m.ID] && (involves(m.Memory) || inScene(m))
		case "view":
			show = current && inScene(m)
		default:
			show = inScene(m)
		}
		if exclude[m.ID] {
			continue
		}
		switch {
		case show && (openNow(m) || guarded[m.ID]):
			picked[m.ID], why[m.ID] = true, shownKept
		case show && inScene(m):
			picked[m.ID], why[m.ID] = true, shownScene
		case show:
			picked[m.ID], why[m.ID] = true, shownByName
		case recalls(m):
			picked[m.ID], why[m.ID] = true, shownRecall
			if openNow(m) || guarded[m.ID] {
				why[m.ID] = shownKept
			}
		}
	}
	// records the shown ones point to (where a fact came from when followAt,
	// what a view is about, a basis, the obligation an effect closes),
	// followed until nothing is added, and the
	// other end of a meaning link from a record shown for itself (one link,
	// not chains of links). The result does not depend on the stored order.
	direct := map[string]bool{}
	for id := range picked {
		direct[id] = true
	}
	linked := map[string]bool{}
	for changed := true; changed; {
		changed = false
		add := func(ref string) {
			id := strings.SplitN(ref, ".", 2)[0]
			if m, ok := byID[id]; ok && !picked[id] && !exclude[id] {
				picked[id], why[id] = true, shownLinked
				if openNow(m) || guarded[id] {
					why[id] = shownKept
				}
				changed = true
			}
		}
		for id := range picked {
			m := byID[id].Memory
			if followAt {
				add(m.At)
			}
			add(m.About)
			if m.Basis != nil {
				add(m.Basis.Ref)
			}
			for _, e := range m.Effects {
				add(e.About)
			}
		}
		for _, l := range k.Links {
			if linked[l.ID] {
				continue
			}
			from, to := strings.SplitN(l.From, ".", 2)[0], strings.SplitN(l.To, ".", 2)[0]
			if direct[from] || direct[to] {
				linked[l.ID] = true
				changed = true
				add(l.From)
				add(l.To)
			}
		}
	}
	for id := range pin {
		if _, ok := byID[id]; ok && !picked[id] {
			picked[id], why[id] = true, shownKept
		}
	}
	source := map[string]bool{} // events whose effects are shown alone
	for _, m := range k.Memories {
		if picked[m.ID] {
			m.Changed = partly[m.ID] // shown with the slots later records changed
			sel.Memories = append(sel.Memories, m)
			ids[m.ID] = true
			continue
		}
		// an event not shown whole still gives the current values it set and
		// the status it gave a shown obligation; its awareness comes with them
		for i, e := range m.Effects {
			ref := m.ID + "." + strconv.Itoa(i+1)
			if sup[ref] || !(((ids[e.About] || ids[subject(e.About)]) && e.Value != "") || (picked[e.About] && e.Status != "")) {
				continue
			}
			ke := KnownEffect{Ref: ref, Turn: m.Turn, Effect: e}
			if s := subject(e.About); s != e.About {
				ke.Subject = s
			}
			sel.Effects = append(sel.Effects, ke)
			source[m.ID] = true
		}
	}
	for _, l := range k.Links {
		if linked[l.ID] {
			sel.Links = append(sel.Links, l)
			ids[l.ID] = true
		}
	}
	// names the shown records use, so every id shown has its name
	for _, m := range sel.Memories {
		for _, r := range refsOf(m.Memory) {
			ids[r] = true
		}
	}
	for _, e := range sel.Effects {
		ids[e.About], ids[e.Subject] = true, true
	}
	// awareness on what is shown, with the names of who knows or must not
	// be told; for a generation call also on a record a shown one cites only
	// by id (its basis, an earlier value it replaced), so a shown record
	// never loses the protection of what it points to
	cited := map[string]bool{}
	if cite {
		for _, m := range sel.Memories {
			refs := append([]string{}, m.Was...)
			for _, e := range m.Effects {
				refs = append(refs, e.Was...)
			}
			if m.Basis != nil {
				refs = append(refs, m.Basis.Ref)
			}
			for _, r := range refs {
				cited[strings.SplitN(r, ".", 2)[0]] = true
			}
		}
		// a value shown on its own line keeps the protection of the
		// earlier value it replaced
		for _, e := range sel.Effects {
			for _, r := range e.Was {
				cited[strings.SplitN(r, ".", 2)[0]] = true
			}
		}
	}
	for _, a := range k.Awareness {
		if t := strings.SplitN(a.Target, ".", 2)[0]; ids[t] || source[t] || cited[t] {
			sel.Awareness = append(sel.Awareness, a)
		}
	}
	for _, a := range sel.Awareness {
		ids[a.Who], ids[a.From] = true, true
	}
	// a name's quote is what tells it from another entity of the same name;
	// a name no other entity has is shown without it
	sameName := map[string]int{}
	for _, n := range k.Names {
		sameName[n.Name]++
	}
	for _, n := range k.Names {
		if ids[n.ID] {
			if sameName[n.Name] < 2 {
				n.Q = ""
			}
			sel.Names = append(sel.Names, n)
		}
	}
	for _, r := range k.Relations {
		if ids[r.ID] {
			sel.Relations = append(sel.Relations, r)
		}
	}
	sel.Scene = sceneID
	return sel, why
}

// countTurns adds to n[w], for each word, the number of texts it is in. The
// words make one matching automaton (Aho-Corasick over bytes: a UTF-8 word
// found in UTF-8 text is found at character boundaries), and each text is
// read once, byte by byte, whatever it repeats.
func countTurns(words []string, texts []string, n map[string]int) {
	type node struct {
		next map[byte]*node
		fail *node
		word string // a word ending here
		out  *node  // the nearest shorter word ending here (by fail links)
		seen int    // the last text that counted this node's word
	}
	newNode := func() *node { return &node{next: map[byte]*node{}, seen: -1} }
	counted := map[string]bool{}
	root := newNode()
	for _, w := range words {
		at := root
		for i := 0; i < len(w); i++ {
			nx := at.next[w[i]]
			if nx == nil {
				nx = newNode()
				at.next[w[i]] = nx
			}
			at = nx
		}
		at.word = w
	}
	// fail links, breadth first: the longest proper suffix that is a prefix
	queue := []*node{}
	for _, c := range root.next {
		c.fail = root
		queue = append(queue, c)
	}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for b, c := range at.next {
			f := at.fail
			for f != root && f.next[b] == nil {
				f = f.fail
			}
			if g := f.next[b]; g != nil && g != c {
				c.fail = g
			} else {
				c.fail = root
			}
			if c.fail.word != "" {
				c.out = c.fail
			} else {
				c.out = c.fail.out
			}
			queue = append(queue, c)
		}
	}
	distinct := 0
	for _, w := range words {
		if !counted[w] {
			counted[w] = true
			distinct++
		}
	}
	for ti, t := range texts {
		at, found := root, 0
		for i := 0; i < len(t) && found < distinct; i++ { // a text with every word is done
			for at != root && at.next[t[i]] == nil {
				at = at.fail
			}
			if nx := at.next[t[i]]; nx != nil {
				at = nx
			}
			// every word ending here, once per text; a word already counted
			// in this text has counted the shorter ones after it too
			for m := at; m != nil; m = m.out {
				if m.word == "" {
					continue
				}
				if m.seen == ti {
					break
				}
				m.seen = ti
				n[m.word]++
				found++
			}
		}
	}
}

// openScene reads the scene marks in story order: the scene opened last and
// not closed since, and the turn it opened at. A continue with no open
// scene starts one there.
func openScene(marks []KnownScene) (string, int) {
	ms := append([]KnownScene(nil), marks...)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Turn < ms[j].Turn })
	id, start := "", 0
	for _, sc := range ms {
		switch sc.Change {
		case "open":
			id, start = sc.ID, sc.Turn
		case "continue":
			if id == "" {
				id, start = sc.ID, sc.Turn
			}
		case "close":
			if sc.ID == id {
				id = ""
			}
		}
	}
	return id, start
}

// refsOf lists the ids a memory refers to (names and records).
func refsOf(m Memory) []string {
	out := []string{m.About, m.Owner, m.At}
	out = append(out, m.To...)
	out = append(out, m.SeenBy...)
	if m.Roles != nil {
		for _, l := range [][]string{m.Roles.Actor, m.Roles.Target, m.Roles.Object, m.Roles.Place, m.Roles.Speaker, m.Roles.Listener} {
			out = append(out, l...)
		}
	}
	for _, e := range m.Effects {
		out = append(out, e.About)
	}
	if m.Basis != nil {
		out = append(out, SourceParts(m.Basis.From)...)
	}
	return out
}

var wordPattern = regexp.MustCompile(`[\p{L}\p{N}]+`)

// particles are Korean postpositions stripped from a word's end to get the
// word itself ("거지일" stays, "거지를" -> "거지").
var particles = []string{"에게서", "으로서", "으로써", "이라는", "에서는", "에게는", "까지", "부터", "에서", "에게", "한테", "으로", "보다", "처럼", "마다", "이나", "라는", "은", "는", "이", "가", "을", "를", "에", "의", "와", "과", "도", "만", "로", "께"}

func stem(w string) string {
	for _, p := range particles {
		if strings.HasSuffix(w, p) && len([]rune(w))-len([]rune(p)) >= 2 {
			return strings.TrimSuffix(w, p)
		}
	}
	return w
}

// contentWords are the distinct words of a record's body, without their
// particles. Names are left out (a name is a starting point above, not a
// recall).
func contentWords(body string, names []string) []string {
	var out []string
	for _, w := range Words(body) {
		if !isName(w, names) {
			out = append(out, w)
		}
	}
	return out
}

func isName(s string, names []string) bool {
	for _, n := range names {
		if n != "" && (strings.Contains(s, n) || strings.Contains(n, s)) {
			return true
		}
	}
	return false
}

// When is when a memory happened as it is read (C12: every memory has a
// date, so later features read dates without error): Text is the time and
// From names where it came from — "time" (its own time), "at" (the time of
// the event it comes from), "turn" (the recorded date of the turn that
// event or the memory itself was written in). Both are empty only when no
// date is known at all (a turn whose passage of days is not known, made
// before 0.0.4).
type When struct {
	Text string `json:"when,omitempty"`
	From string `json:"when_from,omitempty"`
}

// WhenOf gives each memory its When by id: its own time, else the time of
// the event it comes from (at), else that event's recorded date, else its
// own recorded date. Only the memories given are read.
func WhenOf(mems []KnownMemory) map[string]When {
	byID := make(map[string]KnownMemory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}
	out := make(map[string]When, len(mems))
	for _, m := range mems {
		switch {
		case m.Time != "":
			out[m.ID] = When{m.Time, "time"}
		case m.At != "" && byID[m.At].Time != "":
			out[m.ID] = When{byID[m.At].Time, "at"}
		case m.At != "" && byID[m.At].Date != "":
			out[m.ID] = When{byID[m.At].Date, "turn"}
		case m.Date != "":
			out[m.ID] = When{m.Date, "turn"}
		}
	}
	return out
}
