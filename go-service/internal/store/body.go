package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sort"
	"strconv"
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
	"recovery_started": true, "recovery_ended": true, "conception_exposure": true, "emergency_contraception": true, "child_named": true, "paternity_learned": true, "profile": true}

// sexWords are the words a profile's sex is read as (the model may answer
// in the story's language).
var sexWords = map[string]string{"female": "female", "f": "female", "woman": "female", "girl": "female", "여성": "female", "여자": "female", "여": "female", "암컷": "female", "女": "female", "女性": "female",
	"male": "male", "m": "male", "man": "male", "boy": "male", "남성": "male", "남자": "male", "남": "male", "수컷": "male", "男": "male", "男性": "male", "other": "other"}

// bodyProfile is a person's latest recorded profile.
type bodyProfile struct{ sex, species string }

// over is p updated by a later profile: each value the later one gives
// holds (AC1: the last non-empty value)
func (p bodyProfile) over(later bodyProfile) bodyProfile {
	if later.sex != "" {
		p.sex = later.sex
	}
	if later.species != "" {
		p.species = later.species
	}
	return p
}

func (p bodyProfile) text() string {
	return strings.Trim(p.sex+", "+p.species, ", ")
}

// profileNow reports a profile of the story now: not a flashback, a plan or
// a hypothesis, and not dated before or after the turn (like a body
// event's scope and time).
func profileNow(e memory.BodyEvent) bool {
	return strings.TrimSpace(e.Scope) == "" && strings.TrimSpace(e.When) == ""
}

// readProfile is a stored profile's sex (normalized) and species.
func readProfile(e memory.BodyEvent) bodyProfile {
	sex := strings.ToLower(strings.TrimSpace(e.Sex))
	if w, ok := sexWords[sex]; ok {
		sex = w
	}
	return bodyProfile{sex, strings.TrimSpace(e.Species)}
}

// profileRow is a profile recorded under a name.
type profileRow struct {
	who string
	p   bodyProfile
}

// personProfiles are each person's profile from the profiles recorded under
// any of their names in story order (person: the name a name is read as):
// each value the last gives holds, as in AC1. A profile records what the
// person really is (the critic is told a disguise's apparent kind is not
// her species), so a later value is a correction, under whichever name
// (telling an apparent kind from a correction by the name it was recorded
// under could not be done from the stored data).
func personProfiles(rows []profileRow, person func(string) string) map[string]bodyProfile {
	out := map[string]bodyProfile{}
	for _, r := range rows {
		k := person(r.who)
		out[k] = out[k].over(r.p)
	}
	return out
}

// bodyProfiles are the current profiles of the given jobs, in story order
// (the jobs' order).
func (d *DB) bodyProfiles(jobs []int64) ([]profileRow, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	type row struct {
		job int64
		who string
		p   bodyProfile
	}
	var rows []row
	in, args := inJobs(jobs)
	err := d.each(`SELECT job_id, who, detail FROM body_events WHERE kind='profile' AND job_id IN `+in+` ORDER BY job_id, pos`, args, func(r *sql.Rows) error {
		var x row
		var detail string
		if err := r.Scan(&x.job, &x.who, &detail); err != nil {
			return err
		}
		var e memory.BodyEvent
		if err := json.Unmarshal([]byte(detail), &e); err != nil {
			return err
		}
		if !profileNow(e) {
			return nil
		}
		x.p = readProfile(e)
		rows = append(rows, x)
		return nil
	})
	place := map[int64]int{}
	for i, j := range jobs {
		place[j] = i
	}
	sort.SliceStable(rows, func(a, b int) bool { return place[rows[a].job] < place[rows[b].job] })
	var out []profileRow
	for _, x := range rows {
		out = append(out, profileRow{x.who, x.p})
	}
	return out, err
}

// bodyPeople maps each name the user merged or the jobs' relations link to
// the name the person is read as (bodySets).
func (d *DB) bodyPeople(jobs []int64) (map[string]string, error) {
	into, err := d.userAliases()
	if err != nil {
		return nil, err
	}
	links, err := d.bodyIdentities(jobs)
	if err != nil {
		return nil, err
	}
	return bodySets(into, links), nil
}

// knownProfiles adds to each name of k the body profile of the person it
// is read as (every name of one person shows the same), from the jobs in
// story order, while body tracking is on (1.1: the model is shown what is
// recorded, so it does not record it again under another name).
func (d *DB) knownProfiles(k memory.Known, jobs []int64) (memory.Known, error) {
	on, err := d.BodyTracking()
	if err != nil || !on.Any() {
		return k, err
	}
	rows, err := d.bodyProfiles(jobs)
	if err != nil || len(rows) == 0 {
		return k, err
	}
	people, err := d.bodyPeople(jobs)
	if err != nil {
		return k, err
	}
	person := func(id string) string {
		if p, ok := people[id]; ok {
			return p
		}
		return id
	}
	profiles := personProfiles(rows, person)
	for i := range k.Names {
		k.Names[i].Body = profiles[person(k.Names[i].ID)].text()
	}
	return k, nil
}

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

// LatestReplyMade reports whether the latest reply of a read scope (its
// last turn with a reply; the request's own input has none yet) has its
// memory made. With no reply in the scope there is nothing to wait for.
func (d *DB) LatestReplyMade(scope []memory.ScopedTurn) (bool, error) {
	made, _, err := d.latestReply(scope)
	return made, err
}

// latestReply is the read scope's latest reply (its last turn with one; the
// request's own input has none yet), whether its memory is made, and its
// source. With no reply in the scope it counts as made.
func (d *DB) latestReply(scope []memory.ScopedTurn) (bool, memory.Source, error) {
	for i := len(scope) - 1; i >= 0; i-- {
		t := scope[i]
		if t.Off || !t.Source.Readable() {
			continue
		}
		// the made job's ids only (CurrentJobs reads made memory)
		js, src, err := d.turnJobs(t)
		return len(js) > 0, src, err
	}
	return true, memory.Source{}, nil
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
		if e.Kind == "profile" {
			p := readProfile(e)
			e.Sex, e.Species = p.sex, p.species
		}
		e.Partners = r.list(e.Partners)
		if c := strings.TrimSpace(e.Child); c != "" {
			e.Child = r.ref(c)
		}
		if e.Paternity != nil {
			p := *e.Paternity
			p.Candidates, p.Unaware, p.Learns = r.list(p.Candidates), r.list(p.Unaware), r.list(p.Learns)
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
	Spec      body.Spec       `json:"spec"`
	Ref       body.Day        `json:"ref"`
	Intensity *body.Intensity `json:"intensity,omitempty"`
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
	p.Spec, p.Ref, p.Intensity = v.Spec, v.Ref, v.Intensity
	return p, nil
}

// userAliases maps every merged name to the name finally kept, read from
// the user's alias relations like the memory readers (userNameResolver:
// every chat's merges, chains of any length; undoing a merge removes its
// relation).
func (d *DB) userAliases() (map[string]string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := userNameResolver(tx)
	if err != nil {
		return nil, err
	}
	return r.ids, nil
}

// bodyPersonAs is the settings a person is read with: their own, else
// those kept under a name merged into them (the first by name), with the
// name that owns them.
func (d *DB) bodyPersonAs(chatKey, who string, canon map[string]string) (body.Person, string, error) {
	var n int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM body_settings WHERE session_key=? AND who=?`, chatKey, who).Scan(&n); err != nil {
		return body.Person{}, "", err
	}
	owner := who
	if n == 0 {
		err := d.each(`SELECT who FROM body_settings WHERE session_key=? AND who<>'' ORDER BY who`, []any{chatKey}, func(r *sql.Rows) error {
			var a string
			if err := r.Scan(&a); err != nil {
				return err
			}
			if owner == who && a != who && canon[a] == who {
				owner = a
			}
			return nil
		})
		if err != nil {
			return body.Person{}, "", err
		}
	}
	p, err := d.BodyPerson(chatKey, owner)
	p.Who = who
	return p, owner, err
}

// BodyOwner is the name a person's settings are kept under in a chat's
// read scope: their own, or a name merged into or linked to them (as
// BodyStates reads them), without reading or keeping a state.
func (d *DB) BodyOwner(chatKey string, scope []memory.ScopedTurn, who string) (string, error) {
	var jobs []int64
	for _, t := range scope {
		if t.Off {
			continue
		}
		js, _, err := d.turnJobs(t)
		if err != nil {
			return "", err
		}
		jobs = append(jobs, js...)
	}
	merged, err := d.bodyPeople(jobs)
	if err != nil {
		return "", err
	}
	_, owner, err := d.bodyPersonAs(chatKey, who, merged)
	return owner, err
}

// BodySettings is the settings a person of the chat is read with, merged
// names followed (the body screen shows what the state is computed with).
func (d *DB) BodySettings(chatKey, who string) (body.Person, error) {
	canon, err := d.userAliases()
	if err != nil {
		return body.Person{}, err
	}
	p, _, err := d.bodyPersonAs(chatKey, who, canon)
	return p, err
}

// SetBodyPerson keeps a person's settings; the model start chosen before
// is kept unless ref is given.
func (d *DB) SetBodyPerson(chatKey string, p body.Person) error {
	if !p.Ref.Known() || p.Intensity == nil {
		// the model start chosen and the intensity drawn are kept, ones kept
		// under a merged name too
		old, err := d.BodySettings(chatKey, p.Who)
		if err != nil {
			return err
		}
		if !p.Ref.Known() {
			p.Ref = old.Ref
		}
		if p.Intensity == nil {
			p.Intensity = old.Intensity
		}
	}
	b, _ := json.Marshal(bodyPerson{Spec: p.Spec, Ref: p.Ref, Intensity: p.Intensity})
	_, err := d.sql.Exec(`INSERT INTO body_settings (session_key, who, spec) VALUES (?,?,?) ON DUPLICATE KEY UPDATE spec=VALUES(spec)`, chatKey, p.Who, string(b))
	return err
}

// BodyStates reads the body of every person with a body event in a chat's
// read scope (its turns' current jobs, an attached past included, and the
// events the user recorded) on the story's latest known day. Each event's
// day is on the stored days (body.EventDay: its turn's day, a calendar date
// or days before or after it); a result frozen on a day read the old way
// keeps that day. A model start chosen and pregnancy results
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
	made, latest, err := d.latestReply(scope)
	if err != nil {
		return nil, err
	}
	pending := !made
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
	if pending {
		// the latest reply is not made yet: a date it gives (a variable, or
		// one its text writes) is the scene's day now on the axis the chat's
		// days are read on: a calendar date in a chat counted in Days is
		// read with the turn once its memory is made
		if day := body.DayOf(memory.DateText(sourceWithBodyDate(latest).Dates)); day.Known() && (!now.Known() || day.Same(now)) {
			now = day
		}
	}
	type row struct {
		rec     body.Record
		who     string
		job     int64
		profile *bodyProfile // a profile: no body event, the person's sex and species
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
				rec.Exposure = body.ExposureInput{Classification: x.Classification, Compatibility: x.Compatibility, Contraception: x.Contraception, Method: x.Method}
			}
			if p := e.Paternity; p != nil {
				rec.Paternity, rec.Candidates, rec.Unaware, rec.Learns = p.Status, p.Candidates, p.Unaware, p.Learns
			}
			rec.Outcome, rec.Child = e.Outcome, e.Child
			// the critic's time is read against the turn's stored day; a
			// fact recorded here is on its own day
			rec.Day = body.EventDay(e.When, dayOf(job, dayText))
			if result.Valid {
				var x body.Exposure
				if json.Unmarshal([]byte(result.String), &x) == nil {
					rec.Result = &x
					// a result frozen on the day the event was read on before
					// (a story's "day N" as given) keeps that day: a kept
					// result is never moved or drawn again
					if was := body.DayOf(e.When); was.Known() && was.N == x.ExposureDay {
						rec.Day = was
					}
				}
			}
			x := row{rec: rec, who: e.Who, job: job}
			if e.Kind == "profile" {
				if !profileNow(e) {
					return nil // a planned, hypothetical or later profile is not the body now
				}
				p := readProfile(e)
				x.profile = &p
			}
			rows = append(rows, x)
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
	// merged names (the user's alias relations, as the memory readers read
	// them, an attached past's included): a person, a partner and a parent
	// candidate are read as the name kept, so one person's facts are one
	// state (1.1); undoing a merge reads them apart again
	// one person under several names is one body (1.1; AC1 kept a card per
	// id): the identity relations the turns in the read scope record
	// (alias, true name, disguise; a reroll or an edit takes its turn's
	// away), with the user's merges, join names into one person
	merged, err := d.bodyPeople(jobs)
	if err != nil {
		return nil, err
	}
	canon := func(id string) string {
		if k, ok := merged[id]; ok {
			return k
		}
		return id
	}
	canonList := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if c := canon(id); !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
		return out
	}
	var profiles []profileRow
	for _, r := range rows {
		if r.profile != nil {
			profiles = append(profiles, profileRow{r.who, *r.profile})
		}
	}
	profile := personProfiles(profiles, canon)
	for i := range rows {
		rows[i].who = canon(rows[i].who)
		rows[i].rec.Partners = canonList(rows[i].rec.Partners)
		rows[i].rec.Candidates = canonList(rows[i].rec.Candidates)
		rows[i].rec.Unaware = canonList(rows[i].rec.Unaware)
		rows[i].rec.Learns = canonList(rows[i].rec.Learns)
		if rows[i].rec.Child != "" {
			rows[i].rec.Child = canon(rows[i].rec.Child)
		}
	}
	// within a turn, a person's name or learning written just before her
	// birth or pregnancy confirmation is read after it: the birth or the
	// pregnancy it is about is in place whatever order the critic wrote them
	// in. It moves past that one record of hers only, never past another of
	// her events (a loss after it ends the pregnancy it was about); other
	// people's records keep their places. A person is her merged names: read
	// after the names are joined.
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].job == rows[i].job {
			j++
		}
		byWho := map[string][]int{} // each person's places in the turn
		for k := i; k < j; k++ {
			byWho[rows[k].who] = append(byWho[rows[k].who], k)
		}
		for _, at := range byWho {
			var order, wait []row
			for _, k := range at {
				switch r := rows[k].rec; {
				case rows[k].profile != nil:
					order = append(order, rows[k]) // no body event: passed over
				case r.Kind == "child_named" || r.Kind == "paternity_learned":
					wait = append(wait, rows[k])
				case r.Kind == "pregnancy_confirmed" || (r.Kind == "pregnancy_ended" && r.Outcome == "birth"):
					order = append(append(order, rows[k]), wait...)
					wait = nil
				default:
					order = append(append(order, wait...), rows[k])
					wait = nil
				}
			}
			order = append(order, wait...)
			for n, k := range at {
				rows[k] = order[n]
			}
		}
		i = j
	}
	// a person is tracked with a body event, or as a woman by her latest
	// profile (1.1, AC1: every woman, any species, with no event); repeated
	// profiles are one person's, each value the last in story order gives
	// holds
	var order []string
	byWho := map[string][]body.Record{}
	tracked := map[string]bool{}
	for _, r := range rows {
		if _, ok := byWho[r.who]; !ok {
			order = append(order, r.who)
			byWho[r.who] = nil
		}
		if r.profile != nil {
			continue
		}
		tracked[r.who] = true
		byWho[r.who] = append(byWho[r.who], r.rec)
	}
	var out []body.State
	for _, who := range order {
		if !tracked[who] && profile[who].sex != "female" {
			continue
		}
		// settings kept under a merged-away name serve the name kept when it
		// has none of its own; a model start chosen is kept with them
		p, owner, err := d.bodyPersonAs(chatKey, who, merged)
		if err != nil {
			return nil, err
		}
		// a person's intensity is drawn once from the chat's seed
		// (random) and kept with the settings that own it
		drawn := p.Intensity == nil || p.Intensity.Recovery == 0
		if drawn {
			in := body.DrawIntensity(seed, owner)
			if p.Intensity != nil {
				// kept before the recovery speed (1.1.0-test.6): only it is drawn
				kept := *p.Intensity
				kept.Recovery = in.Recovery
				in = kept
			}
			p.Intensity = &in
		}
		st, ref, frozen := body.Assess(on, seed, p, byWho[who], now)
		st.Owner, st.Species = owner, profile[who].species
		for id, k := range merged {
			if k == who && id != who {
				st.Joined = append(st.Joined, id)
			}
		}
		sort.Strings(st.Joined)
		if ref.Known() || drawn {
			kept := p
			kept.Who = owner
			if ref.Known() {
				kept.Ref = ref
			}
			if err := d.SetBodyPerson(chatKey, kept); err != nil {
				return nil, err
			}
		}
		for id, x := range frozen {
			if foreign[id] {
				continue
			}
			b, _ := json.Marshal(x)
			// a result kept once is kept; one kept without a draw by a rule
			// that no longer holds (body.Redrawn) is replaced by what it is
			// read as now
			if _, err := d.sql.Exec(`UPDATE body_events SET result=? WHERE id=? AND (result IS NULL OR JSON_VALUE(result,'$.reason') IN ('modeled_pregnancy_ongoing','exposure_context_not_modeled','contraception_context_not_modeled'))`, string(b), id); err != nil {
				return nil, err
			}
		}
		st.Pending = pending
		out = append(out, st)
	}
	return out, nil
}

// bodyLink is an identity relation between two names: name is the same
// person as same (how: alias | true_name | disguise).
type bodyLink struct{ name, same, how string }

// bodyIdentities are the identity relations (alias, true name, disguise)
// the given jobs record between stored names. A relation kept from someone
// (hide_from or unaware, as delivery reads a secret) joins nothing: the
// body lines would show the link.
func (d *DB) bodyIdentities(jobs []int64) ([]bodyLink, error) {
	var out []bodyLink
	if len(jobs) == 0 {
		return out, nil
	}
	in, args := inJobs(jobs)
	all := append(append([]any(nil), args...), args...)
	// the names as the relation recorded them: a user merge rewrites both
	// to the name kept (the merge itself joins them, bodySets), which would
	// lose which name was the disguise
	origin := func(path string) string {
		return `COALESCE((SELECT c.origin_ref FROM name_ref_changes c WHERE c.row_table='name_relations' AND c.row_id=r.id AND c.path='` + path + `' ORDER BY c.id DESC LIMIT 1),r.` + path + `)`
	}
	err := d.each(`SELECT `+origin("name_ref")+`, `+origin("same_as")+`, LOWER(TRIM(r.how)) FROM name_relations r WHERE LOWER(TRIM(r.how)) IN ('alias','true_name','disguise') AND r.job_id IN `+in+
		` AND NOT EXISTS (SELECT 1 FROM awareness a WHERE a.target=CONCAT('R',r.id) AND a.kind IN ('hide_from','unaware') AND a.job_id IN `+in+`) ORDER BY r.id`, all, func(r *sql.Rows) error {
		var l bodyLink
		if err := r.Scan(&l.name, &l.same, &l.how); err != nil {
			return err
		}
		if strings.HasPrefix(l.same, "N") && l.name != l.same {
			out = append(out, l) // else not resolved to a stored name
		}
		return nil
	})
	return out, err
}

// bodySets joins names into people: every name a merge or a relation
// connects, in any direction or chain, is one person, read as one name — a
// name the user kept, else a true name given, else a name an alias or a
// disguise is the same as and that is no alias or disguise itself (the
// end of a chain), else the earliest name. Each joined name maps to it.
func bodySets(into map[string]string, links []bodyLink) map[string]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(a string) string {
		if p, ok := parent[a]; ok && p != a {
			r := find(p)
			parent[a] = r
			return r
		}
		parent[a] = a
		return a
	}
	join := func(a, b string) { parent[find(a)] = find(b) }
	rank := map[string]int{}
	up := func(id string, r int) { rank[id] = max(rank[id], r) }
	for a, b := range into {
		if b == "" {
			continue
		}
		join(a, b)
		if _, merged := into[b]; !merged {
			up(b, 3)
		}
	}
	other := map[string]bool{} // names that are an alias or a disguise
	for _, l := range links {
		if l.how != "true_name" {
			other[l.name] = true
		}
	}
	for _, l := range links {
		join(l.name, l.same)
		if l.how == "true_name" {
			up(l.name, 2)
		} else if !other[l.same] {
			up(l.same, 1)
		}
	}
	num := func(id string) int64 {
		n, err := strconv.ParseInt(strings.TrimPrefix(id, "N"), 10, 64)
		if err != nil {
			return math.MaxInt64
		}
		return n
	}
	best := map[string]string{}
	for id := range parent {
		root := find(id)
		b, ok := best[root]
		if !ok || rank[id] > rank[b] || rank[id] == rank[b] && (num(id) < num(b) || num(id) == num(b) && id < b) {
			best[root] = id
		}
	}
	out := map[string]string{}
	for id := range parent {
		out[id] = best[find(id)]
	}
	return out
}

// ErrBodyFact is a recorded fact of an unknown kind or without a day.
var ErrBodyFact = errors.New("invalid_body_fact")

// userKinds are the facts a user records (AC1: no exposure by hand).
var userKinds = map[string]bool{"period_start": true, "pregnancy_confirmed": true, "pregnancy_ended": true, "recovery_started": true, "recovery_ended": true, "emergency_contraception": true}

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
