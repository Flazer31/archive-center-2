package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/memory"
)

// Body tracking (1.0 E09/E10). Events are stored with the generation job
// that read them, like memories, so a reroll, an edit, a deletion, a copy
// or a move treats them as it treats the turn's memory; an event a user
// records (job 0) belongs to the chat.
var bodyTables = []string{
	`CREATE TABLE IF NOT EXISTS body_events (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,             -- 0: recorded by the user
		pos INT NOT NULL,
		kind VARCHAR(32) NOT NULL,
		who VARCHAR(500) NOT NULL,          -- N<id>
		detail LONGTEXT NOT NULL,           -- the whole event with persistent ids
		day_text VARCHAR(500) NOT NULL DEFAULT '', -- a user record's story day
		result LONGTEXT NULL,               -- the pregnancy model's result, once computed
		KEY by_job (job_id),
		KEY by_session (session_key, id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS body_settings (
		session_key VARCHAR(191) NOT NULL,
		who VARCHAR(191) NOT NULL,          -- N<id>; '' holds the chat's seed
		spec LONGTEXT NOT NULL,             -- body.Spec, or the seed
		PRIMARY KEY (session_key, who)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
}

// bodyKinds are the event kinds stored; any other is noted and skipped.
var bodyKinds = map[string]bool{"period_start": true, "pregnancy_confirmed": true, "pregnancy_ended": true,
	"recovery_started": true, "recovery_ended": true, "conception_exposure": true}

// BodyTracking reads the two global switches (off until turned on).
func (d *DB) BodyTracking() (body.Toggles, error) {
	var t body.Toggles
	var raw string
	err := d.sql.QueryRow(`SELECT value FROM settings WHERE name='body_tracking'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal([]byte(raw), &t)
}

// SetBodyTracking keeps the two global switches for every chat.
func (d *DB) SetBodyTracking(t body.Toggles) error {
	b, _ := json.Marshal(t)
	_, err := d.sql.Exec(`INSERT INTO settings (name,value) VALUES ('body_tracking',?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, string(b))
	return err
}

// saveBodyEvents stores a job's body events with persistent ids; an event
// of an unknown kind or without a person is noted, never stored.
func saveBodyEvents(tx *sql.Tx, j Job, events []memory.BodyEvent, r *resolver) ([]string, error) {
	var notes []string
	for i, e := range events {
		e.Kind = strings.TrimSpace(e.Kind)
		e.Who = r.ref(strings.TrimSpace(e.Who))
		if !bodyKinds[e.Kind] || e.Who == "" {
			notes = append(notes, "body event skipped: kind "+e.Kind+" who "+e.Who)
			continue
		}
		e.Partners = r.list(e.Partners)
		if e.Paternity != nil {
			p := *e.Paternity
			p.Candidates = r.list(p.Candidates)
			e.Paternity = &p
		}
		b, _ := json.Marshal(e)
		if _, err := tx.Exec(`INSERT INTO body_events (session_key, job_id, pos, kind, who, detail) VALUES (?,?,?,?,?,?)`, j.Session, j.ID, i, e.Kind, e.Who, string(b)); err != nil {
			return notes, err
		}
	}
	return notes, nil
}

func init() { schema = append(schema, bodyTables...) }

// bodyPerson is a person's stored settings: the model's settings and the
// model's own cycle start once chosen.
type bodyPerson struct {
	Spec body.Spec `json:"spec"`
	Ref  body.Day  `json:"ref"`
}

// bodySeed is the chat's model seed, made once (AC1: a random seed per
// chat, never shown).
func (d *DB) bodySeed(chatKey string) (string, error) {
	var seed string
	err := d.sql.QueryRow(`SELECT spec FROM body_settings WHERE session_key=? AND who=''`, chatKey).Scan(&seed)
	if err == nil {
		return seed, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if _, err := d.sql.Exec(`INSERT IGNORE INTO body_settings (session_key, who, spec) VALUES (?,'',?)`, chatKey, hex.EncodeToString(b)); err != nil {
		return "", err
	}
	err = d.sql.QueryRow(`SELECT spec FROM body_settings WHERE session_key=? AND who=''`, chatKey).Scan(&seed)
	return seed, err
}

// BodyPerson reads a person's settings (the defaults when none are kept).
func (d *DB) BodyPerson(chatKey, who string) (body.Person, error) {
	p := body.Person{Who: who, Spec: body.DefaultSpec()}
	var raw string
	err := d.sql.QueryRow(`SELECT spec FROM body_settings WHERE session_key=? AND who=?`, chatKey, who).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	v := bodyPerson{Spec: p.Spec}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return p, err
	}
	p.Spec, p.Ref = v.Spec, v.Ref
	return p, nil
}

// SetBodyPerson keeps a person's settings; the model start chosen before
// is kept unless ref is given.
func (d *DB) SetBodyPerson(chatKey string, p body.Person) error {
	if !p.Ref.Known() {
		old, err := d.BodyPerson(chatKey, p.Who)
		if err != nil {
			return err
		}
		p.Ref = old.Ref
	}
	b, _ := json.Marshal(bodyPerson{Spec: p.Spec, Ref: p.Ref})
	_, err := d.sql.Exec(`INSERT INTO body_settings (session_key, who, spec) VALUES (?,?,?) ON DUPLICATE KEY UPDATE spec=VALUES(spec)`, chatKey, p.Who, string(b))
	return err
}

// BodyStates reads the body of every person with a body event in a chat's
// read scope (its turns' current jobs, an attached past included, and the
// events the user recorded) on the story's latest known day. Each event's
// day is the time it states when that is a date or Day N, else the
// recorded date of its turn. A model start chosen and pregnancy results
// computed here are kept, so later reads give the same story.
func (d *DB) BodyStates(chatKey string, scope []memory.ScopedTurn) ([]body.State, error) {
	on, err := d.BodyTracking()
	if err != nil || !on.Any() {
		return nil, err
	}
	scopeDates := map[int64]string{}
	var jobs []int64
	var first int64
	started := false
	for _, t := range scope {
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
			jobs = append(jobs, j)
		}
	}
	var dates map[int64]string
	now := body.Day{}
	if len(jobs) > 0 {
		if _, dates, err = d.jobDates(jobs, nil, scopeDates, first); err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if day := body.DayOf(dates[j]); day.Known() {
				now = day
			}
		}
	}
	type row struct {
		rec body.Record
		who string
		job int64
	}
	var rows []row
	// a row of another chat (an attached past) is read, never written: its
	// results are that chat's to keep
	foreign := map[int64]bool{}
	read := func(q string, args []any, dayOf func(job int64, dayText string) body.Day) error {
		return d.each(q, args, func(r *sql.Rows) error {
			var id, job int64
			var session, detail, dayText string
			var result sql.NullString
			if err := r.Scan(&id, &session, &job, &detail, &dayText, &result); err != nil {
				return err
			}
			if session != chatKey {
				foreign[id] = true
			}
			var e memory.BodyEvent
			if err := json.Unmarshal([]byte(detail), &e); err != nil {
				return err
			}
			rec := body.Record{ID: id, User: job == 0, Kind: e.Kind, When: e.When, Scope: e.Scope, Visibility: e.Visibility, Q: e.Q, Partners: e.Partners}
			if x := e.Exposure; x != nil {
				rec.Exposure = body.ExposureInput{Classification: x.Classification, Compatibility: x.Compatibility, Contraception: x.Contraception}
			}
			if p := e.Paternity; p != nil {
				rec.Paternity, rec.Candidates = p.Status, p.Candidates
			}
			rec.Day = body.DayOf(e.When)
			if !rec.Day.Known() {
				rec.Day = dayOf(job, dayText)
			}
			if result.Valid {
				var x body.Exposure
				if json.Unmarshal([]byte(result.String), &x) == nil {
					rec.Result = &x
				}
			}
			rows = append(rows, row{rec, e.Who, job})
			return nil
		})
	}
	if len(jobs) > 0 {
		in, args := inJobs(jobs)
		if err := read(`SELECT id, session_key, job_id, detail, day_text, result FROM body_events WHERE job_id IN `+in+` ORDER BY job_id, pos`, args,
			func(job int64, _ string) body.Day { return body.DayOf(dates[job]) }); err != nil {
			return nil, err
		}
	}
	// story order: the turns' order in the read scope (job ids are not:
	// a remade turn or an attached past gets later ids)
	place := map[int64]int{}
	for i, j := range jobs {
		place[j] = i
	}
	sort.SliceStable(rows, func(a, b int) bool { return place[rows[a].job] < place[rows[b].job] })
	turnRows := len(rows)
	if err := read(`SELECT id, session_key, job_id, detail, day_text, result FROM body_events WHERE session_key=? AND job_id=0 ORDER BY id`, []any{chatKey},
		func(_ int64, dayText string) body.Day { return body.DayOf(dayText) }); err != nil {
		return nil, err
	}
	// a fact recorded here goes where its day falls among the turns' facts
	// (after the last one on or before it; at the end when not comparable)
	recorded := append([]row(nil), rows[turnRows:]...)
	rows = rows[:turnRows]
	for _, u := range recorded {
		at := len(rows)
		for i, r := range rows {
			if r.rec.Day.Same(u.rec.Day) && r.rec.Day.N > u.rec.Day.N {
				at = i
				break
			}
		}
		rows = append(rows[:at], append([]row{u}, rows[at:]...)...)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	seed, err := d.bodySeed(chatKey)
	if err != nil {
		return nil, err
	}
	var order []string
	byWho := map[string][]body.Record{}
	for _, r := range rows {
		if _, ok := byWho[r.who]; !ok {
			order = append(order, r.who)
		}
		byWho[r.who] = append(byWho[r.who], r.rec)
	}
	var out []body.State
	for _, who := range order {
		p, err := d.BodyPerson(chatKey, who)
		if err != nil {
			return nil, err
		}
		st, ref, frozen := body.Assess(on, seed, p, byWho[who], now)
		if ref.Known() {
			p.Ref = ref
			if err := d.SetBodyPerson(chatKey, p); err != nil {
				return nil, err
			}
		}
		for id, x := range frozen {
			if foreign[id] {
				continue
			}
			b, _ := json.Marshal(x)
			if _, err := d.sql.Exec(`UPDATE body_events SET result=? WHERE id=? AND result IS NULL`, string(b), id); err != nil {
				return nil, err
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// ErrBodyFact is a recorded fact of an unknown kind or without a day.
var ErrBodyFact = errors.New("invalid_body_fact")

// userKinds are the facts a user records (AC1: no exposure by hand).
var userKinds = map[string]bool{"period_start": true, "pregnancy_confirmed": true, "pregnancy_ended": true, "recovery_started": true, "recovery_ended": true}

// RecordBodyFact keeps a fact the user records for a person of the chat,
// on a story day ("2026/01/02" or "Day 12").
func (d *DB) RecordBodyFact(chatKey, who, kind, day string) (int64, error) {
	if !userKinds[kind] || !body.DayOf(day).Known() {
		return 0, ErrBodyFact
	}
	b, _ := json.Marshal(memory.BodyEvent{Kind: kind, Who: who, Visibility: "private"})
	res, err := d.sql.Exec(`INSERT INTO body_events (session_key, job_id, pos, kind, who, detail, day_text) VALUES (?,0,0,?,?,?,?)`, chatKey, kind, who, string(b), body.DayOf(day).Text())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteBodyFact removes a fact the user recorded in the chat; a fact read
// from a turn is not one of them (it changes with its turn).
func (d *DB) DeleteBodyFact(chatKey string, id int64) (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM body_events WHERE id=? AND session_key=? AND job_id=0`, id, chatKey)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
