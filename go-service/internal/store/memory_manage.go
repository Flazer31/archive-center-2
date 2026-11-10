package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// The memory screen (step 9-4): a chat's memories, read in its read scope;
// a memory of the chat's own turns can have its body and importance edited
// or be deleted (as AC1). A branch reads its parent's memories as they are
// stored, so they are read only there (user decision 10-28).

var (
	ErrMemoryNotFound = errors.New("memory_not_found")
	// ErrInheritedOnly: a branch's memory read from its parent (the caller
	// tells it from the chat's list)
	ErrInheritedOnly = errors.New("inherited_read_only")
	ErrEmptyBody     = errors.New("empty_body")
	ErrBadImportance = errors.New("bad_importance")
)

// ListedMemory is one memory as the memory screen shows it.
type ListedMemory struct {
	ID        string            `json:"id"`
	Turn      int               `json:"turn"` // its turn's place among the chat's turns, from 1
	Kind      string            `json:"kind"`
	About     string            `json:"about,omitempty"` // a name, as read
	Owner     string            `json:"owner,omitempty"`
	Slot      string            `json:"slot,omitempty"`
	Body      string            `json:"body"`
	Imp       *float64          `json:"imp,omitempty"`
	Effects   []memory.Effect   `json:"effects,omitempty"`
	Secret    bool              `json:"secret"`    // someone is kept unaware of it
	Inherited bool              `json:"inherited"` // the parent chat's: read only here
	Aware     []ListedAwareness `json:"awareness,omitempty"`
	Q         string            `json:"q,omitempty"`
	Ref       string            `json:"ref,omitempty"`
	At        string            `json:"at,omitempty"`
	Was       memory.Refs       `json:"was,omitempty"`
	Scene     string            `json:"scene,omitempty"`
	Roles     *memory.Roles     `json:"roles,omitempty"`
	Time      string            `json:"time,omitempty"`
	Outcome   string            `json:"outcome,omitempty"`
	Setting   string            `json:"setting,omitempty"`
	SeenBy    memory.Who        `json:"seen_by,omitempty"`
	Mode      string            `json:"mode,omitempty"`
	To        memory.Who        `json:"to,omitempty"`
	Scope     string            `json:"scope,omitempty"`
	Terms     string            `json:"terms,omitempty"`
	Status    string            `json:"status,omitempty"`
	Aspect    string            `json:"aspect,omitempty"`
	Basis     *memory.Basis     `json:"basis,omitempty"`
	Affect    *memory.Affect    `json:"affect,omitempty"`
	Expressed string            `json:"expressed,omitempty"`
	Differs   string            `json:"differs,omitempty"`

	TurnID     int `json:"turn_id,omitempty"` // local logical turn, independent of display order
	SourceTurn int `json:"source_turn,omitempty"`

	// Date is the recorded date of its turn (the story date, else Day N);
	// When is when it happened (C12, memory.WhenOf).
	Date string `json:"date,omitempty"`
	memory.When

	// Replaced is a memory a later record replaced (memory.Superseded over
	// the story): not delivered, kept for the record (C14).
	Replaced bool `json:"replaced,omitempty"`
	// Changed is the slots later records changed of a fact they changed in
	// part (memory.PartlyChanged over the story): it stays current, as
	// delivered with "changed later".
	Changed []string `json:"changed,omitempty"`
	// Person is whom the by-person view files it under (listedMemory): a
	// fact's about, a view's owner, by name; "" = about nobody.
	Person string `json:"person,omitempty"`
	// Hold is its C18 flag: pin | mute | "" (memory_hold.go).
	Hold string `json:"hold,omitempty"`
	// PersonRef is Person's stored reference (N12), which tells two names
	// that read the same apart.
	PersonRef string `json:"person_ref,omitempty"`
	// NameTypes is the type ("" when none was written) of its person and its
	// effects' names, by stored reference, for the by-name view's tabs
	// (memories.go); a reference to a record is not in it. EffectRefs is
	// each effect's stored about, in the order of Effects. Not sent.
	NameTypes  map[string]string `json:"-"`
	EffectRefs []string          `json:"-"`
}

// ListedAwareness is who knows or does not know a memory.
type ListedAwareness struct {
	Kind string `json:"kind"` // knows | unaware | hide_from | allow_to
	Who  string `json:"who"`
}

// ChatMemories lists the memories of a chat's read scope, in story order;
// turns are the chat's turns (memory.Own), numbered from 1 as the
// worldline numbers them; a turn turned off is skipped (its memory is not
// read). chatKey is the chat's session key.
func (d *DB) ChatMemories(chatKey string, turns []memory.ScopedTurn) ([]ListedMemory, error) {
	return d.ChatMemoriesIn(chatKey, turns, turns)
}

// ChatMemoriesIn lists the memories of turns with dates and when they
// happened read over story, the whole read scope delivery reads (an
// attached past first, turns turned off left out): a list of some turns
// gives each memory the date injection gives it.
func (d *DB) ChatMemoriesIn(chatKey string, turns, story []memory.ScopedTurn) ([]ListedMemory, error) {
	turnOf := map[int64]int{}
	localOf := map[int64]int{}
	sourceOf := map[int64]int{}
	var jobs []int64
	for i, t := range turns {
		if t.Off {
			continue
		}
		js, _, err := d.turnJobs(t) // as delivery reads it
		if err != nil {
			return nil, err
		}
		for _, j := range js {
			turnOf[j] = i + 1
			localOf[j] = t.OwnSource().Turn
			jobs = append(jobs, j)
		}
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	// recorded dates as delivery reads them (the Day from the story's first
	// turn), and when each memory happened over the whole story
	scopeDates := map[int64]string{}
	var storyJobs []int64
	var first int64
	started := false
	for _, t := range story {
		if t.Off {
			continue
		}
		js, src, err := d.turnJobs(t)
		if err != nil {
			return nil, err
		}
		if !started && len(js) > 0 {
			first = js[0]
		}
		started = true
		for _, j := range js {
			scopeDates[j] = memory.DateText(src.Dates)
			storyJobs = append(storyJobs, j)
		}
	}
	dates := map[int64]string{}
	if len(storyJobs) > 0 {
		var err error
		if _, dates, err = d.jobDates(storyJobs, nil, scopeDates, first); err != nil {
			return nil, err
		}
	}
	every, err := d.ItemsOf(storyJobs)
	if err != nil {
		return nil, err
	}
	dated := make([]memory.KnownMemory, 0, len(every.Memories))
	for _, m := range every.Memories {
		dated = append(dated, memory.KnownMemory{ID: m.ID, Date: dates[m.Job], Memory: m.Memory})
	}
	when := memory.WhenOf(dated)
	replaced := memory.Superseded(memory.Known{Memories: dated})
	partly := memory.PartlyChanged(memory.Known{Memories: dated})
	it, err := d.ItemsOf(jobs)
	if err != nil {
		return nil, err
	}
	in, args := inJobs(jobs)
	own := map[int64]bool{}
	err = d.each(`SELECT job_id, session_key, turn_id FROM gen_jobs WHERE job_id IN `+in, args, func(rows *sql.Rows) error {
		var j int64
		var s string
		var sourceTurn int
		if err := rows.Scan(&j, &s, &sourceTurn); err != nil {
			return err
		}
		own[j] = s == chatKey
		sourceOf[j] = sourceTurn
		return nil
	})
	if err != nil {
		return nil, err
	}
	// names the memories use, whatever turn wrote them
	var refs []string
	for _, m := range it.Memories {
		refs = append(refs, listedNameRefs(m.Memory)...)
	}
	for _, a := range it.Awareness {
		refs = append(refs, a.Who)
	}
	names, types, err := d.namesAndTypes(refs)
	if err != nil {
		return nil, err
	}
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}
	aware := map[string][]ListedAwareness{}
	secret := map[string]bool{}
	for _, a := range it.Awareness {
		target := a.Target
		if i := strings.Index(target, "."); i > 0 {
			target = target[:i] // an effect's awareness is its memory's
		}
		aware[target] = append(aware[target], ListedAwareness{Kind: a.Kind, Who: name(a.Who)})
		if a.Kind == "hide_from" || a.Kind == "unaware" {
			secret[target] = true
		}
	}
	holds, err := d.holdsOf(jobs)
	if err != nil {
		return nil, err
	}
	out := make([]ListedMemory, 0, len(it.Memories))
	for _, m := range it.Memories {
		v := listedMemory(m.Memory, name)
		v.ID, v.Turn = m.ID, turnOf[m.Job]
		v.TurnID, v.SourceTurn = localOf[m.Job], sourceOf[m.Job]
		v.Secret, v.Inherited, v.Aware = secret[m.ID], !own[m.Job], aware[m.ID]
		v.Date, v.When = dates[m.Job], when[m.ID]
		v.Replaced, v.Changed = replaced[m.ID], partly[m.ID]
		v.Hold = holds[m.ID]
		// the types of the names the by-name view files it under: its
		// person and the names its effects change (user 11-09: 이름별 tabs
		// show a name's kind and its changed values)
		// by reference, not by the name shown: two names may read the same,
		// and a name may read like a record (review 11-09 A2, A3)
		for _, ref := range append([]string{m.About, m.Owner}, effectAbouts(m.Memory)...) {
			if _, ok := names[ref]; ok { // a name, not a record (M12) left as it is
				if v.NameTypes == nil {
					v.NameTypes = map[string]string{}
				}
				v.NameTypes[ref] = types[ref]
			}
		}
		v.EffectRefs = effectAbouts(m.Memory)
		out = append(out, v)
	}
	// Materializing an inherited past issues IDs after the child's later
	// records. The list still follows story order, not allocation order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Turn < out[j].Turn })
	return out, nil
}

// ownMemory reads a memory of the chat for a change, locked.
func ownMemory(tx *sql.Tx, chatKey, id string) (int64, int64, memory.Memory, error) {
	var m memory.Memory
	n, err := strconv.ParseInt(strings.TrimPrefix(id, "M"), 10, 64)
	if err != nil || !strings.HasPrefix(id, "M") {
		return 0, 0, m, ErrMemoryNotFound
	}
	var session, detail string
	var job int64
	err = tx.QueryRow(`SELECT session_key, job_id, detail FROM memories WHERE id=? FOR UPDATE`, n).Scan(&session, &job, &detail)
	if err == sql.ErrNoRows {
		return 0, 0, m, ErrMemoryNotFound
	}
	if err != nil {
		return 0, 0, m, err
	}
	if session != chatKey {
		return 0, 0, m, ErrMemoryNotFound // another chat's
	}
	if err := json.Unmarshal([]byte(detail), &m); err != nil {
		return 0, 0, m, fmt.Errorf("memory %d detail: %w", n, err)
	}
	return n, job, m, nil
}

// unlinkVectors leaves the memories of a job without a vector until the
// next index of the chat: a memory's text changed or a memory left, and a
// contextualized vector is made from the turn's memories together (embed
// indexGroups), so none of them holds its vector any more. A text whose
// vector is stored already is not sent again.
func unlinkVectors(tx *sql.Tx, job int64) error {
	_, err := tx.Exec(`UPDATE memories SET doc_hash='' WHERE job_id=?`, job)
	return err
}

// EditMemory changes a memory's body and importance (nil = kept). Its
// search words are made again; its turn's vectors are made again by the
// next index of the chat (unlinkVectors).
func (d *DB) EditMemory(chatKey, id string, body *string, imp *float64) error {
	_, err := d.EditMemoryPatch(chatKey, id, body, imp, nil)
	return err
}

// DeleteMemory deletes a memory of the chat with its effects, its earlier
// records list, the chat's awareness of it and its search entries; its
// turn's other memories wait for their vectors again (unlinkVectors). Records of other turns that
// point to it keep the reference, which then reads as nothing.
func (d *DB) DeleteMemory(chatKey, id string) error {
	return d.DeleteMemories(chatKey, []string{id})
}

// DeleteMemories uses the single-memory deletion in one transaction, so a
// failed selection leaves every selected memory intact.
func (d *DB) DeleteMemories(chatKey string, ids []string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if err := deleteMemory(tx, chatKey, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func deleteMemory(tx *sql.Tx, chatKey, id string) error {
	n, job, _, err := ownMemory(tx, chatKey, id)
	if err != nil {
		return err
	}
	if err := unlinkVectors(tx, job); err != nil {
		return err
	}
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`DELETE FROM memories WHERE id=?`, []any{n}},
		{`DELETE FROM effects WHERE memory_id=?`, []any{n}},
		{`DELETE FROM memory_was WHERE memory_id=?`, []any{n}},
		{`DELETE FROM awareness WHERE session_key=? AND (target=? OR target LIKE ?)`, []any{chatKey, id, id + ".%"}},
		{`DELETE FROM search_words WHERE session_key=? AND item=?`, []any{chatKey, id}},
		{`DELETE FROM search_refs WHERE session_key=? AND item=?`, []any{chatKey, id}},
	} {
		if _, err := tx.Exec(s.q, s.args...); err != nil {
			return err
		}
	}
	return nil
}

// MemoryCounts counts the memories of a chat's turns (memory.Own), by turn
// place from 1, choosing each turn's job as delivery does (turnJobs); a
// turn turned off has none. The chat's and its ancestors' done jobs are read
// in one query and the memories counted in another, so a long chat's map
// does not read the memories themselves.
func (d *DB) MemoryCounts(turns []memory.ScopedTurn) (map[int]int, error) {
	sessions := map[string]bool{}
	for _, t := range turns {
		sessions[t.Source.Session], sessions[t.OwnSource().Session] = true, true
	}
	if len(sessions) == 0 {
		return map[int]int{}, nil
	}
	var args []any
	for s := range sessions {
		args = append(args, s)
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	type key struct {
		session string
		turn    int
		source  string
	}
	done := map[key]int64{}
	err := d.each(`SELECT job_id, session_key, turn_id, source_key FROM gen_jobs WHERE state='done' AND session_key IN `+in, args, func(rows *sql.Rows) error {
		var j int64
		var k key
		if err := rows.Scan(&j, &k.session, &k.turn, &k.source); err != nil {
			return err
		}
		done[k] = j
		return nil
	})
	if err != nil {
		return nil, err
	}
	turnOf := map[int64]int{}
	var jobs []int64
	for i, t := range turns {
		if t.Off {
			continue
		}
		j, ok := done[key{t.Source.Session, t.Source.Turn, t.Source.Key()}]
		if !ok && t.Inherited {
			own := t.OwnSource()
			j, ok = done[key{own.Session, own.Turn, own.Key()}]
		}
		if ok {
			turnOf[j] = i + 1
			jobs = append(jobs, j)
		}
	}
	out := map[int]int{}
	if len(jobs) == 0 {
		return out, nil
	}
	jin, jargs := inJobs(jobs)
	err = d.each(`SELECT job_id, COUNT(*) FROM memories WHERE job_id IN `+jin+` GROUP BY job_id`, jargs, func(rows *sql.Rows) error {
		var j int64
		var n int
		if err := rows.Scan(&j, &n); err != nil {
			return err
		}
		out[turnOf[j]] += n
		return nil
	})
	return out, err
}
