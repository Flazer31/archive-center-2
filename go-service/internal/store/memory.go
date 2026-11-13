package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"archivecenter2/goservice/internal/memory"
)

// Job is one generation job: one turn read at one source. A job whose
// answer was received but not stored yet (state received) carries what was
// sent and the answer, so the answer is stored without calling the model
// again.
type Job struct {
	ID        int64
	Session   string
	Turn      int
	SourceKey string
	State     string
	Attempts  int       // model calls started
	NextAt    time.Time // no new call before this (zero: no wait)
	Model     string
	Request   string
	ReadItems string
	Response  string
	Notes     string // the notes kept with the job (JSON), e.g. why its last call failed
}

// JobFor returns the active job of a source, creating it once; a resent or
// retried request reuses that job. An explicit RemakeGeneration retires it
// and gives the source a new active job.
func (d *DB) JobFor(src memory.Source) (Job, error) {
	b, _ := json.Marshal(src)
	key := src.Key()
	if _, err := d.sql.Exec(`INSERT IGNORE INTO gen_jobs (session_key, turn_id, source_key, source, state, created_at) VALUES (?,?,?,?,?,?)`,
		src.Session, src.Turn, key, string(b), "requested", time.Now().UTC()); err != nil {
		return Job{}, err
	}
	j := Job{Session: src.Session, Turn: src.Turn, SourceKey: key}
	var next sql.NullTime
	err := d.sql.QueryRow(`SELECT job_id, state, attempts, next_at, model, COALESCE(request,''), COALESCE(read_items,''), COALESCE(response,''), COALESCE(notes,'') FROM gen_jobs WHERE session_key=? AND turn_id=? AND source_key=?`,
		src.Session, src.Turn, key).Scan(&j.ID, &j.State, &j.Attempts, &next, &j.Model, &j.Request, &j.ReadItems, &j.Response, &j.Notes)
	if next.Valid {
		j.NextAt = next.Time
	}
	if j.State != "received" && j.State != "unreadable" {
		j.Model, j.Request, j.ReadItems, j.Response = "", "", "", ""
	}
	return j, err
}

var bodyDatePattern = regexp.MustCompile(`\b[0-9]{4}[-/.][0-9]{1,2}[-/.][0-9]{1,2}(?:[ T][0-9]{1,2}:[0-9]{2}(?::[0-9]{2})?)?\b`)

// sourceWithBodyDate records the last explicit calendar date in this turn
// when no observed variable supplied one. It only enriches a job's source
// at its first memory save; SourceOf, logical turns and completed jobs are
// not backfilled.
// This is the recording date, not each recalled event's occurrence time.
func sourceWithBodyDate(src memory.Source) memory.Source {
	if len(src.Dates) != 0 {
		return src
	}
	date := ""
	read := func(text string) {
		// numeric dates as written; else a Korean one, as numbers
		if dates := bodyDatePattern.FindAllString(text, -1); len(dates) != 0 {
			date = dates[len(dates)-1]
		} else if dates := memory.KoreanDates(text); len(dates) != 0 {
			date = dates[len(dates)-1]
		}
	}
	for _, m := range src.Members {
		read(m.Data)
	}
	for _, r := range src.Responses {
		read(r.Data)
	}
	if date != "" {
		src.Dates = map[string]string{"body": date}
	}
	return src
}

// KeepUnreadable records an answer in which no response object could be
// read: the answer is kept as received (also in unread_answers, which later
// answers never overwrite), and the job waits until retryAt to be read again
// or made again. A job already done is not touched.
func (d *DB) KeepUnreadable(j Job, model, request, readItems, response string, notes []string, retryAt time.Time) error {
	n, _ := json.Marshal(notes)
	_, err := d.sql.Exec(`UPDATE gen_jobs SET state='unreadable', model=?, request=?, read_items=?, response=?, notes=?, next_at=?,
		unread_answers=JSON_ARRAY_APPEND(COALESCE(unread_answers,'[]'), '$', ?) WHERE job_id=? AND state<>'done'`,
		model, request, readItems, response, string(n), retryAt.UTC(), response, j.ID)
	return err
}

// StartAttempt counts a model call as it starts, so a call cut off by a
// stop still counts. A job already done is not touched.
func (d *DB) StartAttempt(j Job) error {
	_, err := d.sql.Exec(`UPDATE gen_jobs SET attempts=attempts+1, next_at=NULL WHERE job_id=? AND state<>'done'`, j.ID)
	return err
}

// RearmFailedJobs gives the jobs that have no answer yet their model calls
// again (the critic was changed in the settings): their attempts and wait
// are reset. Done jobs and jobs holding an answer are not touched. It
// returns how many jobs were reset.
func (d *DB) RearmFailedJobs() (int64, error) {
	res, err := d.sql.Exec(`UPDATE gen_jobs SET attempts=0, next_at=NULL WHERE state IN ('requested','failed','unreadable') AND attempts>0`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// KeepResponse records a received answer before it is stored, so a failed
// store is retried from it without calling the model again. A job already
// done is not touched.
func (d *DB) KeepResponse(j Job, model, request, readItems, response string) error {
	_, err := d.sql.Exec(`UPDATE gen_jobs SET state='received', model=?, request=?, read_items=?, response=? WHERE job_id=? AND state<>'done'`,
		model, request, readItems, response, j.ID)
	return err
}

// Generation is what a job sent and got back.
type Generation struct {
	Model     string
	Request   string // exact model input
	ReadItems string // stored items shown to the model
	Response  string // raw model output
	Output    memory.Output
	Notes     []string // parse diagnostics
}

// FailJob records a generation that did not finish (the provider failed):
// what was sent and why, and when the next call may start (the wait counts
// from the failure). The job stays open; a later run of the same source
// reuses it. A job already done, or one whose answer was received, is not
// touched.
func (d *DB) FailJob(j Job, model, request, readItems, reason string, retryAt time.Time) error {
	notes, _ := json.Marshal([]string{reason})
	_, err := d.sql.Exec(`UPDATE gen_jobs SET state='failed', model=?, request=?, read_items=?, notes=?, next_at=? WHERE job_id=? AND state NOT IN ('done','received')`,
		model, request, readItems, string(notes), retryAt.UTC(), j.ID)
	return err
}

var tempID = regexp.MustCompile(`^[a-z]+\d+(\.\d+)?$`)

// mention reports a memory item that is not a new record: a re-mention or
// awareness update of a stored item (no id). An item without id that has
// content of its own (a kind or a body) is a new record and gets an id.
func mention(m memory.Memory) bool {
	return m.ID == "" && (m.Ref != "" || (m.Kind == "" && m.Body == ""))
}

// SaveGeneration stores a job's result in one transaction: every new item
// gets a persistent id, temporary ids inside the output are replaced, and
// every row carries the job. A job already done is left as it is (a resent
// result adds nothing). References that resolve to nothing are kept as
// written and noted, never attached to another item.
func (d *DB) SaveGeneration(j Job, g Generation) ([]string, error) {
	// two saves at once can deadlock (MariaDB 10.11 does where 11.8 does
	// not); the save is one transaction, rolled back whole, so it runs again
	return againOnConflict(func() ([]string, error) { return d.saveGeneration(j, g) })
}

func (d *DB) saveGeneration(j Job, g Generation) (notes []string, err error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state, source string
	if err := tx.QueryRow(`SELECT state, source FROM gen_jobs WHERE job_id=? FOR UPDATE`, j.ID).Scan(&state, &source); err != nil {
		return nil, err
	}
	if state == "done" {
		return nil, nil
	}
	var src memory.Source
	_ = json.Unmarshal([]byte(source), &src)
	if len(src.Dates) == 0 {
		if dated := sourceWithBodyDate(src); len(dated.Dates) != 0 {
			b, _ := json.Marshal(dated)
			if _, err := tx.Exec(`UPDATE gen_jobs SET source=? WHERE job_id=?`, string(b), j.ID); err != nil {
				return nil, err
			}
		}
	}
	notes = append(notes, g.Notes...)
	ids := map[string]string{} // temporary id -> persistent id
	// a temporary id written twice cannot be resolved to either item
	seen := map[string]int{}
	countID := func(id string) {
		if id != "" {
			seen[id]++
		}
	}
	for _, e := range g.Output.Entities {
		countID(e.ID)
		countID(e.Relation)
	}
	for _, sc := range g.Output.Scenes {
		if sc.Ref == "" {
			countID(sc.ID)
		}
	}
	for _, m := range g.Output.Memories {
		countID(m.ID)
	}
	for _, l := range g.Output.Links {
		countID(l.ID)
	}
	setID := func(temp, persistent string) {
		if temp == "" {
			return
		}
		if seen[temp] > 1 {
			if _, done := ids[temp]; !done {
				notes = append(notes, "ambiguous temporary id "+temp+" (written more than once)")
			}
			ids[temp] = "" // marked: not resolvable
			return
		}
		ids[temp] = persistent
	}
	effects := map[string]int{} // temporary memory id -> number of effects
	insert := func(q string, args ...any) (int64, error) {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	out := g.Output
	// each new item's issued id, by array and place (gen_items)
	issued := func(arr string, pos int, temp, item string) error {
		_, err := tx.Exec(`INSERT INTO gen_items (job_id, arr, pos, temp_id, item_id) VALUES (?,?,?,?,?)`, j.ID, arr, pos, temp, item)
		return err
	}

	// 1. names, identity relations, scenes, links and new memories get ids first
	var nameIDs []string
	for i, e := range out.Entities {
		id, err := insert(`INSERT INTO names (session_key, job_id, name, type, q) VALUES (?,?,?,?,?)`, j.Session, j.ID, e.Name, e.Type, e.Q)
		if err != nil {
			return nil, fmt.Errorf("name: %w", err)
		}
		nameIDs = append(nameIDs, fmt.Sprintf("N%d", id))
		setID(e.ID, fmt.Sprintf("N%d", id))
		if err := issued("entities", i, e.ID, nameIDs[i]); err != nil {
			return nil, err
		}
	}
	for i, sc := range out.Scenes {
		if sc.ID != "" && sc.Ref == "" {
			id, err := insert(`INSERT INTO scenes (session_key, job_id) VALUES (?,?)`, j.Session, j.ID)
			if err != nil {
				return nil, fmt.Errorf("scene: %w", err)
			}
			setID(sc.ID, fmt.Sprintf("S%d", id))
			if err := issued("scenes", i, sc.ID, fmt.Sprintf("S%d", id)); err != nil {
				return nil, err
			}
		}
	}
	memIDs := make([]int64, len(out.Memories))
	for i, m := range out.Memories {
		if mention(m) {
			continue // a re-mention or an awareness update of a stored item
		}
		id, err := insert(`INSERT INTO memories (session_key, job_id, kind, body, q, detail) VALUES (?,?,?,?,?,?)`, j.Session, j.ID, m.Kind, m.Body, m.Q, "{}")
		if err != nil {
			return nil, fmt.Errorf("memory: %w", err)
		}
		memIDs[i] = id
		setID(m.ID, fmt.Sprintf("M%d", id))
		if err := issued("memories", i, m.ID, fmt.Sprintf("M%d", id)); err != nil {
			return nil, err
		}
		if seen[m.ID] == 1 {
			effects[m.ID] = len(m.Effects)
		}
	}
	linkIDs := make([]int64, len(out.Links))
	for i, l := range out.Links {
		id, err := insert(`INSERT INTO links (session_key, job_id, from_ref, rel, to_ref, q) VALUES (?,?,?,?,?,?)`, j.Session, j.ID, "", l.Rel, "", l.Q)
		if err != nil {
			return nil, fmt.Errorf("link: %w", err)
		}
		linkIDs[i] = id
		setID(l.ID, fmt.Sprintf("L%d", id))
		if err := issued("links", i, l.ID, fmt.Sprintf("L%d", id)); err != nil {
			return nil, err
		}
	}
	relIDs := make([]string, len(out.Entities)) // the relation each entity states
	relRows := make([]int64, len(out.Entities))
	for i, e := range out.Entities {
		if e.SameAs == "" {
			continue
		}
		id, err := insert(`INSERT INTO name_relations (session_key, job_id, name_ref, same_as, how, q) VALUES (?,?,?,?,?,?)`,
			j.Session, j.ID, nameIDs[i], "", e.How, e.Q)
		if err != nil {
			return nil, fmt.Errorf("relation: %w", err)
		}
		relIDs[i], relRows[i] = fmt.Sprintf("R%d", id), id
		setID(e.Relation, relIDs[i])
		if err := issued("relations", i, e.Relation, relIDs[i]); err != nil {
			return nil, err
		}
	}

	// 2. resolve references now that every new item has its id
	r := resolver{ids: ids, effects: effects}
	for i, e := range out.Entities {
		target := nameIDs[i]
		if e.SameAs != "" {
			if _, err := tx.Exec(`UPDATE name_relations SET same_as=? WHERE id=?`, r.ref(e.SameAs), relRows[i]); err != nil {
				return nil, err
			}
			target = relIDs[i] // knowledge of an identity is about the relation
		}
		if err := d.saveAwareness(tx, j, target, r.awareness(e.Awareness)); err != nil {
			return nil, err
		}
	}
	for i, m := range out.Memories {
		if m.ID == "" && m.Ref == "" && !mention(m) {
			notes = append(notes, "a memory without id was given a new id")
		}
		m = r.memory(m)
		if mention(m) {
			target := m.Ref
			if target == "" {
				notes = append(notes, "memory without id, ref or content skipped")
				continue
			}
			if m.Q != "" || isEmpty(m.Awareness) {
				if _, err := tx.Exec(`INSERT INTO mentions (session_key, job_id, target, q) VALUES (?,?,?,?)`, j.Session, j.ID, target, m.Q); err != nil {
					return nil, err
				}
				if err := indexMention(tx, j.Session, j.ID, target, m.Q); err != nil {
					return nil, err
				}
			}
			if err := d.saveAwareness(tx, j, target, m.Awareness); err != nil {
				return nil, err
			}
			continue
		}
		pid := fmt.Sprintf("M%d", memIDs[i])
		m.ID = pid
		detail, _ := json.Marshal(m)
		if _, err := tx.Exec(`UPDATE memories SET body=?, q=?, imp=?, about=?, slot=?, mode=?, status=?, owner=?, aspect=?, scene=?, detail=? WHERE id=?`,
			m.Body, m.Q, m.Imp, m.About, m.Slot, m.Mode, m.Status, m.Owner, m.Aspect, m.Scene, string(detail), memIDs[i]); err != nil {
			return nil, err
		}
		if err := indexMemory(tx, j.Session, j.ID, pid, m); err != nil {
			return nil, err
		}
		for k, w := range m.Was {
			if _, err := tx.Exec(`INSERT INTO memory_was (memory_id, ord, target) VALUES (?,?,?)`, memIDs[i], k, w); err != nil {
				return nil, err
			}
		}
		for k, e := range m.Effects {
			if _, err := tx.Exec(`INSERT INTO effects (memory_id, k, about, slot, before_value, value, delta, was, status) VALUES (?,?,?,?,?,?,?,?,?)`,
				memIDs[i], k+1, e.About, e.Slot, e.Before, e.Value, e.Delta, jsonList(e.Was), e.Status); err != nil {
				return nil, err
			}
		}
		if err := d.saveAwareness(tx, j, pid, m.Awareness); err != nil {
			return nil, err
		}
	}
	for i, l := range out.Links {
		if _, err := tx.Exec(`UPDATE links SET from_ref=?, to_ref=? WHERE id=?`, r.ref(l.From), r.ref(l.To), linkIDs[i]); err != nil {
			return nil, err
		}
		if err := d.saveAwareness(tx, j, fmt.Sprintf("L%d", linkIDs[i]), r.awareness(l.Awareness)); err != nil {
			return nil, err
		}
	}
	for _, sc := range out.Scenes {
		target := sc.Ref
		if target == "" {
			target = sc.ID
		}
		if _, err := tx.Exec(`INSERT INTO scene_marks (session_key, job_id, scene, change_kind, q) VALUES (?,?,?,?,?)`, j.Session, j.ID, r.ref(target), sc.Change, sc.Q); err != nil {
			return nil, err
		}
	}
	// Resolve user aliases after storing temporary-ID resolution so the original
	// persistent references and their materialization commit together.
	if err := resolveUserAliases(tx, &r, out); err != nil {
		return nil, err
	}
	if _, err := applyNameReferences(tx, j.ID, 0, &r); err != nil {
		return nil, err
	}
	bodyNotes, err := saveBodyEvents(tx, j, g.Output.BodyEvents, &r)
	if err != nil {
		return nil, err
	}
	notes = append(notes, bodyNotes...)
	notes = append(notes, r.notes...)
	days := g.Output.ElapsedDays
	if days < 0 {
		days = 0
	}
	var storyDate any // NULL unless the critic read a date that exists
	if d := memory.StoryDateOf(g.Output.StoryDate); d != "" {
		storyDate = d
	}
	if _, err := tx.Exec(`UPDATE gen_jobs SET state='done', model=?, request=?, read_items=?, response=?, notes=?, done_at=?, elapsed_days=?, story_date=? WHERE job_id=?`,
		g.Model, g.Request, g.ReadItems, g.Response, strings.Join(notes, "\n"), time.Now().UTC(), int64(days), storyDate, j.ID); err != nil {
		return nil, err
	}
	return notes, tx.Commit()
}

func isEmpty(a memory.Awareness) bool {
	return len(a.Knows) == 0 && len(a.Unaware) == 0 && len(a.HideFrom) == 0 && len(a.AllowTo) == 0
}

func (d *DB) saveAwareness(tx *sql.Tx, j Job, target string, a memory.Awareness) error {
	add := func(kind, who string, k memory.Knows) error {
		_, err := tx.Exec(`INSERT INTO awareness (session_key, job_id, target, kind, who, via, from_who, extent, part, ref, q) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			j.Session, j.ID, target, kind, who, k.Via, k.From, k.Extent, k.Part, k.Ref, k.Q)
		return err
	}
	for _, k := range a.Knows {
		if err := add("knows", k.Who, k); err != nil {
			return err
		}
	}
	for kind, list := range map[string][]string{"unaware": a.Unaware, "hide_from": a.HideFrom, "allow_to": a.AllowTo} {
		for _, who := range list {
			if err := add(kind, who, memory.Knows{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolver replaces temporary ids written in this output with persistent
// ids. Anything else (a persistent id shown to the model, a plain name) is
// kept as written; a temporary-looking id nobody issued is noted.
type resolver struct {
	ids     map[string]string // "" for an ambiguous temporary id
	effects map[string]int    // effects of each new memory
	notes   []string
}

func jsonList(l []string) string {
	if l == nil {
		l = []string{}
	}
	b, _ := json.Marshal(l)
	return string(b)
}

func (r *resolver) ref(s string) string {
	if s == "" {
		return s
	}
	if p, ok := r.ids[s]; ok {
		if p == "" {
			r.notes = append(r.notes, "unresolved reference "+s+" (ambiguous)")
			return s
		}
		return p
	}
	// an effect of a new memory: m3.2 -> M41.2, only for an effect it has
	if i := strings.IndexByte(s, '.'); i > 0 {
		if p, ok := r.ids[s[:i]]; ok && p != "" {
			if k, err := strconv.Atoi(s[i+1:]); err == nil && k >= 1 && k <= r.effects[s[:i]] {
				return p + s[i:]
			}
			r.notes = append(r.notes, "unresolved reference "+s+" (no such effect)")
			return s
		}
	}
	if tempID.MatchString(s) {
		r.notes = append(r.notes, "unresolved reference "+s)
	}
	return s
}

func (r *resolver) list(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = r.ref(s)
	}
	return out
}

func (r *resolver) awareness(a memory.Awareness) memory.Awareness {
	out := memory.Awareness{Unaware: r.list(a.Unaware), HideFrom: r.list(a.HideFrom), AllowTo: r.list(a.AllowTo)}
	for _, k := range a.Knows {
		k.Who, k.From, k.Ref = r.ref(k.Who), r.ref(k.From), r.ref(k.Ref)
		out.Knows = append(out.Knows, k)
	}
	return out
}

func (r *resolver) memory(m memory.Memory) memory.Memory {
	m.Ref, m.At, m.Scene = r.ref(m.Ref), r.ref(m.At), r.ref(m.Scene)
	m.About, m.Owner = r.ref(m.About), r.ref(m.Owner)
	m.To = r.list(m.To)
	m.Was = r.list(m.Was)
	m.SeenBy = r.list(m.SeenBy)
	if m.Roles != nil {
		ro := *m.Roles
		ro.Actor, ro.Target, ro.Object = r.list(ro.Actor), r.list(ro.Target), r.list(ro.Object)
		ro.Place, ro.Speaker, ro.Listener = r.list(ro.Place), r.list(ro.Speaker), r.list(ro.Listener)
		m.Roles = &ro
	}
	if len(m.Effects) > 0 {
		eff := make([]memory.Effect, len(m.Effects))
		for i, e := range m.Effects {
			e.About, e.Was = r.ref(e.About), r.list(e.Was)
			eff[i] = e
		}
		m.Effects = eff
	}
	if m.Basis != nil {
		b := *m.Basis
		b.From, b.Ref = memory.MapSource(b.From, r.ref), r.ref(b.Ref)
		m.Basis = &b
	}
	m.Awareness = r.awareness(m.Awareness)
	return m
}
