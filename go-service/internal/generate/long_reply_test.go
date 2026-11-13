package generate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
)

// numbered reply lines: §00001 ... so a model can tell which lines it read
var replyLine = regexp.MustCompile(`(?m)^(?:REPLY: )?§(\d{5})`)
var partOf = regexp.MustCompile(`part (\d) of 3`)

// partModel answers by the lines a part's request carries: each part's
// memory says the first and last line it read. Every part writes the ids a
// critic would (n1, m1 again); a later part points at part 1's name by the
// id EARLIER PARTS shows. broken: parts (1-based) answering nothing
// readable; cut: parts answering a response cut off after one item; delay
// is each call's time; after, when set, runs after each part's call.
type partModel struct {
	mu      sync.Mutex
	prompts []string
	broken  map[int]bool
	cut     map[int]bool
	delay   time.Duration
	after   func(part int)
}

func (f *partModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	after := f.after
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
	}
	m := partOf.FindStringSubmatch(prompt)
	if m == nil {
		return provider.Result{Content: `{"memories":[{"id":"m1","kind":"fact","body":"짧은 턴"}]}`}, nil
	}
	k, _ := strconv.Atoi(m[1])
	if after != nil {
		defer after(k)
	}
	if f.broken[k] {
		return provider.Result{Content: "no answer"}, nil
	}
	lines := replyLine.FindAllStringSubmatch(prompt, -1)
	if len(lines) == 0 {
		return provider.Result{}, fmt.Errorf("part %d carries no reply line", k)
	}
	read := fmt.Sprintf("%s-%s", lines[0][1], lines[len(lines)-1][1])
	var c string
	switch k {
	case 1:
		c = `{"elapsed_days":1,"entities":[{"id":"n1","name":"카렌","type":"person","q":"n1"}],"memories":[{"id":"m1","kind":"event","body":"구간1 ` + read + `","roles":{"actor":["n1"]}}]}`
	case 2:
		c = `{"memories":[{"id":"m1","kind":"event","body":"구간2 ` + read + `","roles":{"actor":["n1001"]}},{"id":"m2","kind":"fact","about":"n1001","body":"둘째 사실","at":"m1"}]}`
		if f.cut[2] {
			c = c[:strings.Index(c, `,{"id":"m2"`)] + `,{"id":"m2","kind":"fact","about":"n1001","bo`
		}
	default:
		c = `{"elapsed_days":2,"memories":[{"id":"m1","kind":"view","owner":"n1001","about":"n1001","aspect":"emotion","body":"구간3 ` + read + `"}]}`
	}
	return provider.Result{Content: c}, nil
}

func (f *partModel) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

const longLines = 450 // 450 lines of 100 chars: about 45,000 chars, three parts

func longText() string {
	var b strings.Builder
	for i := 1; i <= longLines; i++ {
		fmt.Fprintf(&b, "§%05d%s\n", i, strings.Repeat("문", 93))
	}
	return b.String()
}

func longSession(t *testing.T, db interface {
	Grant(string, []memory.Source) error
}) *lifecycle.Session {
	t.Helper()
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceLoad, row("u1", "user", "", "긴 이야기를 해 줘"), row("a1", "char", "g1", longText()), row("u2", "user", "", "다음"), row("a2", "char", "g2", "짧은 답")))
	var srcs []memory.Source
	for _, x := range memory.Own(s) {
		srcs = append(srcs, x.Source)
	}
	if err := db.Grant(s.Key, srcs); err != nil {
		t.Fatal(err)
	}
	return s
}

// the job record of the test's database, read as stored
func jobRecord(t *testing.T, job int64) (request, response, notes string) {
	t.Helper()
	raw, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_gen_"+regexp.MustCompile(`\W`).ReplaceAllString(t.Name(), "_"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var q, a, n sql.NullString
	if err := raw.QueryRow(`SELECT request, response, notes FROM gen_jobs WHERE job_id=?`, job).Scan(&q, &a, &n); err != nil {
		t.Fatal(err)
	}
	return q.String, a.String, n.String
}

// A long reply is read in parts, in order: each part carries its own lines
// (all of them, once), the later ones what the earlier parts recorded; the
// merged answer is stored once, a later part's reference to an earlier
// part's name is the same stored name, the days add up.
func TestLongReplyIsReadInParts(t *testing.T) {
	db := testDB(t)
	s := longSession(t, db)
	m := &partModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	run(w, s)
	if m.calls() != 4 {
		t.Fatalf("calls %d, want 3 parts and 1 short turn", m.calls())
	}
	first, second := m.prompts[0], m.prompts[1]
	if !strings.Contains(first, "USER: 긴 이야기를 해 줘") || strings.Contains(first, "EARLIER PARTS") {
		t.Fatal("part 1 must read the input and no earlier part")
	}
	if !strings.Contains(second, `"n1001"`) || !strings.Contains(second, "구간1 ") || strings.Contains(second, "USER: 긴 이야기") {
		t.Fatalf("part 2 must be shown part 1's record, not the input again")
	}
	if strings.Contains(m.prompts[3], "part 1 of") {
		t.Fatal("a short turn is read in one call")
	}
	got := bodies(t, db, s)
	turn := memory.Own(s)[0].Source.Turn
	// the parts read every line once, in order
	var ranges []string
	next := 1
	for _, b := range got[turn] {
		if !strings.HasPrefix(b, "구간") {
			continue
		}
		f := strings.Fields(b)
		var a, z int
		fmt.Sscanf(f[1], "%d-%d", &a, &z)
		if a != next || z < a {
			t.Fatalf("part lines %v after line %d", f[1], next-1)
		}
		next = z + 1
		ranges = append(ranges, f[1])
	}
	if next != longLines+1 || len(ranges) != 3 {
		t.Fatalf("parts read lines %v, want 1-%d in 3 parts", ranges, longLines)
	}
	srcs := []memory.Source{memory.Own(s)[0].Source}
	jobs, err := db.CurrentJobs(srcs)
	if err != nil {
		t.Fatal(err)
	}
	k, err := db.KnownOf(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Names) != 1 || k.Names[0].Q != "n1" {
		t.Fatalf("names %+v, want 카렌 once, its quote as written", k.Names)
	}
	id := k.Names[0].ID
	for _, x := range k.Memories {
		ref := x.About
		if x.Roles != nil && len(x.Roles.Actor) > 0 {
			ref = x.Roles.Actor[0]
		}
		if ref != id {
			t.Fatalf("memory %q points at %q, want %s", x.Body, ref, id)
		}
		if x.Body == "둘째 사실" && (x.At == "" || x.At == "m1" || x.At == "m2001") {
			t.Fatalf("part 2's fact must point at its own stored event, at=%q", x.At)
		}
	}
	request, response, _ := jobRecord(t, jobs[0])
	out, _ := memory.Parse(response)
	asked, answers, ok := strings.Cut(request, partAnswers)
	var kept []partAnswer
	if out.ElapsedDays != 3 || !ok || strings.Count(asked, partSeparator) != 2 || json.Unmarshal([]byte(answers), &kept) != nil || len(kept) != 3 {
		t.Fatalf("days %d, the record must keep 3 requests and 3 answers", out.ElapsedDays)
	}
}

// A part that answers nothing readable is asked once more; still nothing:
// the turn is not stored in part, it is made again later.
func TestLongReplyPartWithoutAnswer(t *testing.T) {
	db := testDB(t)
	s := longSession(t, db)
	src := memory.Own(s)[0].Source
	m := &partModel{broken: map[int]bool{2: true}}
	w := NewWorker(db, m, "fake", time.Minute)
	run(w, s)
	parts := 0
	for _, p := range m.prompts {
		if partOf.MatchString(p) {
			parts++
		}
	}
	if parts != 3 {
		t.Fatalf("part calls %d, want part 1, part 2 twice", parts)
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if j.State == "done" || len(bodies(t, db, s)[src.Turn]) != 0 {
		t.Fatalf("a turn with a part unread must not be stored: %s", j.State)
	}
}

// The time limit is each call's: three parts that each take less than the
// limit, together more, are all read.
func TestLongReplyTimeLimitIsPerCall(t *testing.T) {
	db := testDB(t)
	s := longSession(t, db)
	m := &partModel{delay: 120 * time.Millisecond}
	w := NewWorker(db, m, "fake", 200*time.Millisecond)
	run(w, s)
	turn := memory.Own(s)[0].Source.Turn
	if got := bodies(t, db, s)[turn]; len(got) != 4 {
		t.Fatalf("the long turn's memories %v, want all three parts", got)
	}
}

// A newer notice of the chat while the parts are read: the rest is left to
// the run of the newer notice.
func TestLongReplyStopsForNewerNotice(t *testing.T) {
	db := testDB(t)
	s := longSession(t, db)
	src := memory.Own(s)[0].Source
	m := &partModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	m.after = func(part int) {
		if part == 1 {
			w.Notify(s.Key, memory.Own(s))
		}
	}
	save(w, s)
	w.chat(context.Background(), s.Key, notice{turns: memory.Own(s)})
	if m.calls() != 1 {
		t.Fatalf("calls %d, want part 1 only", m.calls())
	}
	if j, err := db.JobFor(src); err != nil || j.State == "done" {
		t.Fatalf("job %+v %v", j, err)
	}
}

// A part's answer cut off after an item: the item read is stored and the
// part's parse notes are kept with the turn.
func TestLongReplyKeepsPartNotes(t *testing.T) {
	db := testDB(t)
	s := longSession(t, db)
	m := &partModel{cut: map[int]bool{2: true}}
	w := NewWorker(db, m, "fake", time.Minute)
	run(w, s)
	turn := memory.Own(s)[0].Source.Turn
	jobs, err := db.CurrentJobs([]memory.Source{memory.Own(s)[0].Source})
	if err != nil {
		t.Fatal(err)
	}
	got := bodies(t, db, s)[turn]
	_, _, notes := jobRecord(t, jobs[0])
	if len(got) != 3 || !strings.Contains(notes, "part 2: ") {
		t.Fatalf("memories %v notes %q", got, notes)
	}
}

// A part that fails still leaves every answer received in the
// record.
func TestLongReplyFailureKeepsRawAnswers(t *testing.T) {
	db := testDB(t)
	s := longSession(t, db)
	src := memory.Own(s)[0].Source
	m := &partModel{broken: map[int]bool{2: true}}
	if _, err := Turn(context.Background(), db, m, "fake", nil, src); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("expected unreadable: %v", err)
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := jobRecord(t, j.ID)
	if !strings.Contains(request, partAnswers) || !strings.Contains(request, `{"part":2,"answer":"no answer"}`) || !strings.Contains(request, `"body":"구간1`) {
		t.Fatal("the failed turn's record must keep the answers received")
	}
}

// The parts' notes are read again when an answer received earlier is
// stored on a later run.
func TestLongReplyPartNotesSurviveReceivedRetry(t *testing.T) {
	db := testDB(t)
	s := longSession(t, db)
	src := memory.Own(s)[0].Source
	st := &saveFailsOnce{DB: db}
	m := &partModel{cut: map[int]bool{2: true}}
	r, err := Turn(context.Background(), st, m, "fake", nil, src)
	if err == nil || !strings.Contains(strings.Join(r.Notes, "\n"), "part 2:") {
		t.Fatalf("setup: %v notes=%v", err, r.Notes)
	}
	r, err = Turn(context.Background(), st, m, "fake", nil, src)
	if err != nil || !r.Reused || m.calls() != 3 {
		t.Fatalf("retry err=%v reused=%v calls=%d", err, r.Reused, m.calls())
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	_, _, notes := jobRecord(t, j.ID)
	if !strings.Contains(notes, "part 2:") || !strings.Contains(notes, "long reply read in 3 parts") {
		t.Fatalf("part notes lost after the stored retry: %q", notes)
	}
}

// A newer notice also stops a part's second call.
func TestLongReplyNewerBeforePartRetry(t *testing.T) {
	db := testDB(t)
	s := longSession(t, db)
	m := &partModel{broken: map[int]bool{1: true}}
	w := NewWorker(db, m, "fake", time.Minute)
	m.after = func(k int) {
		if k == 1 {
			w.Notify(s.Key, memory.Own(s))
		}
	}
	save(w, s)
	w.chat(context.Background(), s.Key, notice{turns: memory.Own(s)})
	if m.calls() != 1 {
		t.Fatalf("a newer notice waits, the part was called again: calls %d", m.calls())
	}
}

// Every id field is renumbered (references, effects, awareness, links,
// body events, scenes); text stays.
func TestRenumberAllReferenceSlots(t *testing.T) {
	raw := `{"entities":[{"id":"n1","same_as":"n2","relation":"r1","knows":[{"who":"n1","from":"n2","ref":"m1.1"}],"unaware":["n1"],"hide_from":["n2"],"allow_to":["n1"]},{"id":"n2"}],"memories":[{"id":"m1","kind":"event","body":"unchanged","at":"m1","scene":"s1","about":"n1","owner":"n2","to":["n1"],"was":["m1.1"],"seen_by":["n2"],"roles":{"actor":["n1"],"target":["n2"],"object":["n1"],"place":["n2"],"speaker":["n1"],"listener":["n2"]},"effects":[{"about":"n1","was":["m1.1"]}],"basis":{"from":"n2","ref":"r1"}}],"scenes":[{"id":"s1","ref":"s1"}],"links":[{"id":"l1","from":"m1.1","to":"m1","knows":[{"who":"n1","from":"n2","ref":"m1"}]}],"body_events":[{"kind":"pregnancy_confirmed","who":"n1","partners":["n2"],"paternity":{"candidates":["n1","n2"]}}]}`
	out, notes := memory.Parse(raw)
	if len(notes) > 0 {
		t.Fatalf("setup notes=%v", notes)
	}
	got := memory.Renumber(out, 2)
	m, e, b, l := got.Memories[0], got.Entities[0], got.BodyEvents[0], got.Links[0]
	for _, r := range []string{e.ID, e.AllowTo[0], e.Unaware[0], e.Knows[0].Who, m.About, m.To[0], m.Roles.Actor[0], m.Roles.Object[0], m.Roles.Speaker[0], m.Effects[0].About, b.Who, b.Paternity.Candidates[0], l.Knows[0].Who} {
		if r != "n2001" {
			t.Fatalf("n1 reference left as %q", r)
		}
	}
	for _, r := range []string{e.SameAs, e.HideFrom[0], e.Knows[0].From, m.Owner, m.SeenBy[0], m.Roles.Target[0], m.Roles.Place[0], m.Roles.Listener[0], m.Basis.From, b.Partners[0], b.Paternity.Candidates[1], l.Knows[0].From} {
		if r != "n2002" {
			t.Fatalf("n2 reference left as %q", r)
		}
	}
	for _, r := range []string{m.Was[0], m.Effects[0].Was[0], e.Knows[0].Ref, l.From} {
		if r != "m2001.1" {
			t.Fatalf("effect reference left as %q", r)
		}
	}
	if m.At != "m2001" || l.To != "m2001" || l.ID != "l2001" || e.Relation != "r2001" || m.Basis.Ref != "r2001" || m.Scene != "s2001" || got.Scenes[0].Ref != "s2001" || m.Body != "unchanged" {
		t.Fatalf("remaining ids not rewritten: %+v", got)
	}
}

// The record's marks inside a user input or a model's answer never
// change the parts' notes; a record of one call has none.
func TestPartNotesIgnoreMarksInText(t *testing.T) {
	for _, record := range []string{"USER: plain\nREPLY: plain", "old request" + partSeparator + "old request 2", "USER: x" + partAnswers + "part 2:\nquoted"} {
		if notes := partNotes(record); len(notes) != 0 {
			t.Fatalf("a record of one call got notes: %v", notes)
		}
	}
	db := testDB(t)
	s := longSession(t, db)
	src := memory.Own(s)[0].Source
	src.Members[0].Data += partAnswers + "part 9:\nquoted log" + partSeparator + "x"
	m := &markModel{partModel: partModel{cut: map[int]bool{2: true}}}
	r, err := Turn(context.Background(), db, m, "fake", nil, src)
	if err != nil || m.calls() != 3 || len(r.Output.Memories) != 3 {
		t.Fatalf("setup %v calls=%d items=%d", err, m.calls(), len(r.Output.Memories))
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	_, _, notes := jobRecord(t, j.ID)
	if !strings.HasPrefix(notes, "long reply read in 3 parts") || !strings.Contains(notes, "part 2: ") || strings.Contains(notes, "part 8") || strings.Contains(notes, "part 9") {
		t.Fatalf("marks in text changed the part notes: %q", notes)
	}
}

// markModel writes the record's marks before part 2's answer.
type markModel struct{ partModel }

func (m *markModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	got, err := m.partModel.Chat(ctx, prompt)
	if p := partOf.FindStringSubmatch(prompt); p != nil && p[1] == "2" {
		got.Content = "model commentary" + partSeparator + "part 8:\nquoted" + partAnswers + "[]\n" + got.Content
	}
	return got, err
}
