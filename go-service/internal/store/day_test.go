package store

import (
	"database/sql"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

func daySave(t *testing.T, db *DB, src memory.Source, delta, body string) Job {
	t.Helper()
	raw := `{"elapsed_days":` + delta + `}`
	if body != "" {
		raw = fmt.Sprintf(`{"elapsed_days":%s,"memories":[{"kind":"event","body":%q}]}`, delta, body)
	}
	out, notes := memory.Parse(raw)
	if memory.Unreadable(notes) {
		t.Fatalf("unreadable fixture %s", raw)
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveGeneration(j, Generation{Output: out, Response: raw, Notes: notes}); err != nil {
		t.Fatal(err)
	}
	return j
}

func dayKnown(t *testing.T, db *DB, scope []memory.ScopedTurn, want map[string]string) {
	t.Helper()
	k, _, err := db.knownScope(scope, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range k.Memories {
		got[m.Body] = m.Date
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dates %v, want %v", got, want)
	}
}

func TestDaySparseScope(t *testing.T) {
	db := testDB(t)
	scope := []memory.ScopedTurn{}
	for _, id := range []int{10, 40, 90} {
		scope = append(scope, memory.ScopedTurn{Source: memory.Source{Session: "chat:sparse", Turn: id}})
	}
	// No generated first turn: the second turn must still contribute its delta.
	daySave(t, db, scope[1].Source, "2", "second")
	dayKnown(t, db, scope, map[string]string{"second": "Day 2"})
	// The origin is the first source, not turn_id 1 or the first memory row.
	daySave(t, db, scope[0].Source, "9", "")
	daySave(t, db, scope[2].Source, "3", "third")
	dayKnown(t, db, scope, map[string]string{"second": "Day 2", "third": "Day 5"})
	// A metadata-only middle turn contributes even though it stores no item.
	scope[1].Source.Members = []memory.SourceMember{{Data: "rerolled middle"}}
	daySave(t, db, scope[1].Source, "4", "")
	dayKnown(t, db, scope, map[string]string{"third": "Day 7"})
	dayKnown(t, db, []memory.ScopedTurn{scope[0], scope[2]}, map[string]string{"third": "Day 3"})
	dayKnown(t, db, scope[2:], map[string]string{"third": "Day 0"})
}

func TestDayDatePriorityAndLegacy(t *testing.T) {
	db := testDB(t)
	sources := []memory.Source{
		{Session: "chat:dates", Turn: 1, Dates: map[string]string{"clock": "1423/05/15"}, Responses: []memory.SourceResponse{{Seq: 1, Data: "1423/05/14"}}},
		{Session: "chat:dates", Turn: 2, Responses: []memory.SourceResponse{{Seq: 1, Data: "1423/05/16"}}},
		{Session: "chat:dates", Turn: 3}, {Session: "chat:dates", Turn: 4}, {Session: "chat:dates", Turn: 5},
	}
	// Turn 4 was made before 0.0.4 (no passage): from there on Day is hidden
	// (user decision 11-05), calendar dates before it stay.
	want := []string{"clock = 1423/05/15", "body = 1423/05/16", "Day 3", "", ""}
	for i, delta := range []string{"9", "2", "1", "7", `{"bad":true}`} {
		j := daySave(t, db, sources[i], delta, fmt.Sprint(i))
		if i == 3 {
			if _, err := db.sql.Exec(`UPDATE gen_jobs SET elapsed_days=NULL WHERE job_id=?`, j.ID); err != nil {
				t.Fatal(err)
			}
			// A completed legacy job retry is not backfilled with Day metadata.
			daySave(t, db, sources[i], "8", "must not replace")
		}
	}
	k, err := db.KnownFor(sources)
	if err != nil || len(k.Memories) != len(want) {
		t.Fatalf("known %+v %v", k, err)
	}
	for i, m := range k.Memories {
		if m.Date != want[i] {
			t.Fatalf("turn %d date %q want %q", m.Turn, m.Date, want[i])
		}
		line := deliveryLine(m, nil, "", nil)
		stamp := fmt.Sprintf("(T%d", m.Turn)
		if want[i] != "" {
			stamp += ", " + want[i]
		}
		if !strings.Contains(line, stamp+")") {
			t.Fatalf("delivery %q lacks %q", line, stamp+")")
		}
	}
	// Missing and malformed deltas are nonfatal zero, while retaining memories.
	for i, raw := range []string{`{"memories":[{"kind":"event","body":"absent"}]}`, `{"elapsed_days":-2,"memories":[{"kind":"event","body":"negative"}]}`} {
		src := memory.Source{Session: fmt.Sprintf("chat:invalid%d", i), Turn: 1}
		j, err := db.JobFor(src)
		if err != nil {
			t.Fatal(err)
		}
		out, notes := memory.Parse(raw)
		if _, err := db.SaveGeneration(j, Generation{Output: out, Notes: notes}); err != nil {
			t.Fatal(err)
		}
		var got sql.NullInt64
		if err := db.sql.QueryRow(`SELECT elapsed_days FROM gen_jobs WHERE job_id=?`, j.ID).Scan(&got); err != nil || !got.Valid || got.Int64 != 0 {
			t.Fatalf("stored delta %+v %v", got, err)
		}
		k, err := db.KnownFor([]memory.Source{src})
		if err != nil || len(k.Memories) != 1 || k.Memories[0].Date != "Day 0" {
			t.Fatalf("zero %+v %v", k, err)
		}
	}
}

func TestDayCanonicalLifecycle(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	u1, a1 := lrow("u1", "user", "", "first"), lrow("a1", "char", "g1", "first reply")
	u2, a2 := lrow("u2", "user", "", "same text"), lrow("a2", "char", "g2", "second reply")
	u3, a3 := lrow("u3", "user", "", "third"), lrow("a3", "char", "g3", "third reply")
	apply := func(src lifecycle.Source, rows ...lifecycle.Row) {
		s.Apply(lsnap(src, rows...))
		if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
			t.Fatal(err)
		}
	}
	apply(lifecycle.SourceOutputEvent, u1, a1)
	daySave(t, db, turnSource(s, 0), "5", "first")
	apply(lifecycle.SourceOutputEvent, u1, a1, u2, a2)
	second := s.ActiveTurns()[1].ID
	j := daySave(t, db, turnSource(s, 1), "1", "second")
	// Same-request retry must neither duplicate rows nor change its saved delta.
	if err := db.StartAttempt(j); err != nil {
		t.Fatal(err)
	}
	jRetry := daySave(t, db, turnSource(s, 1), "20", "retry must not write")
	if j.ID != jRetry.ID || len(s.ActiveTurns()) != 2 {
		t.Fatal("retry appended")
	}
	dayKnown(t, db, memory.Own(s), map[string]string{"first": "Day 0", "second": "Day 1"})
	// Same Host user row reroll replaces the source without appending a turn.
	apply(lifecycle.SourceBeforeRequest, u1, a1, u2)
	a2 = lrow("a2r", "char", "g2r", "rerolled second")
	apply(lifecycle.SourceOutputEvent, u1, a1, u2, a2)
	if len(s.ActiveTurns()) != 2 || s.ActiveTurns()[1].ID != second {
		t.Fatal("reroll appended")
	}
	daySave(t, db, turnSource(s, 1), "2", "rerolled")
	apply(lifecycle.SourceOutputEvent, u1, a1, u2, a2, u3, a3)
	third := s.ActiveTurns()[2].ID
	daySave(t, db, turnSource(s, 2), "3", "third")
	dayKnown(t, db, memory.Own(s), map[string]string{"first": "Day 0", "rerolled": "Day 2", "third": "Day 5"})
	// An interior source revision recomputes the following turn's date.
	a2.Data = "edited interior response"
	apply(lifecycle.SourceLoad, u1, a1, u2, a2, u3, a3)
	daySave(t, db, turnSource(s, 1), "4", "interior")
	dayKnown(t, db, memory.Own(s), map[string]string{"first": "Day 0", "interior": "Day 4", "third": "Day 7"})
	apply(lifecycle.SourceLoad, u1, a1, u3, a3)
	dayKnown(t, db, memory.Own(s), map[string]string{"first": "Day 0", "third": "Day 3"})
	// Delete assistant, edit its same user row, then regenerate: replace.
	apply(lifecycle.SourceBeforeRequest, u1, a1, u3)
	u3.Data = "edited third"
	apply(lifecycle.SourceBeforeRequest, u1, a1, u3)
	a3 = lrow("a3r", "char", "g3r", "edited third reply")
	apply(lifecycle.SourceOutputEvent, u1, a1, u3, a3)
	if len(s.ActiveTurns()) != 2 || s.ActiveTurns()[1].ID != third {
		t.Fatal("edit/regenerate appended")
	}
	daySave(t, db, turnSource(s, 1), "1", "edited")
	// Identical text in a genuinely new Host user row appends.
	u4, a4 := lrow("u4", "user", "", u3.Data), lrow("a4", "char", "g4", "new reply")
	apply(lifecycle.SourceOutputEvent, u1, a1, u3, a3, u4, a4)
	if len(s.ActiveTurns()) != 3 || s.ActiveTurns()[2].ID == third {
		t.Fatal("identical new row replaced")
	}
	daySave(t, db, turnSource(s, 2), "2", "identical new row")
	// A normal new turn appends through the same production owner.
	apply(lifecycle.SourceOutputEvent, u1, a1, u3, a3, u4, a4, lrow("u5", "user", "", "new input"), lrow("a5", "char", "g5", "new response"))
	if len(s.ActiveTurns()) != 4 {
		t.Fatal("normal new row did not append")
	}
	daySave(t, db, turnSource(s, 3), "1", "normal")
	dayKnown(t, db, memory.Own(s), map[string]string{"first": "Day 0", "edited": "Day 1", "identical new row": "Day 3", "normal": "Day 4"})
	loaded, ok, err := db.LoadSession(s.Key)
	if err != nil || !ok {
		t.Fatalf("reload %v %v", ok, err)
	}
	dayKnown(t, db, memory.Own(loaded), map[string]string{"first": "Day 0", "edited": "Day 1", "identical new row": "Day 3", "normal": "Day 4"})
}

func TestDayBranchAndCopy(t *testing.T) {
	db := testDB(t)
	parent := u2Session(t, db, "chat:P", "first", "second", "parent tail")
	for i, delta := range []string{"0", "2", "6"} {
		daySave(t, db, turnSource(parent, i), delta, fmt.Sprintf("parent%d", i))
	}
	// Use actual branch-marker observation and production copy recording.
	rows := []lifecycle.Row{}
	for i, text := range []string{"first", "second"} {
		rows = append(rows, lrow(fmt.Sprintf("chat:Pu%d", i), "user", "", text), lrow(fmt.Sprintf("chat:Pa%d", i), "char", "", text+" 답"))
	}
	marker := lifecycle.Row{ChatID: "marker", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::P::name::chat:Pa1::}}"}
	rows = append(rows, marker, lrow("cu", "user", "", "child input"), lrow("ca", "char", "gc", "child response"))
	child := lifecycle.NewSession("chat:C")
	child.Apply(lsnap(lifecycle.SourceOutputEvent, rows...))
	ok, err := memory.RecordCopies(child, db.LoadSession)
	if err != nil || !ok {
		t.Fatalf("record copies %v %v", ok, err)
	}
	if err := db.SaveSession(child, child.TakeEvents(), child.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	daySave(t, db, turnSource(child, 2), "3", "child")
	want := map[string]string{"parent0": "Day 0", "parent1": "Day 2", "child": "Day 5"}
	dayKnown(t, db, memory.Own(child), want)
	// Deleting the parent materializes inherited jobs with their per-turn
	// deltas; it must not copy the parent's later unbranched tail.
	if _, err := db.ChatOperation("delete", parent.Key, "", false); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := db.LoadSession(child.Key)
	if err != nil || !ok {
		t.Fatalf("load child %v %v", ok, err)
	}
	dayKnown(t, db, memory.Own(loaded), want)

	// Explicit copying preserves the nullable distinction for old records.
	from := u2Session(t, db, "chat:F", "copy first", "copy second")
	to := u2Session(t, db, "chat:T", "copy first", "copy second")
	old := daySave(t, db, turnSource(from, 0), "0", "legacy copy")
	if _, err := db.sql.Exec(`UPDATE gen_jobs SET elapsed_days=NULL WHERE job_id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	daySave(t, db, turnSource(from, 1), "4", "new copy")
	if _, err := db.ChatOperation("copy", from.Key, to.Key, false); err != nil {
		t.Fatal(err)
	}
	dayKnown(t, db, memory.Own(to), map[string]string{"legacy copy": "", "new copy": ""})
}

// A chat begun before 0.0.4 shows no Day for its new turns until the old
// turns are made again; remaking them brings the Day back (user decision 11-05).
func TestDayHiddenAfterLegacyUntilRemade(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:legacy-hide", "old first", "new second", "new third")
	old := daySave(t, db, turnSource(s, 0), "0", "old first")
	if _, err := db.sql.Exec(`UPDATE gen_jobs SET elapsed_days=NULL WHERE job_id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	daySave(t, db, turnSource(s, 1), "1", "new second")
	daySave(t, db, turnSource(s, 2), "2", "new third")
	dayKnown(t, db, memory.Own(s), map[string]string{"old first": "", "new second": "", "new third": ""})
	// the remade first turn gets its passage like any new generation
	if _, err := db.sql.Exec(`UPDATE gen_jobs SET elapsed_days=0 WHERE job_id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	dayKnown(t, db, memory.Own(s), map[string]string{"old first": "Day 0", "new second": "Day 1", "new third": "Day 3"})
}

func TestDayLegacySchemaUpgrade(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:upgrade", Turn: 1}
	j := daySave(t, db, src, "0", "legacy row")
	// Model the actual old schema, not just an old response parsed today.
	if _, err := db.sql.Exec(`ALTER TABLE gen_jobs DROP COLUMN elapsed_days`); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.sql.QueryRow(`SELECT DATABASE()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(os.Getenv("AC2_TEST_MARIADB_DSN") + name + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var delta sql.NullInt64
	if err := upgraded.sql.QueryRow(`SELECT elapsed_days FROM gen_jobs WHERE job_id=?`, j.ID).Scan(&delta); err != nil || delta.Valid {
		t.Fatalf("legacy delta %+v %v", delta, err)
	}
	daySave(t, upgraded, src, "9", "retry must not backfill")
	k, err := upgraded.KnownFor([]memory.Source{src})
	if err != nil || len(k.Memories) != 1 || k.Memories[0].Body != "legacy row" || k.Memories[0].Date != "" {
		t.Fatalf("legacy read %+v %v", k, err)
	}
	if err := upgraded.RemakeGeneration(src); err != nil {
		t.Fatal(err)
	}
	daySave(t, upgraded, src, "0", "remade row")
	k, err = upgraded.KnownFor([]memory.Source{src})
	if err != nil || len(k.Memories) != 1 || k.Memories[0].Body != "remade row" || k.Memories[0].Date != "Day 0" {
		t.Fatalf("remade read %+v %v", k, err)
	}
}

func TestDayUnscopedJobsKeepOnlyCalendar(t *testing.T) {
	db := testDB(t)
	s := u2Session(t, db, "chat:subset", "first", "second", "body 1423/05/16", "variable")
	s.ActiveTurns()[3].Dates = map[string]string{"clock": "1423/05/17"}
	if err := db.SaveSession(s, nil, nil); err != nil {
		t.Fatal(err)
	}
	var jobs []int64
	var sources []memory.Source
	for i, delta := range []string{"0", "2", "3", "1"} {
		src := turnSource(s, i)
		sources = append(sources, src)
		jobs = append(jobs, daySave(t, db, src, delta, fmt.Sprint(i)).ID)
	}
	// An embedding batch may read a lone late job or an arbitrary order.
	// Neither list establishes the story's origin or cumulative day number.
	for _, subset := range [][]int64{{jobs[1]}, {jobs[3], jobs[1], jobs[0], jobs[2]}} {
		k, err := db.KnownOf(subset)
		if err != nil {
			t.Fatal(err)
		}
		if len(k.Memories) != len(subset) {
			t.Fatalf("subset items: %+v", k)
		}
		for _, m := range k.Memories {
			want := ""
			if m.Body == "2" {
				want = "body = 1423/05/16"
			}
			if m.Body == "3" {
				want = "clock = 1423/05/17"
			}
			if m.Date != want {
				t.Fatalf("unscoped %q acquired %q, want %q", m.Body, m.Date, want)
			}
		}
	}
	k, err := db.KnownFor(sources)
	if err != nil || len(k.Memories) != len(sources) {
		t.Fatalf("canonical %+v %v", k, err)
	}
	for i, want := range []string{"Day 0", "Day 2", "body = 1423/05/16", "clock = 1423/05/17"} {
		if k.Memories[i].Date != want {
			t.Fatalf("canonical turn %d date %q, want %q", i, k.Memories[i].Date, want)
		}
	}
}

func TestDayLargeCumulativeTotal(t *testing.T) {
	db := testDB(t)
	maximum := int64(^uint64(0) >> 1)
	delta := fmt.Sprint(maximum)
	var sources []memory.Source
	for i := 0; i < 3; i++ {
		src := memory.Source{Session: "chat:large", Turn: i + 1}
		sources = append(sources, src)
		daySave(t, db, src, delta, fmt.Sprint(i))
	}
	k, err := db.KnownFor(sources)
	if err != nil || len(k.Memories) != len(sources) {
		t.Fatalf("large known %+v %v", k, err)
	}
	want := new(big.Int).Mul(big.NewInt(maximum), big.NewInt(2))
	if k.Memories[0].Date != "Day 0" || k.Memories[2].Date != "Day "+want.String() {
		t.Fatalf("large cumulative dates %+v", k.Memories)
	}
}
