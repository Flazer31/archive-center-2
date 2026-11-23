package memory

import (
	"fmt"
	"strings"
)

// IdentityMode is the fact mode of what someone or something is and stays
// (true nature, group, role, an alias and whose it is). A state does not
// replace it (1.3).
const IdentityMode = "정체"

// StateSlot is the slot of a subject's current state (1.3). Only states
// written under it are replaced by the next one; states of other slots
// (every state before 1.3) are left as they are. Since 1.4 a state is
// written per aspect, "지금/위치", "지금/몸", "지금/차림·소지": a new state
// replaces the subject's earlier state of the same aspect and an earlier
// whole "지금" state; a whole "지금" state replaces every one (1.4: a whole
// state replaced together lost what stayed, L-085's usual clothes).
const StateSlot = "지금"

// stateSlot tells a current-state slot, whole or one aspect.
func stateSlot(s string) bool {
	return s == StateSlot || strings.HasPrefix(s, StateSlot+"/")
}

// replacesState tells whether a new state under slot replaces an earlier
// one under old (both state slots).
func replacesState(slot, old string) bool {
	return slot == StateSlot || old == StateSlot || slot == old
}

// PlaceSlot is the slot of an identity fact saying where a place is (1.3).
const PlaceSlot = "위치"

// Fold reads the lists of their own (boundaries, closed) into the items the
// store already keeps, and names the earlier current state a new one
// replaces (1.3). Nothing is refused: an entry that names nothing is noted
// and the rest of the answer is stored as it is.
//
//   - a boundary on an event of this answer that says the secret (what) is
//     a new identity fact with the awareness, the event recorded at it;
//   - a boundary on another item of this answer is that item's awareness; on a
//     stored item it is an awareness update of it ({ref}); with no item it is
//     a new identity fact that says the hidden thing, with the awareness;
//   - a closed record is a status effect on the first event of the answer;
//     with no event, an event that says how it ended carries it;
//   - a new state under StateSlot replaces the current StateSlot state of the
//     same subject in k (was).
func Fold(out Output, k Known) (Output, []string) {
	var notes []string
	// the caller's answer is left as it was read
	out.Memories = append([]Memory(nil), out.Memories...)
	out.Entities = append([]Entity(nil), out.Entities...)
	if len(out.Boundaries) > 0 {
		at := map[string]int{}
		for i, m := range out.Memories {
			if m.ID != "" {
				at[m.ID] = i
			}
		}
		// a name of this answer: its awareness goes on the entity, so a name
		// written as another's alias is protected on that relation as the
		// store keeps it (11-17)
		ent := map[string]int{}
		for i, e := range out.Entities {
			if e.ID != "" {
				ent[e.ID] = i
			}
		}
		protectName := func(id string, a Awareness) bool {
			i, ok := ent[id]
			if !ok {
				return false
			}
			e := &out.Entities[i]
			e.Knows = append(append([]Knows{}, e.Knows...), a.Knows...)
			e.Unaware = append(append(Who{}, e.Unaware...), a.Unaware...)
			e.HideFrom = append(append(Who{}, e.HideFrom...), a.HideFrom...)
			return true
		}
		// protectItem gives an item of this answer the awareness
		protectItem := func(i int, a Awareness) {
			m := &out.Memories[i]
			m.Knows = append(append([]Knows{}, m.Knows...), a.Knows...)
			m.Unaware = append(append(Who{}, m.Unaware...), a.Unaware...)
			m.HideFrom = append(append(Who{}, m.HideFrom...), a.HideFrom...)
		}
		for n, b := range out.Boundaries {
			a := Awareness{Unaware: b.Unaware, HideFrom: b.HideFrom}
			for _, w := range b.Knows {
				a.Knows = append(a.Knows, Knows{Who: w, Q: b.Q})
			}
			i, here := at[b.On]
			switch {
			case b.On != "" && here && b.What != "" && out.Memories[i].Kind == "event" && out.Memories[i].At == "":
				// the secret is the sentence that says it, not the whole event:
				// a fact of its own keeps the awareness, and the event is
				// recorded at it, so its line keeps that protection (11-17)
				id := fmt.Sprintf("bd%d", n+1)
				out.Memories[i].At = id
				out.Memories = append(out.Memories, Memory{ID: id, Kind: "fact", Mode: IdentityMode,
					About: b.Whose, Body: b.What, Q: b.Q, Awareness: a})
			case b.On != "" && here:
				protectItem(i, a)
			case b.On != "":
				// a stored item, a name or an identity relation: an awareness
				// update of it (no quote: not counted as a re-mention)
				if len(a.Knows)+len(a.Unaware)+len(a.HideFrom) == 0 {
					notes = append(notes, fmt.Sprintf("boundaries[%d]: names nobody", n))
					break
				}
				if !protectName(b.On, a) {
					out.Memories = append(out.Memories, Memory{Ref: b.On, Awareness: a})
				}
			case b.What != "":
				out.Memories = append(out.Memories, Memory{ID: fmt.Sprintf("bd%d", n+1), Kind: "fact", Mode: IdentityMode,
					About: b.Whose, Body: b.What, Q: b.Q, Awareness: a})
			default:
				notes = append(notes, fmt.Sprintf("boundaries[%d]: names no item and says nothing", n))
			}
		}
		out.Boundaries = nil
	}
	if len(out.Closed) > 0 {
		carrier := -1
		for i, m := range out.Memories {
			if m.Kind == "event" && m.ID != "" {
				carrier = i
				break
			}
		}
		for n, c := range out.Closed {
			if c.Ref == "" || c.Status == "" {
				notes = append(notes, fmt.Sprintf("closed[%d]: no record or status", n))
				continue
			}
			e := Effect{About: c.Ref, Status: c.Status}
			if carrier >= 0 {
				out.Memories[carrier].Effects = append(append([]Effect{}, out.Memories[carrier].Effects...), e)
				continue
			}
			body := c.How
			if body == "" {
				body = c.Q
			}
			if body == "" {
				notes = append(notes, fmt.Sprintf("closed[%d]: no event to carry it", n))
				continue
			}
			out.Memories = append(out.Memories, Memory{ID: fmt.Sprintf("cl%d", n+1), Kind: "event", Body: body, Q: c.Q, Effects: []Effect{e}})
			carrier = len(out.Memories) - 1
		}
		out.Closed = nil
	}
	// a fact written under the slot of a current state or of where a place
	// is, with its mode left out (or "위치" written as a mode), reads as
	// that mode (11-17: a shorter thinking left the mode out of 43 facts,
	// and their states were not replaced)
	for i, m := range out.Memories {
		if m.Kind != "fact" {
			continue
		}
		switch {
		case stateSlot(m.Slot) && m.Mode == "":
			out.Memories[i].Mode = "상태"
		case m.Slot == PlaceSlot && (m.Mode == "" || m.Mode == PlaceSlot):
			out.Memories[i].Mode = IdentityMode
		}
	}
	// one current state per subject and aspect
	type state struct{ id, slot string }
	var current map[string][]state // subject -> its current states in k
	earlier := map[string]string{} // subject + slot -> this answer's earlier state
	for i, m := range out.Memories {
		if m.Kind != "fact" || m.Mode != "상태" || !stateSlot(m.Slot) || m.About == "" {
			continue
		}
		// a second state of the same subject and slot in one answer replaces
		// the first (1.4)
		key := m.About + "|" + m.Slot
		if id, ok := earlier[key]; ok && id != "" {
			out.Memories[i].Was = append(append(Refs{}, out.Memories[i].Was...), id)
		}
		earlier[key] = m.ID
		if current == nil {
			current = map[string][]state{}
			sup := Superseded(k)
			for _, km := range k.Memories {
				if km.Kind == "fact" && km.Mode == "상태" && stateSlot(km.Slot) && !sup[km.ID] {
					current[km.About] = append(current[km.About], state{km.ID, km.Slot})
				}
			}
		}
		for _, st := range current[m.About] {
			if !replacesState(m.Slot, st.slot) {
				continue
			}
			id := st.id
			named := false
			for _, w := range m.Was {
				named = named || w == id
			}
			if !named {
				out.Memories[i].Was = append(append(Refs{}, out.Memories[i].Was...), id)
			}
		}
	}
	return out, notes
}
