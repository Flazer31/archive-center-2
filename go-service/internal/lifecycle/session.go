package lifecycle

import (
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"
)

// Session reconciles successive snapshots of one host chat.
type Session struct {
	Key    string
	Turns  []*Turn
	Events []Event

	nextTurnID int
	memberTurn map[string]*Turn        // member row key -> turn
	respTurn   map[string]*Turn        // response row key -> turn
	respRow    map[string]*ResponseRow // response row key -> row
	member     map[string]*Member
	revSeq     int
	renames    []Rename
	noted      map[string]bool // observations already reported as events
	// Progress is the last applied plugin seq per plugin boot (boot_id ->
	// seq), kept with the session so an older observation of this chat that
	// arrives later never moves its state back.
	Progress map[string]int64
	// BranchOf is the parent chat and message named by a RisuAI branch
	// marker ("chat:<id>", "c:<message chatId>"); empty when not a branch.
	BranchOf [2]string
	// Markers are the branch markers seen in this chat (parent chat and
	// message): only a marker not seen before names a new parent.
	Markers [][2]string
	// Copies are the branch's copies of its parent's turns, recorded once
	// when the branch was first seen with its parent (memory.RecordCopies),
	// by the branch's turn ID; CopiesOf is the parent they were recorded
	// against. Later deletions, edits or rerolls in either chat do not
	// change which parent turn a copy is.
	Copies   map[int]Copy
	CopiesOf [2]string
	// BeforeMarker: in the last snapshot that showed the branch marker, the
	// turns all of whose rows came before it (the rows the host copied);
	// nil when the last snapshot showed no marker. Not stored.
	BeforeMarker map[int]bool
	// Past is the stored chats the user attached as this chat's earlier
	// story (C22, C25 in the order chosen): each chat's read scope frozen
	// when attached, read before this chat's own turns. It is not a host
	// observation: Apply never sees, deletes or replaces it.
	Past []PastTurn
}

// PastTurn is one turn of an attached past: the source it reads and, for a
// branch copy that chat made as its own, that own source (JSON of
// memory.Source, kept opaque here; Own is empty otherwise). From is the
// stored chat it was attached from.
type PastTurn struct {
	Source string
	Own    string
	From   string
}

// Copy is a branch turn that copies a parent turn: Own is the branch
// turn's source key when recorded (the copy as it was); Source is the
// parent's source it reads (JSON of memory.Source, kept opaque here).
type Copy struct {
	Own    string
	Source string
}

// RecordCopies sets the branch's copies against parent.
func (s *Session) RecordCopies(parent [2]string, copies map[int]Copy) {
	s.Copies, s.CopiesOf = copies, parent
	s.emit("branch_copies", 0, fmt.Sprintf("%s %s %d", parent[0], parent[1], len(copies)))
}

// Rename is a row key change: a row first seen without chatId got one later
// (RisuAI assigns chatId when it builds a request).
type Rename struct{ Old, New string }

func NewSession(key string) *Session {
	return &Session{
		Key: key, nextTurnID: 1,
		memberTurn: map[string]*Turn{}, respTurn: map[string]*Turn{},
		respRow: map[string]*ResponseRow{}, member: map[string]*Member{},
		noted: map[string]bool{}, Progress: map[string]int64{},
	}
}

// note emits an observation event once per in-memory session and detail, so
// repeated snapshots do not grow the event log (after a restart it may be
// reported again).
func (s *Session) note(kind, detail string) {
	if s.noted[kind+"\x00"+detail] {
		return
	}
	s.noted[kind+"\x00"+detail] = true
	s.emit(kind, 0, detail)
}

// branchParent reads a RisuAI branch marker:
// {{specialcomment::branchedfrom::<chat id>::<chat name>::<message chatId>::}}
func branchParent(data string) (chat, message string, ok bool) {
	const prefix = "{{specialcomment::branchedfrom::"
	i := strings.Index(data, prefix)
	if i < 0 {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimSpace(data[i+len(prefix):]), "}}"), "::")
	if len(parts) < 3 || parts[0] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// obsRow is a snapshot row with its key.
type obsRow struct {
	Row
	key      string
	stable   bool
	fallback string // the key the row would have without chatId
}

// group is consecutive user rows and the char rows after them. A greeting
// group holds char rows before the first user row.
type group struct {
	rows     []obsRow
	greeting bool
}

// rowKeys keys each row by its host chatId. Without one, role + time + body
// hash + occurrence is used (not the index, which shifts when earlier rows
// are deleted); such keys are marked unverified.
func rowKeys(rows []Row) []obsRow {
	out := make([]obsRow, 0, len(rows))
	seenFallback := map[string]int{}
	for _, r := range rows {
		h := fnv.New32a()
		_, _ = h.Write([]byte(r.Data))
		base := fmt.Sprintf("f:%s:%d:%x", r.Role, r.Time, h.Sum32())
		seenFallback[base]++
		fb := fmt.Sprintf("%s#%d", base, seenFallback[base])
		if r.ChatID != "" {
			out = append(out, obsRow{Row: r, key: "c:" + r.ChatID, stable: true, fallback: fb})
			continue
		}
		out = append(out, obsRow{Row: r, key: fb, fallback: fb})
	}
	return out
}

// branchMarker reports the RisuAI branch marker row (comment + disabled char row).
func branchMarker(r Row) bool {
	return r.Role == "char" && r.IsComment && r.Disabled == "true"
}

func (s *Session) emit(kind string, turn int, detail string) {
	s.Events = append(s.Events, Event{Kind: kind, TurnID: turn, Detail: detail})
}

func (s *Session) newTurn() *Turn {
	t := &Turn{ID: s.nextTurnID}
	s.nextTurnID++
	s.Turns = append(s.Turns, t)
	return t
}

func (s *Session) lastActiveTurn() *Turn {
	for i := len(s.Turns) - 1; i >= 0; i-- {
		if !s.Turns[i].Inactive && !s.Turns[i].Greeting {
			return s.Turns[i]
		}
	}
	return nil
}

func activeResponses(t *Turn) int {
	n := 0
	for _, r := range t.Responses {
		if r.State == Active {
			n++
		}
	}
	return n
}

// skipped reports rows that are not part of any turn: branch markers and
// empty readable char rows. A known row that became empty is therefore
// missing (deleted), not kept with its old body.
func skipped(r obsRow) bool {
	return branchMarker(r.Row) || (r.Role == "char" && !r.Unreadable && strings.TrimSpace(r.Data) == "")
}

// groups splits a snapshot into input groups.
func groups(rows []obsRow) []group {
	var out []group
	var cur *group
	lastRole := ""
	for _, r := range rows {
		if skipped(r) {
			continue
		}
		switch {
		case r.Role == "char" && cur == nil:
			out = append(out, group{greeting: true})
			cur = &out[len(out)-1]
		case r.Role == "user" && (cur == nil || lastRole != "user"):
			out = append(out, group{})
			cur = &out[len(out)-1]
		}
		cur.rows = append(cur.rows, r)
		lastRole = r.Role
	}
	return out
}

// knownTurn returns the turn of the first row of the group already linked.
func (s *Session) knownTurn(g group) *Turn {
	for _, r := range g.rows {
		if t, ok := s.memberTurn[r.key]; ok && r.Role == "user" {
			return t
		}
		if t, ok := s.respTurn[r.key]; ok && r.Role == "char" {
			return t
		}
	}
	return nil
}

func vacated(t *Turn, seen map[string]bool) bool {
	for _, m := range t.Members {
		if seen[m.Key] {
			return false
		}
	}
	for _, r := range t.Responses {
		if seen[r.Key] {
			return false
		}
	}
	return true
}

// membersUnverified reports a group whose user rows all lack chatId. RisuAI
// gives rows a chatId while building a request (process/index.svelte.ts:917);
// rows never sent, imported or added by an output trigger can lack one, so a
// continuation chosen this way is unverified (it is reported as an event).
func membersUnverified(g group) bool {
	users := 0
	for _, r := range g.rows {
		if r.Role == "user" {
			users++
			if r.stable {
				return false
			}
		}
	}
	return users > 0
}

// unverifiedTurn reports a turn whose user rows were all keyed without chatId.
func unverifiedTurn(t *Turn) bool {
	for _, m := range t.Members {
		if !strings.HasPrefix(m.Key, "f:") {
			return false
		}
	}
	return len(t.Members) > 0
}

// upgradeKeys re-keys rows first seen without chatId that now carry one, so
// the same host row keeps its links. Only rows still present in the last
// view are re-keyed; a deleted row is history, not the owner of a new row.
func (s *Session) upgradeKeys(rows []obsRow) {
	present := map[string]bool{}
	for _, r := range rows {
		if !r.stable {
			present[r.key] = true
		}
	}
	for _, r := range rows {
		if !r.stable || present[r.fallback] {
			continue
		}
		// a row cut away keeps its key; a new row with its text is new
		// (review 21 B-3; the mark is stored, review 22 A-5)
		if m, ok := s.member[r.fallback]; ok && m.Cut {
			continue
		}
		if row, ok := s.respRow[r.fallback]; ok && row.Cut {
			continue
		}
		if r.Role == "user" {
			if m, ok := s.member[r.fallback]; ok && m.State == Active {
				if _, known := s.member[r.key]; !known {
					t := s.memberTurn[r.fallback]
					delete(s.member, r.fallback)
					delete(s.memberTurn, r.fallback)
					m.Key = r.key
					s.member[r.key], s.memberTurn[r.key] = m, t
					s.renames = append(s.renames, Rename{r.fallback, r.key})
					s.emit("row_key_upgraded", t.ID, r.key)
				}
			}
		} else if row, ok := s.respRow[r.fallback]; ok && row.State == Active {
			if _, known := s.respRow[r.key]; !known {
				t := s.respTurn[r.fallback]
				delete(s.respRow, r.fallback)
				delete(s.respTurn, r.fallback)
				row.Key = r.key
				s.respRow[r.key], s.respTurn[r.key] = row, t
				s.renames = append(s.renames, Rename{r.fallback, r.key})
				s.emit("row_key_upgraded", t.ID, r.key)
			}
		}
	}
}

// TakeRenames returns key changes since the last call (for storage).
func (s *Session) TakeRenames() []Rename {
	r := s.renames
	s.renames = nil
	return r
}

// Apply reconciles one snapshot. It never rejects: unknown or missing
// coordinates only produce events.
//
// A group joins the turn of any of its rows that is already linked; links
// are kept once made. A group without linked rows is a new turn, except
// that a group whose user rows all lack chatId takes the next existing turn in
// order whose rows all disappeared (an edit changes the fallback key), so a
// missing coordinate never turns a replacement into an append.
func (s *Session) Apply(snap Snapshot) {
	rows := rowKeys(snap.Rows)
	s.upgradeKeys(rows)
	seen := map[string]bool{}
	// the parent is the last branch marker: a branch of a branch also holds
	// the copied marker of the earlier branch, before its own. A marker seen
	// before does not name the parent again: when the direct parent's
	// marker is gone (deleted, or taken off for a PocketRisu reroll) and only
	// an ancestor's is left, the parent stays.
	var parent [2]string
	for _, r := range rows {
		if branchMarker(r.Row) {
			if chat, msg, ok := branchParent(r.Data); ok {
				parent = [2]string{"chat:" + chat, "c:" + msg}
			}
			s.note("branch_marker", r.Data)
		}
		if skipped(r) {
			continue
		}
		seen[r.key] = true
		if !r.stable {
			s.note("row_without_chat_id", r.key)
		}
	}

	newMarker := parent[0] != ""
	for _, m := range s.Markers {
		if m == parent {
			newMarker = false
		}
	}
	for _, r := range rows {
		if branchMarker(r.Row) {
			if chat, msg, ok := branchParent(r.Data); ok {
				s.seeMarker([2]string{"chat:" + chat, "c:" + msg})
			}
		}
	}
	if newMarker && parent != s.BranchOf {
		s.BranchOf = parent
		s.emit("branch_of", 0, s.BranchOf[0]+" "+s.BranchOf[1])
	}

	// the front cut away is read from the rows known before this snapshot:
	// a row added now is not a row "still there"
	front := frontTrim(s.Turns, seen)
	// a row cut away before stays cut while it is gone: it is no longer on
	// the host's screen, so no later delete there can be about it (review
	// 21: an edit after a cut found no remaining row and lost the cut turn)
	for _, t := range s.Turns {
		for _, k := range cutKeys(t) {
			if !seen[k] {
				if front == nil {
					front = map[string]bool{}
				}
				front[k] = true
			}
		}
	}
	order := s.ActiveTurns() // existing turns, for the no-chatId takeover
	pos := map[*Turn]int{}
	for i, t := range order {
		pos[t] = i
	}
	lastPos := -1
	newInput := false
	added := map[*Turn]bool{}  // turns that got a new response row in this snapshot
	grew := map[*Turn]bool{}   // turns that got any new row (an edit, a reroll): not a front cut
	offOf := map[string]bool{} // disabled marks of rows that stand for a known row

	for _, g := range groups(rows) {
		var t *Turn
		switch {
		case g.greeting:
			t = s.greetingTurn()
		default:
			t = s.knownTurn(g)
			// The next existing turn whose rows all disappeared continues
			// when either side lacks chatId: an edit changes the fallback key
			// (and RisuAI may assign the chatId in the same request).
			// a turn cut away from the front is not one an edit continues
			// (review 21 B-2): the next turn after it is
			next := lastPos + 1
			for next < len(order) && (wasCut(order[next]) || cutTurn(order[next], front)) {
				next++
			}
			if t == nil && next < len(order) {
				if cand := order[next]; (membersUnverified(g) || unverifiedTurn(cand)) && vacated(cand, seen) {
					t = cand
					s.emit("turn_continued_without_chat_id", t.ID, g.rows[0].key) // unverified
				}
			}
			if t == nil {
				t = s.newTurn()
				s.emit("turn_opened", t.ID, g.rows[0].key)
			}
		}
		if p, ok := pos[t]; ok && p > lastPos {
			lastPos = p
		}
		cur := t
		parts := 0     // char rows of this group already placed on cur
		users := 0     // user rows of this group already placed
		known := false // a row of this group already known was placed
		for _, r := range g.rows {
			switch r.Role {
			case "user":
				// An unreadable row without chatId has no key of its own; it
				// stands for the member at the same place.
				if r.Unreadable && !r.stable {
					if m := nthActiveMember(cur, users); m != nil {
						seen[m.Key] = true
						offOf[m.Key] = r.Disabled == "true"
						users++
						continue
					}
				}
				users++
				if owner, ok := s.memberTurn[r.key]; ok {
					m := s.member[r.key]
					if !r.Unreadable && m.Data != r.Data {
						s.emit("member_edited", owner.ID, r.key)
						m.Data = r.Data
					}
					m.State = Active
					s.restore(owner)
					cur = owner
					known = true
					continue
				}
				// A row without chatId before the known rows of a later turn
				// is an edit of the turn right before it when that turn's rows
				// are all gone (its reply deleted, its input edited): it joins
				// that turn as a whole group without known rows does, not the
				// later one (review 21/22 A-4)
				// The host adds new rows at the end only, so a new row before a
				// later turn's known rows is an edit even when it got a chatId
				// on the way (review 23 A-2). It continues the turn right before
				// the group's own turn t: later new rows of the group go there
				// too, never further back (review 23 A-1).
				if !known {
					if p, ok := pos[t]; ok && p > 0 {
						if prev := order[p-1]; !wasCut(prev) && vacated(prev, seen) {
							cur = prev
							s.emit("turn_continued_without_chat_id", prev.ID, r.key) // unverified
						}
					}
				}
				// A new input is a new user row on a turn still waiting for a
				// reply; a changed fallback key of an answered row is an edit.
				// Rows first seen in a load are a restore, not a new input.
				if snap.Source != SourceLoad && activeResponses(cur) == 0 {
					newInput = true
				}
				m := &Member{Key: r.key, Data: r.Data, State: Active}
				cur.Members = append(cur.Members, m)
				s.memberTurn[r.key] = cur
				s.member[r.key] = m
				s.restore(cur)
				grew[cur] = true
				s.emit("member_added", cur.ID, r.key)
			case "char":
				// An unreadable row without chatId has no key of its own (the
				// fallback hashes the text); it stands for the response at the
				// same place on the current turn.
				if r.Unreadable && !r.stable {
					if row := nthActive(cur, parts); row != nil {
						seen[row.Key] = true
						offOf[row.Key] = r.Disabled == "true"
						s.observeGeneration(cur, row, r.GenerationID)
						parts++
						continue
					}
				}
				// The first readable text of a row seen unreadable without chatId
				// arrives under a new key (a fallback of the text, or a chatId
				// given meanwhile); it is the same row.
				if !r.Unreadable {
					if _, known := s.respRow[r.key]; !known {
						if ph := nthActive(cur, parts); ph != nil && len(ph.Revisions) == 0 && strings.HasPrefix(ph.Key, "f:") {
							s.rekeyResponse(ph.Key, r.key)
						}
					}
				}
				prev := cur
				var isNew bool
				cur, isNew = s.applyResponse(cur, r, snap.Source)
				if isNew {
					added[cur] = true
					grew[cur] = true
				}
				if cur != prev {
					parts = 0
				}
				parts++
			}
		}
	}
	// a turn that got a new row in this snapshot was edited or rerolled
	// there, not cut away: its gone rows go as before
	for t := range grew {
		for _, m := range t.Members {
			delete(front, m.Key)
		}
		for _, r := range t.Responses {
			delete(front, r.Key)
		}
	}
	s.reconcileMissing(seen, added, front)
	markCut(s.Turns, front)
	s.noteBeforeMarker(rows)
	s.noteOff(rows, offOf)
	s.finalize(newInput)
	s.noteDates(snap.Dates)
}

// noteDates gives the newest active turn the date variables seen now.
func (s *Session) noteDates(dates map[string]string) {
	turns := s.ActiveTurns()
	if len(dates) == 0 || len(turns) == 0 {
		return
	}
	t := turns[len(turns)-1]
	if reflect.DeepEqual(t.Dates, dates) {
		return
	}
	t.Dates = map[string]string{}
	for k, v := range dates {
		t.Dates[k] = v
	}
	s.emit("story_date", t.ID, "")
}

// restore makes a turn active again when its rows are present.
func (s *Session) restore(t *Turn) {
	if t.Inactive {
		t.Inactive = false
		s.emit("turn_restored", t.ID, "")
	}
}

func (s *Session) greetingTurn() *Turn {
	for _, t := range s.Turns {
		if t.Greeting {
			return t
		}
	}
	t := &Turn{ID: 0, Greeting: true}
	s.Turns = append([]*Turn{t}, s.Turns...)
	return t
}

// applyResponse links a char row. A known row stays with its owner (and later
// new rows follow that owner); a new row joins the given turn. It returns the
// owner and whether the row is new.
func (s *Session) applyResponse(t *Turn, r obsRow, src Source) (*Turn, bool) {
	if row, ok := s.respRow[r.key]; ok {
		owner := s.respTurn[r.key]
		if owner != t {
			s.emit("response_row_moved", t.ID, r.key) // unverified host shape; kept on its owner
		}
		row.State = Active
		row.AwaitingReplacement = false
		s.restore(owner)
		newGen := s.observeGeneration(owner, row, r.GenerationID)
		if r.Unreadable {
			return owner, false
		}
		n := len(row.Revisions)
		// the text of a generation observed earlier while unreadable arrives
		// (with its id, or without one: it belongs to the row's known
		// generation): it is a new revision even when its text equals the
		// previous one
		genCatchUp := n > 0 && row.Gen != "" && row.Revisions[n-1].GenerationID != "" && row.Revisions[n-1].GenerationID != row.Gen
		if n == 0 || row.Revisions[n-1].Data != r.Data || newGen || genCatchUp {
			s.revSeq++
			// a text seen without a generation id belongs to the row's
			// generation known at that time (an edit keeps its generation)
			rev := Revision{Seq: s.revSeq, Data: r.Data, GenerationID: row.Gen, Source: src}
			if n == 0 || (genCatchUp && !newGen) {
				// the first readable text of a row (or generation) already
				// counted (for example a translation original that arrived
				// after the output event) keeps the observed completion
				rev.Confirmed = owner.Confirmed
			}
			row.Revisions = append(row.Revisions, rev)
			s.emit("response_revised", owner.ID, r.key)
		}
		return owner, false
	}
	// A reroll removes the old reply before the request, so the old row is
	// seen deleted first; a new reply to a turn left without one replaces
	// the rows deleted last, not parts deleted earlier on their own.
	noneActive := activeResponses(t) == 0
	for _, old := range t.Responses {
		if old.AwaitingReplacement {
			if noneActive && old.State == Deleted {
				old.State = Replaced
				s.emit("response_replaced", t.ID, old.Key)
			}
			old.AwaitingReplacement = false
		}
	}
	row := &ResponseRow{Key: r.key, State: Active, Gen: r.GenerationID}
	if !r.Unreadable {
		s.revSeq++
		row.Revisions = []Revision{{Seq: s.revSeq, Data: r.Data, GenerationID: r.GenerationID, Source: src}}
	} else {
		s.emit("response_unreadable", t.ID, r.key)
	}
	t.Responses = append(t.Responses, row)
	s.respRow[r.key] = row
	s.respTurn[r.key] = t
	t.Confirmed = false
	s.emit("response_added", t.ID, r.key)
	return t, true
}

// observeGeneration records the row's generation id. Only a known id that
// differs from the last known one is a new generation, which the earlier
// completion does not cover; a missing id is unknown, not new.
func (s *Session) observeGeneration(owner *Turn, row *ResponseRow, gen string) bool {
	if gen == "" {
		return false
	}
	newGen := row.Gen != "" && gen != row.Gen
	if row.Gen == "" {
		// the first id seen names the generation the texts read so far had
		for i := range row.Revisions {
			if row.Revisions[i].GenerationID == "" {
				row.Revisions[i].GenerationID = gen
			}
		}
	}
	row.Gen = gen
	if newGen {
		owner.Confirmed = false
		s.emit("response_new_generation", owner.ID, row.Key)
	}
	return newGen
}

// nthActiveMember returns the n-th active member of a turn, or nil.
func nthActiveMember(t *Turn, n int) *Member {
	for _, m := range t.Members {
		if m.State == Active {
			if n == 0 {
				return m
			}
			n--
		}
	}
	return nil
}

// rekeyResponse gives a response row a new key and records the rename.
func (s *Session) rekeyResponse(old, key string) {
	row, t := s.respRow[old], s.respTurn[old]
	delete(s.respRow, old)
	delete(s.respTurn, old)
	row.Key = key
	s.respRow[key], s.respTurn[key] = row, t
	s.renames = append(s.renames, Rename{old, key})
	s.emit("row_key_upgraded", t.ID, key)
}

// nthActive returns the n-th active response of a turn, or nil.
func nthActive(t *Turn, n int) *ResponseRow {
	for _, r := range t.Responses {
		if r.State == Active {
			if n == 0 {
				return r
			}
			n--
		}
	}
	return nil
}

// reconcileMissing marks rows that disappeared. A response that vanished from
// a turn that got a new response row in this snapshot was replaced (reroll,
// regenerate); otherwise it was deleted. User rows that vanished keep their turn.
func (s *Session) reconcileMissing(seen map[string]bool, added map[*Turn]bool, front map[string]bool) {
	for _, t := range s.Turns {
		var deleted []*ResponseRow
		for _, r := range t.Responses {
			if seen[r.Key] || front[r.Key] || r.State != Active {
				continue
			}
			if added[t] {
				r.State = Replaced
				s.emit("response_replaced", t.ID, r.Key)
			} else {
				r.State = Deleted
				deleted = append(deleted, r)
				s.emit("response_deleted", t.ID, r.Key)
			}
		}
		// the turn's last active replies were removed: a reroll may follow
		if len(deleted) > 0 && activeResponses(t) == 0 {
			for _, r := range deleted {
				r.AwaitingReplacement = true
			}
		}
		allMembersGone := len(t.Members) > 0
		for _, m := range t.Members {
			if seen[m.Key] || front[m.Key] {
				allMembersGone = false
				continue
			}
			if m.State == Active {
				m.State = Deleted
				s.emit("member_deleted", t.ID, m.Key)
			}
		}
		if allMembersGone && activeResponses(t) == 0 && !t.Inactive {
			t.Inactive = true
			s.emit("turn_deleted", t.ID, "")
		}
	}
}

// frontTrim is the chat's earliest active rows that are gone while a later
// row is still there: RisuAI's /cut and /del cut the front of a chat away to
// save browser memory, and their text and memory must stay (user 11-08: "해당
// 명령어가 사용되어서 삭제가 되었을 경우에는 원문이나 DB에 저장된 기억을
// 날려선 안돼"). The host runs these commands before any plugin hook, so the
// shape tells them: a screen delete removes one message or a message and all
// after it, never the front. A screen delete of the first message alone
// looks the same and is kept too (user 11-08 "앞쪽 연속 삭제는 보존"). Rows
// gone after the first row still there are deleted as before; with no row
// left there is no front to keep.
func frontTrim(turns []*Turn, seen map[string]bool) map[string]bool {
	front := map[string]bool{}
	for _, t := range turns {
		if t.Inactive {
			continue
		}
		for _, m := range t.Members {
			if m.State == Active {
				if seen[m.Key] {
					return front
				}
				front[m.Key] = true
			}
		}
		for _, r := range t.Responses {
			if r.State == Active {
				if seen[r.Key] {
					return front
				}
				front[r.Key] = true
			}
		}
	}
	return nil
}

// cutKeys are the turn's rows marked cut (still active).
func cutKeys(t *Turn) []string {
	var out []string
	for _, m := range t.Members {
		if m.Cut && m.State == Active {
			out = append(out, m.Key)
		}
	}
	for _, r := range t.Responses {
		if r.Cut && r.State == Active {
			out = append(out, r.Key)
		}
	}
	return out
}

// markCut marks the rows cut away from the front in this snapshot and
// clears the mark of every other row (a cut row seen again is back).
func markCut(turns []*Turn, front map[string]bool) {
	for _, t := range turns {
		for _, m := range t.Members {
			m.Cut = front[m.Key] && m.State == Active
		}
		for _, r := range t.Responses {
			r.Cut = front[r.Key] && r.State == Active
		}
	}
}

// wasCut is a turn whose active rows were marked cut at an earlier snapshot:
// no later edit can be about it (it is off the screen). A turn whose rows
// all went in this snapshot without chatId may be an edit and may be
// continued (review 21/22 A-4).
func wasCut(t *Turn) bool {
	found := false
	for _, m := range t.Members {
		if m.State == Active {
			if !m.Cut {
				return false
			}
			found = true
		}
	}
	for _, r := range t.Responses {
		if r.State == Active {
			if !r.Cut {
				return false
			}
			found = true
		}
	}
	return found
}

// cutTurn is a turn whose active rows were all cut away from the front.
func cutTurn(t *Turn, front map[string]bool) bool {
	found := false
	for _, m := range t.Members {
		if m.State == Active {
			if !front[m.Key] {
				return false
			}
			found = true
		}
	}
	for _, r := range t.Responses {
		if r.State == Active {
			if !front[r.Key] {
				return false
			}
			found = true
		}
	}
	return found
}

func (s *Session) confirm(t *Turn, why string) {
	t.Confirmed = true
	for _, r := range t.Responses {
		if r.State == Active && len(r.Revisions) > 0 {
			r.Revisions[len(r.Revisions)-1].Confirmed = true
		}
	}
	s.emit("turn_confirmed", t.ID, why)
}

// finalize confirms a turn when the next input comes: the reply the user
// went on from is the one kept, so rerolls and reply choices before it
// (the host's reroll arrows) make no memory of their own. The latest turn
// is still in the recent chat the model reads. A repeated beforeRequest
// with no new user row (provider retry) never confirms anything.
func (s *Session) finalize(newInput bool) {
	if !newInput {
		return
	}
	active := s.ActiveTurns()
	if len(active) >= 2 {
		prev := active[len(active)-2]
		if activeResponses(prev) > 0 && !prev.Confirmed {
			s.confirm(prev, "next_input")
		}
	}
}

// TakeEvents returns the events recorded since the last call and clears
// them, so a long chat does not keep every event in memory.
func (s *Session) TakeEvents() []Event {
	ev := s.Events
	s.Events = nil
	return ev
}

// ActiveTurns returns turns that still exist in the host chat (excluding greeting).
func (s *Session) ActiveTurns() []*Turn {
	out := []*Turn{}
	for _, t := range s.Turns {
		if !t.Inactive && !t.Greeting {
			out = append(out, t)
		}
	}
	return out
}

// noteOff marks what the snapshot shows disabled ("true"; "allBefore" only
// cuts what the model reads before it, and those turns stay in memory): a
// turn whose user row is disabled is off, and a disabled reply is off by
// itself. A row not seen by key keeps its mark. offOf holds the marks of rows without a key of
// their own that stand for a known row (an unreadable row without chatId).
func (s *Session) noteOff(rows []obsRow, offOf map[string]bool) {
	disabled := map[string]bool{}
	for _, r := range rows {
		if !skipped(r) {
			disabled[r.key] = r.Disabled == "true"
		}
	}
	for k, d := range offOf {
		disabled[k] = d
	}
	for _, t := range s.Turns {
		seen, off := false, false
		for _, m := range t.Members {
			if d, ok := disabled[m.Key]; ok && m.State == Active {
				seen, off = true, off || d
			}
		}
		for _, r := range t.Responses {
			if d, ok := disabled[r.Key]; ok && r.State == Active && d != r.Off {
				r.Off = d
				s.emit("reply_off", t.ID, r.Key+" "+fmt.Sprint(d))
			}
		}
		if seen && off != t.Off {
			t.Off = off
			s.emit("turn_off", t.ID, fmt.Sprint(off))
		}
	}
}

// seeMarker records a branch marker as seen.
func (s *Session) seeMarker(m [2]string) {
	for _, x := range s.Markers {
		if x == m {
			return
		}
	}
	s.Markers = append(s.Markers, m)
}

// noteBeforeMarker finds the turns whose rows all come before the last
// branch marker of the snapshot: the copies a branch can hold.
func (s *Session) noteBeforeMarker(rows []obsRow) {
	s.BeforeMarker = nil
	at := -1
	index := map[string]int{}
	for i, r := range rows {
		if branchMarker(r.Row) {
			at = i
		}
		index[r.key] = i
	}
	if at < 0 {
		return
	}
	s.BeforeMarker = map[int]bool{}
	for _, t := range s.ActiveTurns() {
		before, any := true, false
		for _, k := range rowKeysOf(t) {
			i, ok := index[k]
			if !ok || i > at {
				before = false
			}
			any = true
		}
		if any && before {
			s.BeforeMarker[t.ID] = true
		}
	}
}

func rowKeysOf(t *Turn) []string {
	var out []string
	for _, m := range t.Members {
		if m.State == Active {
			out = append(out, m.Key)
		}
	}
	for _, r := range t.Responses {
		if r.State == Active {
			out = append(out, r.Key)
		}
	}
	return out
}
