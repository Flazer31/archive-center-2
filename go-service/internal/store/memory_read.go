package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// CurrentJobs returns the done jobs whose source is the turn's current
// source: a turn rerolled or edited since has a new source, so its earlier
// job is not current (its rows stay as history).
func (d *DB) CurrentJobs(sources []memory.Source) ([]int64, error) {
	ids, err := d.jobsForSources(sources)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, id := range ids {
		if id != 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

// Keep source order (and missing places) while looking up all sources once.
func (d *DB) jobsForSources(sources []memory.Source) ([]int64, error) {
	keys := make([]string, len(sources))
	byKey := map[string]int64{}
	for start := 0; start < len(sources); start += 250 {
		end := min(start+250, len(sources))
		args := make([]any, 0, (end-start)*3)
		for i := start; i < end; i++ {
			s := sources[i]
			keys[i] = s.Key()
			args = append(args, s.Session, s.Turn, keys[i])
		}
		q := `SELECT job_id, source_key FROM gen_jobs WHERE state='done' AND (session_key,turn_id,source_key) IN (` + strings.TrimSuffix(strings.Repeat("(?,?,?),", end-start), ",") + ")"
		if err := d.each(q, args, func(rows *sql.Rows) error {
			var id int64
			var key string
			if err := rows.Scan(&id, &key); err != nil {
				return err
			}
			byKey[key] = id
			return nil
		}); err != nil {
			return nil, err
		}
	}
	out := make([]int64, len(keys))
	for i, key := range keys {
		out[i] = byKey[key]
	}
	return out, nil
}

// JobSources reads the source each job read.
func (d *DB) JobSources(jobs []int64) (map[int64]memory.Source, error) {
	out := map[int64]memory.Source{}
	in, args := inJobs(jobs)
	err := d.each(`SELECT job_id, source FROM gen_jobs WHERE job_id IN `+in, args, func(rows *sql.Rows) error {
		var j int64
		var src string
		if err := rows.Scan(&j, &src); err != nil {
			return err
		}
		var s memory.Source
		if err := json.Unmarshal([]byte(src), &s); err != nil {
			return fmt.Errorf("job %d source: %w", j, err)
		}
		out[j] = s
		return nil
	})
	return out, err
}

// DoneKeys returns the source keys of the chat's done jobs.
func (d *DB) DoneKeys(session string) (map[string]bool, error) {
	out := map[string]bool{}
	err := d.each(`SELECT source_key FROM gen_jobs WHERE session_key=? AND state='done'`, []any{session}, func(rows *sql.Rows) error {
		var k string
		if err := rows.Scan(&k); err != nil {
			return err
		}
		out[k] = true
		return nil
	})
	return out, err
}

// StoredMemory is a memory as stored, with its persistent id and job.
type StoredMemory struct {
	ID  string
	Job int64
	memory.Memory
	BodyWords []string
}

// Items are the stored rows of some jobs.
type Items struct {
	Names     []StoredName
	Relations []StoredRelation
	Memories  []StoredMemory
	Mentions  []StoredMention
	Awareness []StoredAwareness
	Links     []StoredLink
	Scenes    []StoredSceneMark
}

type StoredName struct {
	ID, Name, Type, Q string
	Job               int64
}

type StoredRelation struct {
	ID, Name, SameAs, How, Q string
	Job                      int64
}

type StoredMention struct {
	Target, Q string
	Job       int64
}

type StoredAwareness struct {
	Target, Kind string
	Job          int64
	memory.Knows
}

func inJobs(jobs []int64) (string, []any) {
	if len(jobs) == 0 {
		return "(NULL)", nil
	}
	args := make([]any, len(jobs))
	for i, j := range jobs {
		args[i] = j
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?,", len(jobs)), ",") + ")", args
}

// ItemsOf reads everything the given jobs stored, in insertion order.
func (d *DB) ItemsOf(jobs []int64) (Items, error) {
	var it Items
	in, args := inJobs(jobs)
	err := d.each(`SELECT id, job_id, name, type, q FROM names WHERE job_id IN `+in+` ORDER BY id`, args, func(rows *sql.Rows) error {
		var n StoredName
		var id int64
		if err := rows.Scan(&id, &n.Job, &n.Name, &n.Type, &n.Q); err != nil {
			return err
		}
		n.ID = fmt.Sprintf("N%d", id)
		it.Names = append(it.Names, n)
		return nil
	})
	if err != nil {
		return it, err
	}
	err = d.each(`SELECT id, job_id, name_ref, same_as, how, q FROM name_relations WHERE job_id IN `+in+` ORDER BY id`, args, func(rows *sql.Rows) error {
		var r StoredRelation
		var id int64
		if err := rows.Scan(&id, &r.Job, &r.Name, &r.SameAs, &r.How, &r.Q); err != nil {
			return err
		}
		r.ID = fmt.Sprintf("R%d", id)
		it.Relations = append(it.Relations, r)
		return nil
	})
	if err != nil {
		return it, err
	}
	err = d.each(`SELECT id, job_id, detail, read_words FROM memories WHERE job_id IN `+in+` ORDER BY id`, args, func(rows *sql.Rows) error {
		var m StoredMemory
		var id int64
		var detail string
		var words sql.NullString
		if err := rows.Scan(&id, &m.Job, &detail, &words); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(detail), &m.Memory); err != nil {
			return fmt.Errorf("memory %d detail: %w", id, err)
		}
		if words.Valid {
			// A missing/old projection takes the original calculation path.
			_ = json.Unmarshal([]byte(words.String), &m.BodyWords)
		}
		if m.BodyWords == nil {
			m.BodyWords = memory.Words(m.Body)
		}
		m.ID = fmt.Sprintf("M%d", id)
		it.Memories = append(it.Memories, m)
		return nil
	})
	if err != nil {
		return it, err
	}
	err = d.each(`SELECT job_id, target, q FROM mentions WHERE job_id IN `+in+` ORDER BY id`, args, func(rows *sql.Rows) error {
		var m StoredMention
		if err := rows.Scan(&m.Job, &m.Target, &m.Q); err != nil {
			return err
		}
		it.Mentions = append(it.Mentions, m)
		return nil
	})
	if err != nil {
		return it, err
	}
	err = d.each(`SELECT job_id, target, kind, who, via, from_who, extent, part, ref, q FROM awareness WHERE job_id IN `+in+` ORDER BY id`, args, func(rows *sql.Rows) error {
		var a StoredAwareness
		if err := rows.Scan(&a.Job, &a.Target, &a.Kind, &a.Who, &a.Via, &a.From, &a.Extent, &a.Part, &a.Ref, &a.Q); err != nil {
			return err
		}
		it.Awareness = append(it.Awareness, a)
		return nil
	})
	if err != nil {
		return it, err
	}
	err = d.each(`SELECT id, job_id, from_ref, rel, to_ref, q FROM links WHERE job_id IN `+in+` ORDER BY id`, args, func(rows *sql.Rows) error {
		var l StoredLink
		var id int64
		if err := rows.Scan(&id, &l.Job, &l.From, &l.Rel, &l.To, &l.Q); err != nil {
			return err
		}
		l.ID = fmt.Sprintf("L%d", id)
		it.Links = append(it.Links, l)
		return nil
	})
	if err != nil {
		return it, err
	}
	err = d.each(`SELECT job_id, scene, change_kind, q FROM scene_marks WHERE job_id IN `+in+` ORDER BY id`, args, func(rows *sql.Rows) error {
		var sc StoredSceneMark
		if err := rows.Scan(&sc.Job, &sc.Ref, &sc.Change, &sc.Q); err != nil {
			return err
		}
		it.Scenes = append(it.Scenes, sc)
		return nil
	})
	return it, err
}

// StoredSceneMark is a scene mark with the job that wrote it (its turn
// gives the story order, whatever order the jobs finished in).
type StoredSceneMark struct {
	Job int64
	memory.Scene
}

// StoredLink is a meaning link with the job that wrote it.
type StoredLink struct {
	Job int64
	memory.Link
}

// KnownOf gives the stored items of the given jobs as generation input, each
// with the turn its job read. An arbitrary job subset has no story origin,
// so only calendar dates are hydrated here; scoped readers also compute Day.
func (d *DB) KnownOf(jobs []int64) (memory.Known, error) {
	return d.knownOfAt(jobs, nil, nil, 0)
}

// knownOfAt is KnownOf with each job's turn given by at when at is set (a
// read scope's story order, where a branch's copied parent turns keep the
// parent's turn numbers).
func (d *DB) knownOfAt(jobs []int64, at map[int64]int, scopeDates map[int64]string, first int64) (memory.Known, error) {
	var k memory.Known
	it, err := d.ItemsOf(jobs)
	if err != nil {
		return k, err
	}
	turn, date, err := d.jobDates(jobs, at, scopeDates, first)
	if err != nil {
		return k, err
	}
	for _, n := range it.Names {
		k.Names = append(k.Names, memory.KnownName{ID: n.ID, Name: n.Name, Type: n.Type, Q: n.Q, Turn: turn[n.Job]})
	}
	for _, r := range it.Relations {
		how := r.How
		if how == "user_alias" {
			how = "alias"
		}
		k.Relations = append(k.Relations, memory.KnownRelation{ID: r.ID, Name: r.Name, SameAs: r.SameAs, How: how, Q: r.Q, Turn: turn[r.Job]})
	}
	for _, m := range it.Memories {
		k.Memories = append(k.Memories, memory.KnownMemory{ID: m.ID, Turn: turn[m.Job], Job: m.Job, Date: date[m.Job], Memory: m.Memory, BodyWords: m.BodyWords})
	}
	for _, a := range it.Awareness {
		k.Awareness = append(k.Awareness, memory.KnownAwareness{Target: a.Target, Kind: a.Kind, Turn: turn[a.Job], Knows: a.Knows})
	}
	for _, l := range it.Links {
		k.Links = append(k.Links, memory.KnownLink{ID: l.ID, From: l.From, Rel: l.Rel, To: l.To, Turn: turn[l.Job]})
	}
	// story order: by the turn each mark was written for, then as stored
	for _, sc := range it.Scenes {
		k.Scenes = append(k.Scenes, memory.KnownScene{ID: sc.Ref, Change: sc.Change, Turn: turn[sc.Job]})
	}
	sort.SliceStable(k.Scenes, func(i, j int) bool { return k.Scenes[i].Turn < k.Scenes[j].Turn })
	if err := d.hydrateUserAliases(&k); err != nil {
		return k, err
	}
	return k, nil
}

// jobDates gives each job's turn (its place in at when at is set) and its
// recorded date: the turn's story date as the turn holds it now (a date seen
// after the memory was made is read here, not made again), else, for a
// read scope (scopeDates set, jobs in story order, first the scope's first
// job), the Day counted from the scope's start. knownOfAt and the memory
// list (ChatMemories) read dates through it.
func (d *DB) jobDates(jobs []int64, at map[int64]int, scopeDates map[int64]string, first int64) (map[int64]int, map[int64]string, error) {
	var err error
	turn := map[int64]int{}
	in, args := inJobs(jobs)
	// the story date of each job's turn as the turn holds it now (a date seen
	// after the memory was made is read here, not made again)
	date := map[int64]string{}
	deltas := map[int64]sql.NullInt64{}
	legacy := map[int64]bool{}
	err = d.each(`SELECT g.job_id, g.turn_id, t.dates, JSON_UNQUOTE(JSON_EXTRACT(g.source, '$.dates.body')), g.elapsed_days, g.state FROM gen_jobs g LEFT JOIN logical_turns t ON t.session_key=g.session_key AND t.turn_id=g.turn_id WHERE g.job_id IN `+in, args, func(rows *sql.Rows) error {
		var j int64
		var t int
		var dates, bodyDate, state sql.NullString
		var delta sql.NullInt64
		if err := rows.Scan(&j, &t, &dates, &bodyDate, &delta, &state); err != nil {
			return err
		}
		deltas[j] = delta
		// made before 0.0.4: the days that passed in it are not known
		legacy[j] = !delta.Valid && state.String == "done"
		turn[j] = t
		if at != nil {
			turn[j] = at[j]
		}
		if dates.Valid {
			var m map[string]string
			json.Unmarshal([]byte(dates.String), &m)
			date[j] = memory.DateText(m)
		}
		if scopeDates != nil {
			date[j] = scopeDates[j]
		}
		if date[j] == "" && bodyDate.Valid && bodyDate.String != "" {
			date[j] = memory.DateText(map[string]string{"body": bodyDate.String})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if scopeDates != nil {
		// Scoped jobs are in canonical story order, including jobs with no
		// memory items. Only the scope's first source establishes Day 0;
		// an unmade first turn must not reset a later made turn's delta.
		// User decision 11-05 ("숨기기"): once a turn made before 0.0.4 is
		// in the story, its unknown passage would make every later Day
		// wrong, so no Day is shown from there on (remaking those turns
		// fills their passage). Calendar dates are unaffected.
		var day big.Int
		unknown := false
		for _, j := range jobs {
			delta := deltas[j]
			unknown = unknown || legacy[j]
			if j != first && delta.Valid && delta.Int64 > 0 {
				day.Add(&day, big.NewInt(delta.Int64))
			}
			if date[j] == "" && delta.Valid && !unknown {
				date[j] = "Day " + day.String()
			}
		}
	}
	return turn, date, nil
}

// CurrentKnown reads a chat's current memory: the current jobs of the turns
// in its read scope (memory.Scope; a branch reads its parent up to the
// branch point). It is the read later steps (search, injection, the admin
// screen) start from, the same range generation reads.
func (d *DB) CurrentKnown(s *lifecycle.Session) (memory.Known, error) {
	k, _, err := d.knownScope(memory.ScopeOf(s), false)
	return k, err
}

// NamesByID reads the names of the given name ids (N<n>), whatever job
// wrote them: a name id is issued once and names one entity.
func (d *DB) NamesByID(ids []string) (map[string]string, error) {
	out, _, err := d.namesAndTypes(ids)
	return out, err
}

// namesAndTypes is NamesByID with each name's type (person | place | thing
// | group, "" when none was written), keyed the same way.
func (d *DB) namesAndTypes(ids []string) (map[string]string, map[string]string, error) {
	out, types := map[string]string{}, map[string]string{}
	var nums []any
	for _, id := range ids {
		if len(id) > 1 && id[0] == 'N' {
			if n, err := strconv.ParseInt(id[1:], 10, 64); err == nil {
				nums = append(nums, n)
			}
		}
	}
	for start := 0; start < len(nums); start += 500 {
		part := nums[start:min(start+500, len(nums))]
		err := d.each(`SELECT id, name, type FROM names WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")+`)`, part, func(rows *sql.Rows) error {
			var id int64
			var name, typ string
			if err := rows.Scan(&id, &name, &typ); err != nil {
				return err
			}
			out[fmt.Sprintf("N%d", id)] = name
			types[fmt.Sprintf("N%d", id)] = typ
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return out, types, nil
}

// Candidates is the injection search: the Select generation reads with,
// over the request's text (the current input and the recent turns the host
// sent), against the chat's current memory. window is how many of the
// chat's latest turns that text holds; word rarity is counted over the
// turns before them, as generation counts it over the turns before the one
// it reads. The result is a reading range for the delivery step, not what
// is delivered.
func (d *DB) Candidates(s *lifecycle.Session, text string, window int) (memory.Selection, memory.Known, error) {
	k, err := d.CurrentKnown(s)
	if err != nil {
		return memory.Selection{}, k, err
	}
	scope := memory.ScopeOf(s)
	var earlier []string
	for _, t := range scope[:max(len(scope)-window, 0)] {
		earlier = append(earlier, memory.TurnText(t.Source))
	}
	return memory.Select(k, text, earlier), k, nil
}

// KnownFor reads the current memory of the turns of a read scope, each
// memory dated by its source's dates: the turn's dates as the reading chat
// holds them (for a branch, the dates it saw on its copy of a parent turn,
// not the parent's later ones).
func (d *DB) KnownFor(sources []memory.Source) (memory.Known, error) {
	ids, err := d.jobsForSources(sources)
	if err != nil {
		return memory.Known{}, err
	}
	var jobs []int64
	var first int64
	dates := map[int64]string{}
	for i, s := range sources {
		if id := ids[i]; id != 0 {
			if i == 0 {
				first = id
			}
			jobs = append(jobs, id)
			dates[id] = memory.DateText(s.Dates)
		}
	}
	return d.knownOfAt(jobs, nil, dates, first)
}

// KnownForStory is KnownFor for a chat with an attached past (C22): the
// first past sources are another chat's turns, numbered 1..past in story
// order, and the chat's own turns come after them (their turn + past), so
// the past's turns read as earlier than the chat's own.
func (d *DB) KnownForStory(sources []memory.Source, past int) (memory.Known, error) {
	ids, err := d.jobsForSources(sources)
	if err != nil {
		return memory.Known{}, err
	}
	var jobs []int64
	var first int64
	dates := map[int64]string{}
	at := map[int64]int{}
	for i, s := range sources {
		if id := ids[i]; id != 0 {
			if i == 0 {
				first = id
			}
			jobs = append(jobs, id)
			dates[id] = memory.DateText(s.Dates)
			at[id] = s.Turn + past
			if i < past {
				at[id] = i + 1
			}
		}
	}
	return d.knownOfAt(jobs, at, dates, first)
}

// ItemIDs gives the persistent id each new item of a job's answer got, by
// array and place ("memories/0" -> "M12").
func (d *DB) ItemIDs(job int64) (map[string]string, error) {
	out := map[string]string{}
	err := d.each(`SELECT arr, pos, item_id FROM gen_items WHERE job_id=?`, []any{job}, func(rows *sql.Rows) error {
		var arr, id string
		var pos int
		if err := rows.Scan(&arr, &pos, &id); err != nil {
			return err
		}
		out[fmt.Sprintf("%s/%d", arr, pos)] = id
		return nil
	})
	return out, err
}

// UnreadAnswers gives the answers of a job in which nothing could be read,
// in the order they came.
func (d *DB) UnreadAnswers(job int64) ([]string, error) {
	var raw sql.NullString
	if err := d.sql.QueryRow(`SELECT unread_answers FROM gen_jobs WHERE job_id=?`, job).Scan(&raw); err != nil {
		return nil, err
	}
	var out []string
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Grant records history turns of a chat the user asked to make.
func (d *DB) Grant(session string, turns []int) error {
	tx, err := d.sql.Begin() // all asked turns or none
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range turns {
		if _, err := tx.Exec(`INSERT IGNORE INTO gen_grants (session_key, turn_id, created_at) VALUES (?,?,?)`, session, t, time.Now().UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Grants gives the history turns of a chat still asked for.
func (d *DB) Grants(session string) (map[int]bool, error) {
	out := map[int]bool{}
	err := d.each(`SELECT turn_id FROM gen_grants WHERE session_key=?`, []any{session}, func(rows *sql.Rows) error {
		var t int
		if err := rows.Scan(&t); err != nil {
			return err
		}
		out[t] = true
		return nil
	})
	return out, err
}

// Ungrant drops a grant once its turn is made.
func (d *DB) Ungrant(session string, turn int) error {
	// A remake can grant this turn while its previous call is still
	// finishing. Completing that retired job must leave the new grant for
	// the new job; grant completion follows the latest job of the turn.
	_, err := d.sql.Exec(`DELETE FROM gen_grants WHERE session_key=? AND turn_id=?
		AND (SELECT state FROM gen_jobs WHERE session_key=? AND turn_id=? ORDER BY job_id DESC LIMIT 1)='done'`, session, turn, session, turn)
	return err
}

// SessionKeys gives every stored chat.
func (d *DB) SessionKeys() ([]string, error) {
	var out []string
	err := d.each(`SELECT DISTINCT session_key FROM logical_turns ORDER BY session_key`, nil, func(rows *sql.Rows) error {
		var k string
		if err := rows.Scan(&k); err != nil {
			return err
		}
		out = append(out, k)
		return nil
	})
	return out, err
}

// Branch is a chat branched from another: its key, the parent's row it
// split at, and its active turns.
type Branch struct {
	Key     string
	Message string
	Turns   int
}

// BranchesOf lists the chats whose parent is now the given chat.
func (d *DB) BranchesOf(parent string) ([]Branch, error) {
	var out []Branch
	err := d.each(`SELECT b.session_key, b.parent_message,
		(SELECT COUNT(*) FROM logical_turns t WHERE t.session_key=b.session_key AND NOT t.inactive AND NOT t.greeting)
		FROM session_branch b WHERE b.parent_chat=? ORDER BY b.session_key`, []any{parent}, func(rows *sql.Rows) error {
		var b Branch
		if err := rows.Scan(&b.Key, &b.Message, &b.Turns); err != nil {
			return err
		}
		out = append(out, b)
		return nil
	})
	return out, err
}

// TurnJob is a turn's latest generation job as the cold start shows it.
type TurnJob struct {
	State    string
	Attempts int
	Notes    []string
}

// TurnJobs gives each turn of a chat its latest job.
func (d *DB) TurnJobs(session string) (map[int]TurnJob, error) {
	out := map[int]TurnJob{}
	err := d.each(`SELECT turn_id, state, attempts, COALESCE(notes,'') FROM gen_jobs WHERE session_key=? ORDER BY job_id`, []any{session}, func(rows *sql.Rows) error {
		var t int
		var j TurnJob
		var notes string
		if err := rows.Scan(&t, &j.State, &j.Attempts, &notes); err != nil {
			return err
		}
		if notes != "" {
			_ = json.Unmarshal([]byte(notes), &j.Notes)
		}
		out[t] = j
		return nil
	})
	return out, err
}

// UngrantAll drops every history turn of a chat still asked for (the cold
// start cancelled); a turn being made now finishes.
func (d *DB) UngrantAll(session string) (int, error) {
	r, err := d.sql.Exec(`DELETE FROM gen_grants WHERE session_key=?`, session)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}
