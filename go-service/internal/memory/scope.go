package memory

import (
	"encoding/json"
	"strings"

	"archivecenter2/goservice/internal/lifecycle"
)

// ScopedTurn is a turn a chat reads memory from, in story order.
type ScopedTurn struct {
	Source    Source
	Confirmed bool
	// Inherited is an earlier chat's turn (the parent of a branch, or its
	// own parent): its memory is that chat's.
	Inherited bool
	// Off: the chat's own turn is turned off in the host; its memory is
	// neither made nor read (On drops it), but it keeps its place for
	// matching a branch's copies.
	Off bool
	// Past: a turn of a stored chat attached as this chat's earlier story
	// (C22, lifecycle.Session.Past). It is read like an inherited turn and
	// never made by this chat.
	Past bool
	keys []string        // the turn's row keys, for finding the branch point
	turn *lifecycle.Turn // the turn itself, for its earlier revisions (nil for a recorded copy)
	// recorded: the chat's copies are recorded (lifecycle.Session.Copies),
	// so this item is already what the branch reads; no matching is done.
	recorded bool
	// own: for a copy read as the parent's source, the branch's own source
	// of the turn. A copy the parent has not made is made as this, the
	// branch's own memory (as AC1, a branch never makes the parent's).
	own Source
}

// OwnSource is the chat's own source of the turn: for a copy read as the
// parent's source, the branch's own; otherwise the item's source.
func (t ScopedTurn) OwnSource() Source {
	if t.Inherited && t.own.Session != "" {
		return t.own
	}
	return t.Source
}

// Own lists a chat's active turns, the ones turned off in the host marked:
// the chat's read scope. In a branch whose copies are recorded against its
// current parent, a copy still as recorded (and with no reply turned off
// in the branch) reads the parent's source it was recorded with; the
// branch's own confirmation decides when it is made. Without a record a
// branch reads only its own turns: copies are matched only when recorded.
func Own(s *lifecycle.Session) []ScopedTurn {
	turns := s.ActiveTurns()
	recorded := s.CopiesOf[0] != "" && s.CopiesOf == s.BranchOf
	out := make([]ScopedTurn, len(turns))
	for i, t := range turns {
		own := ScopedTurn{Source: SourceOf(s.Key, t), Confirmed: t.Confirmed, Off: t.Off, keys: rowKeys(t), turn: t, recorded: recorded}
		if src, ok := recordedCopy(s, t); ok && recorded {
			own.own = own.Source
			own.Source, own.Inherited, own.turn = src, true, nil
		}
		out[i] = own
	}
	return out
}

// recordedCopy is the parent source a branch turn reads when it is still
// the copy recorded: the same whole source (every reply, turned off or
// not) and no reply turned off in the branch now.
func recordedCopy(s *lifecycle.Session, t *lifecycle.Turn) (Source, bool) {
	c, ok := s.Copies[t.ID]
	if !ok || c.Own != WholeSourceOf(s.Key, t).Key() {
		return Source{}, false
	}
	for _, r := range t.Responses {
		if r.State == lifecycle.Active && r.Off {
			return Source{}, false
		}
	}
	var src Source
	if json.Unmarshal([]byte(c.Source), &src) != nil {
		return Source{}, false
	}
	return src, true
}

// RecordCopies matches a branch's turns with its parent's turns and records
// them once on the branch (lifecycle.Session.RecordCopies) when the branch
// is first seen with its parent. It matches whole turns as the host copied
// them: every parent turn, also one deleted later, with every reply, also
// one turned off (ASTRA-RECHECK-55 3.1, 3.2, 3.4). Only the turns before
// the branch marker can be copies; a turn after it is the branch's own.
// ok is false, and the branch is matched again at a later observation,
// while the parent is not stored, the parent does not show the branch
// point, a copy's text is not read yet, or the snapshot showed no marker.
func RecordCopies(s *lifecycle.Session, load Loader) (ok bool, err error) {
	if s.BranchOf[0] == "" || s.BeforeMarker == nil {
		return false, nil
	}
	p, found, err := load(s.BranchOf[0])
	if err != nil || !found || p == nil {
		return false, err
	}
	parent := wholeScope(p)
	if !holds(parent, s.BranchOf[1]) {
		return false, nil
	}
	var own []ScopedTurn
	for _, t := range s.ActiveTurns() {
		if !s.BeforeMarker[t.ID] {
			continue
		}
		c := ScopedTurn{Source: WholeSourceOf(s.Key, t), keys: wholeKeys(t), turn: t}
		if !c.Source.Readable() && len(c.Source.Responses) > 0 {
			return false, nil // a copy's text arrives later
		}
		own = append(own, c)
	}
	copies := map[int]lifecycle.Copy{}
	for i, t := range Inherit(parent, s.BranchOf[1], own) {
		if t.Inherited {
			b, _ := json.Marshal(t.Source)
			copies[own[i].Source.Turn] = lifecycle.Copy{Own: own[i].Source.Key(), Source: string(b)}
		}
	}
	s.RecordCopies(s.BranchOf, copies)
	return true, nil
}

// wholeScope lists every turn of a chat in story order, also the ones
// deleted, with whole sources; a recorded copy of a branch is the source
// it reads (an ancestor's), as the branch holds it.
func wholeScope(p *lifecycle.Session) []ScopedTurn {
	recorded := p.CopiesOf[0] != "" && p.CopiesOf == p.BranchOf
	var out []ScopedTurn
	for _, t := range p.Turns {
		if t.Greeting {
			continue
		}
		item := ScopedTurn{Source: WholeSourceOf(p.Key, t), keys: wholeKeys(t), turn: t}
		if c, ok := p.Copies[t.ID]; ok && recorded && c.Own == item.Source.Key() {
			var src Source
			if json.Unmarshal([]byte(c.Source), &src) == nil {
				item.Source, item.Inherited, item.turn = src, true, nil
			}
		}
		out = append(out, item)
	}
	return out
}

// holds reports whether a scope has the row the branch point names.
func holds(scope []ScopedTurn, point string) bool {
	for _, p := range scope {
		for _, k := range p.keys {
			if k == point {
				return true
			}
		}
	}
	return false
}

// On drops the turns turned off in the host.
func On(turns []ScopedTurn) []ScopedTurn {
	out := []ScopedTurn{}
	for _, t := range turns {
		if !t.Off {
			out = append(out, t)
		}
	}
	return out
}

// Inherit lists a branch's read scope from its parent's scope (the
// parent's own read scope, so a branch of a branch reads back to the first
// chat) and its own turns, in the branch's own order. A parent turn up to the
// branch point row is the branch's past when the branch holds a copy of it
// (same texts): its memory is the parent's, and the copy is not made again.
// When the parent changed the turn after the branch (a reroll), the parent's
// revision the branch copied is read, so the parent's later change does not
// rewrite the branch's past. A branch turn that copies no parent turn (the
// branch edited its copy, or starts inside it) is its own turn. Parent turns
// after the branch point are not the branch's past.
//
// Which parent turn a branch turn copies: the one with the same row keys
// (RisuAI keeps the message ids in a branch). When the branch shares no row
// key with the parent (PocketRisu gives the copied messages new ids), the
// parent's turns are taken in order from the start, as far as each one's
// copy matches; the rest of the branch is its own.
func Inherit(parent []ScopedTurn, branchPoint string, own []ScopedTurn) []ScopedTurn {
	if len(own) > 0 && own[0].recorded {
		return own
	}
	// The turn holding the branch point is the branch's past only when the
	// point is its last row: a branch from a user row or from an earlier
	// part of the reply did not copy the rest, and what the branch writes
	// there is its own even when the text is the same.
	end := -1
	for i, p := range parent {
		for j, k := range p.keys {
			if k == branchPoint {
				end = i
				if j != len(p.keys)-1 {
					end = i - 1
				}
			}
		}
	}
	past := parent[:end+1]
	copied := map[int]ScopedTurn{} // branch turn index -> the parent turn it copies
	byKeys := map[string]ScopedTurn{}
	for _, p := range past {
		byKeys[keyIdentity(p)] = p
	}
	shared := false
	for i, c := range own {
		if p, ok := byKeys[keyIdentity(c)]; ok {
			shared = true
			copied[i] = p
		}
	}
	if !shared {
		// A copy whose texts more than one parent turn has (the same input
		// and a reply revision again) cannot name the turn it copies once an earlier
		// copy is gone; it stays the branch's own rather than reading
		// another occurrence's memory.
		byInput := map[string][]ScopedTurn{}
		for _, p := range past {
			byInput[inputIdentity(p.Source)] = append(byInput[inputIdentity(p.Source)], p)
		}
		for i := 0; i < len(own) && i < len(past); i++ {
			if _, ok := copiedRevision(past[i], own[i].Source); !ok {
				break
			}
			copies := 0
			for _, p := range byInput[inputIdentity(own[i].Source)] {
				if _, ok := copiedRevision(p, own[i].Source); ok {
					copies++
				}
			}
			if copies == 1 {
				copied[i] = past[i]
			}
		}
	}
	out := make([]ScopedTurn, 0, len(own))
	for i, c := range own {
		if p, ok := copied[i]; ok {
			if src, ok := copiedRevision(p, c.Source); ok {
				p.Source = src
				p.Inherited = true
				p.Off = c.Off             // the branch's own copy decides
				p.Confirmed = c.Confirmed // and its confirmation (the branch went on from it)
				p.keys = c.keys           // the branch's rows: a branch of it finds its branch point here
				out = append(out, p)
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// inputIdentity is a turn's input texts.
func inputIdentity(s Source) string {
	var b strings.Builder
	for _, m := range s.Members {
		b.WriteString(m.Data)
		b.WriteByte(0)
	}
	return b.String()
}

// keyIdentity is a turn's row keys (RisuAI keeps them in a branch).
func keyIdentity(t ScopedTurn) string { return strings.Join(t.keys, "\x00") }

// copiedRevision is the parent turn's source at the revision the copy
// holds: the same input texts and, for every reply row, the revision the
// copy carries. The copy's generation id names it when there is one (a
// reroll with the same text is another revision); otherwise the earliest
// revision with the copy's text. ok is false when the copy matches none.
// The copy's dates are the dates the branch saw.
func copiedRevision(p ScopedTurn, copy Source) (Source, bool) {
	if len(p.Source.Members) != len(copy.Members) || len(p.Source.Responses) != len(copy.Responses) {
		return Source{}, false
	}
	if p.turn == nil {
		// a copy the parent recorded: only the revision it reads
		for i, m := range p.Source.Members {
			if m.Data != copy.Members[i].Data {
				return Source{}, false
			}
		}
		for i, r := range p.Source.Responses {
			if r.Data != copy.Responses[i].Data {
				return Source{}, false
			}
		}
		src := p.Source
		src.Dates = copy.Dates
		return src, true
	}
	for i, m := range p.Source.Members {
		if m.Data != copy.Members[i].Data {
			return Source{}, false
		}
	}
	src := p.Source
	src.Dates = copy.Dates
	src.Responses = append([]SourceResponse(nil), p.Source.Responses...)
	for i, r := range src.Responses {
		want := copy.Responses[i]
		var pick *lifecycle.Revision
		for _, row := range p.turn.Responses {
			if row.Key != r.Key {
				continue
			}
			for j := range row.Revisions {
				rev := &row.Revisions[j]
				if rev.Data != want.Data {
					continue
				}
				if want.Gen != "" && rev.GenerationID == want.Gen {
					pick = rev
					break
				}
				if pick == nil {
					pick = rev
				}
			}
		}
		if pick == nil {
			return Source{}, false
		}
		src.Responses[i] = SourceResponse{Key: r.Key, Seq: pick.Seq, Gen: pick.GenerationID, Data: pick.Data}
	}
	return src, true
}

func rowKeys(t *lifecycle.Turn) []string {
	var out []string
	for _, m := range t.Members {
		if m.State == lifecycle.Active {
			out = append(out, m.Key)
		}
	}
	for _, r := range t.Responses {
		if r.State == lifecycle.Active {
			out = append(out, r.Key)
		}
	}
	return out
}

// Loader loads a stored chat by key.
type Loader func(key string) (*lifecycle.Session, bool, error)

// ScopeOf lists a chat's read scope: an attached past first, then its turns
// not turned off, a recorded copy read as its parent's source.
func ScopeOf(s *lifecycle.Session) []ScopedTurn { return On(WithPast(s)) }

// Past lists a chat's attached past (C22) in story order: read as inherited
// turns of the chat it was attached from, confirmed there.
func Past(s *lifecycle.Session) []ScopedTurn {
	out := []ScopedTurn{}
	for _, p := range s.Past {
		var src Source
		if json.Unmarshal([]byte(p.Source), &src) != nil {
			continue
		}
		t := ScopedTurn{Source: src, Confirmed: true, Inherited: true, Past: true}
		if p.Own != "" {
			json.Unmarshal([]byte(p.Own), &t.own)
		}
		out = append(out, t)
	}
	return out
}

// WithPast is Own with the attached past before it: what the chat's memory
// is read from. Own alone stays the chat's own turns, one per active turn.
func WithPast(s *lifecycle.Session) []ScopedTurn {
	past := Past(s)
	if len(past) == 0 {
		return Own(s)
	}
	return append(past, Own(s)...)
}

// UniquePast keeps each turn of an attached past once, where it first
// comes: chats stitched together can carry the same turn (a chat that
// already reads another as its past, and that other chat too). A turn is
// the source it reads with its own source; other revisions stay apart.
func UniquePast(past []lifecycle.PastTurn) []lifecycle.PastTurn {
	out := make([]lifecycle.PastTurn, 0, len(past))
	seen := map[string]bool{}
	for _, p := range past {
		var src, own Source
		json.Unmarshal([]byte(p.Source), &src)
		key := src.Key()
		if p.Own != "" {
			json.Unmarshal([]byte(p.Own), &own)
			key += "|" + own.Key()
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// PastOf freezes a stored chat's read scope (its own past included) as the
// past another chat attaches, each turn marked as attached from s.
func PastOf(s *lifecycle.Session) []lifecycle.PastTurn {
	var out []lifecycle.PastTurn
	for _, t := range ScopeOf(s) {
		b, _ := json.Marshal(t.Source)
		p := lifecycle.PastTurn{Source: string(b), From: s.Key}
		if t.Inherited && t.own.Session != "" {
			o, _ := json.Marshal(t.own)
			p.Own = string(o)
		}
		out = append(out, p)
	}
	return out
}

// wholeKeys are a turn's row keys as WholeSourceOf reads its rows.
func wholeKeys(t *lifecycle.Turn) []string {
	var out []string
	for _, m := range t.Members {
		if wholeRow(m.State, t.Inactive) {
			out = append(out, m.Key)
		}
	}
	for _, r := range t.Responses {
		if wholeRow(r.State, t.Inactive) {
			out = append(out, r.Key)
		}
	}
	return out
}
