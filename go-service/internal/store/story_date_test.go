package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// Exercise the observed-chat -> persisted job -> actual delivery path. The
// model boundary is a written synthetic memory; no provider is called.
func TestStoryDateObservedBodySavedAndDelivered(t *testing.T) {
	db := testDB(t)
	for _, tc := range []struct {
		name, body, variable, want string
	}{
		{"variable", "응답\n1423/05/14", "1423/05/15", "clock = 1423/05/15"},
		{"slash", "응답\n1423/05/15", "", "body = 1423/05/15"},
		{"dash", "응답\n2900-09-24", "", "body = 2900-09-24"},
		{"dot", "응답\n2900.08.15", "", "body = 2900.08.15"},
		{"last_date", "1423/05/14 과거 이야기\n현재 1423/05/15 13:00", "", "body = 1423/05/15 13:00"},
		{"undated", "응답에 날짜 없음", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := map[string]any{"chat_id": tc.name, "rows": []any{
				map[string]any{"i": 0, "role": "user", "chat_id": "u", "data": map[string]string{"text": "기록해"}},
				map[string]any{"i": 1, "role": "char", "chat_id": "a", "data": map[string]string{"text": tc.body}},
			}}
			if tc.variable != "" {
				chat["scriptstate"] = map[string]any{"clock": map[string]string{"text": tc.variable}}
			}
			raw, _ := json.Marshal(map[string]any{"hook": "load", "stored_chat": chat})
			observed, err := ingest.FromEvent(raw, ingest.NewTextCache())
			if err != nil || len(observed) != 1 {
				t.Fatalf("observe: %v, %v", observed, err)
			}
			s := lifecycle.NewSession(observed[0].Snapshot.SessionKey)
			s.Apply(observed[0].Snapshot)
			if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
				t.Fatal(err)
			}
			src := memory.SourceOf(s.Key, s.ActiveTurns()[0])
			key := src.Key()
			job, err := db.JobFor(src)
			if err != nil {
				t.Fatal(err)
			}
			out := parse(t, `{"entities":[{"id":"n","name":"리나","type":"person"}],"memories":[{"id":"m","kind":"fact","about":"n","slot":"직업","body":"서기"}]}`)
			if _, err := db.SaveGeneration(job, Generation{Output: out}); err != nil {
				t.Fatal(err)
			}
			sources, err := db.JobSources([]int64{job.ID})
			if err != nil || memory.DateText(sources[job.ID].Dates) != tc.want || sources[job.ID].Key() != key {
				t.Fatalf("stored date/key: %+v, err %v, want %q", sources[job.ID], err, tc.want)
			}
			restarted, _, err := db.LoadSession(s.Key)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s.Turns[0].Dates, restarted.Turns[0].Dates) {
				t.Fatal("job body date changed the turn variables")
			}
			k, err := db.KnownFor([]memory.Source{memory.SourceOf(restarted.Key, restarted.Turns[0])})
			wantDate := tc.want
			if wantDate == "" {
				wantDate = "Day 0" // a newly saved undated turn has a zero delta
			}
			if err != nil || len(k.Memories) != 1 || k.Memories[0].Date != wantDate {
				t.Fatalf("known date: %+v, %v", k, err)
			}
			d, err := db.Deliver(memory.Own(restarted), DeliveryRequest{Input: "리나", Payload: []TextPrint{{Role: "user"}}, Chars: 10000})
			stamp := "(T1"
			if wantDate != "" {
				stamp += ", " + wantDate
			}
			stamp += ")"
			if err != nil || len(d.IDs) != 1 || !strings.Contains(d.Block, stamp) {
				t.Fatalf("delivery: %+v, %v; want %q", d, err, stamp)
			}
		})
	}
}

func TestStoryDateExistingJobSourceNotBackfilled(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:old", Turn: 1, Responses: []memory.SourceResponse{{Seq: 1, Data: "응답 1423/05/15"}}}
	// Seed an already completed pre-fix memory, including its original
	// source bytes. Retrying must preserve both the source and memory rows.
	old, _ := json.Marshal(src)
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	g := Generation{Output: parse(t, `{"memories":[{"kind":"event","body":"기존 기억"}]}`)}
	if _, err := db.SaveGeneration(j, g); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE gen_jobs SET source=?, elapsed_days=NULL WHERE job_id=?`, string(old), j.ID); err != nil {
		t.Fatal(err)
	}
	before := count(t, db, "memories")
	j, err = db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveGeneration(j, g); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := db.sql.QueryRow(`SELECT source FROM gen_jobs WHERE job_id=?`, j.ID).Scan(&after); err != nil || after != string(old) {
		t.Fatalf("existing source changed: %q, %v", after, err)
	}
	if count(t, db, "memories") != before {
		t.Fatal("retry changed the completed memories")
	}
	k, _, err := db.knownScope([]memory.ScopedTurn{{Source: src}}, false)
	if err != nil || len(k.Memories) != 1 || k.Memories[0].Date != "" {
		t.Fatalf("old memory acquired a date: %+v, %v", k, err)
	}
}

func TestStoryDateNewJobsUseOwnTurnAndCopiedScope(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	rows := []lifecycle.Row{
		lrow("u1", "user", "", "첫 입력"), lrow("a1", "char", "g1", "첫 응답 1423/05/15"),
		lrow("u2", "user", "", "다음 입력"), lrow("a2", "char", "g2", "다음 응답 1423/05/16"),
		lrow("off", "char", "g3", "꺼진 응답 1423/05/30"), lrow("u3", "user", "", "날짜 없는 새 입력"),
	}
	rows[4].Disabled = "true"
	s.Apply(lsnap(lifecycle.SourceLoad, rows...))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	scope := memory.Own(s)
	tail, err := db.JobFor(scope[2].Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveGeneration(tail, Generation{Output: parse(t, `{"memories":[{"kind":"event","body":"日付なしの合成記録"}]}`)}); err != nil {
		t.Fatal(err)
	}
	tailSources, err := db.JobSources([]int64{tail.ID})
	if err != nil || len(tailSources[tail.ID].Dates) != 0 {
		t.Fatalf("undated new input borrowed a previous turn's date: %+v, %v", tailSources, err)
	}
	want := []string{"body = 1423/05/15", "body = 1423/05/16"}
	for i := range want {
		j, err := db.JobFor(scope[i].Source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.SaveGeneration(j, Generation{Output: parse(t, `{"memories":[{"kind":"event","body":"合成の記録"}]}`)}); err != nil {
			t.Fatal(err)
		}
	}
	// A copied scope's empty variables must not borrow the parent's later
	// variable clock. Only its job's recorded body date is the fallback.
	s.Turns[0].Dates = map[string]string{"clock": "1423/06/01"}
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	k, _, err := db.knownScope(scope[:2], false)
	if err != nil || len(k.Memories) != len(want) {
		t.Fatalf("scope: %+v, %v", k, err)
	}
	for i, m := range k.Memories {
		if m.Date != want[i] {
			t.Fatalf("turn %d has date %q, want %q", m.Turn, m.Date, want[i])
		}
	}
}

func TestStoryDateRemakeCreatesDatedSource(t *testing.T) {
	db := testDB(t)
	src := memory.Source{Session: "chat:remake", Turn: 1, Members: []memory.SourceMember{{Data: "1423/05/15에 기록해"}}}
	if err := db.RemakeGeneration(src); err != nil {
		t.Fatal(err)
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveGeneration(j, Generation{Output: parse(t, `{"memories":[{"kind":"event","body":"새 기억"}]}`)}); err != nil {
		t.Fatal(err)
	}
	sources, err := db.JobSources([]int64{j.ID})
	if err != nil || memory.DateText(sources[j.ID].Dates) != "body = 1423/05/15" || sources[j.ID].Key() != src.Key() {
		t.Fatalf("remade source: %+v, %v", sources, err)
	}
}

func TestStoryDateExistingPendingJobFirstSave(t *testing.T) {
	db := testDB(t)
	for _, state := range []string{"requested", "failed", "unreadable", "received"} {
		t.Run(state, func(t *testing.T) {
			src := memory.Source{Session: "chat:" + state, Turn: 1, Responses: []memory.SourceResponse{{Seq: 1, Data: "응답 1423/05/15"}}}
			j, err := db.JobFor(src)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.sql.Exec(`UPDATE gen_jobs SET state=? WHERE job_id=?`, state, j.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SaveGeneration(j, Generation{Output: parse(t, `{"memories":[{"kind":"event","body":"처음 저장되는 기억"}]}`)}); err != nil {
				t.Fatal(err)
			}
			k, err := db.KnownFor([]memory.Source{src})
			if err != nil || len(k.Memories) != 1 || k.Memories[0].Date != "body = 1423/05/15" {
				t.Fatalf("first save lost date: %+v, %v", k, err)
			}
		})
	}
}
