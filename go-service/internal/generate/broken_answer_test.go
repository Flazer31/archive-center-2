package generate

import (
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// A broken answer from which nothing could be read is made again once
// (1.1); a second broken answer is stored as it reads (no loop over the
// kept answer); "{}" stays an answer.
func TestBrokenAnswerIsMadeAgainOnce(t *testing.T) {
	const broken = `{"memories":[17]}` // a response object, no item readable
	for _, tc := range []struct {
		name    string
		replies []string
		calls   int
		bodies  int
	}{
		{"then good", []string{broken, goodAnswer}, 2, 1},
		{"broken twice", []string{broken, broken, goodAnswer}, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			shortWaits(t, 100*time.Millisecond)
			m := &scriptModel{replies: tc.replies}
			w := NewWorker(db, m, "fake", time.Minute)
			s := lifecycle.NewSession("chat:A")
			s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
			goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
			run(w, s)
			src := memory.Own(s)[0].Source
			if j, _ := db.JobFor(src); j.State != "unreadable" || j.Response != broken || m.calls() != 1 {
				t.Fatalf("after the broken answer: %+v calls %d", j, m.calls())
			}
			runFor(t, w)
			if !waitFor(t, func() bool { j, _ := db.JobFor(src); return j.State == "done" }) {
				j, _ := db.JobFor(src)
				t.Fatalf("not stored: %+v calls %d", j, m.calls())
			}
			// a few more runs do not call again
			run(w, s)
			run(w, s)
			if m.calls() != tc.calls || len(bodies(t, db, s)[1]) != tc.bodies {
				t.Fatalf("calls %d (want %d), bodies %v", m.calls(), tc.calls, bodies(t, db, s))
			}
		})
	}
}

// A failed call before the broken answer does not use up its one more
// request (the calls are counted apart from it).
func TestBrokenAnswerAfterFailedCall(t *testing.T) {
	const broken = `{"memories":[17]}`
	db := testDB(t)
	shortWaits(t, 100*time.Millisecond)
	m := &scriptModel{replies: []string{"", broken, goodAnswer}}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	runFor(t, w)
	src := memory.Own(s)[0].Source
	if !waitFor(t, func() bool { j, _ := db.JobFor(src); return j.State == "done" }) || m.calls() != 3 || len(bodies(t, db, s)[1]) != 1 {
		j, _ := db.JobFor(src)
		t.Fatalf("job %+v calls %d bodies %v", j, m.calls(), bodies(t, db, s))
	}
}

// A broken answer that still gave its elapsed days is stored at once (the
// days are kept), not made again.
func TestBrokenAnswerWithDaysIsStored(t *testing.T) {
	db := testDB(t)
	m := &scriptModel{replies: []string{`{"elapsed_days":2,"memories":[17]}`, goodAnswer}}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	run(w, s)
	if j, _ := db.JobFor(memory.Own(s)[0].Source); j.State != "done" || m.calls() != 1 {
		t.Fatalf("job %q calls %d", j.State, m.calls())
	}
}

// With no retries set, a broken answer is not made again: the call limit
// holds (the kept answer is not read as one to store without a call).
func TestBrokenAnswerKeepsTheCallLimit(t *testing.T) {
	db := testDB(t)
	m := &scriptModel{replies: []string{`{"memories":[17]}`, goodAnswer}}
	w := NewWorker(db, m, "fake", time.Minute)
	w.Retries, w.now = 0, func() time.Time { return time.Now().Add(24 * time.Hour) }
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	for i := 0; i < 4; i++ {
		run(w, s)
	}
	if j, _ := db.JobFor(memory.Own(s)[0].Source); m.calls() != 1 || j.State != "unreadable" || w.Diagnostics()["generation_given_up_turns"] != 1 {
		t.Fatalf("calls %d job %+v diag %v", m.calls(), j, w.Diagnostics())
	}
}

// The days a broken answer still gave are the days stored: the turn after
// it is read on that day.
func TestBrokenAnswerKeepsItsDays(t *testing.T) {
	db := testDB(t)
	m := &scriptModel{replies: []string{
		`{"memories":[{"id":"m1","kind":"fact","body":"첫째 기록"}]}`,
		`{"elapsed_days":2,"memories":[17]}`,
		`{"memories":[{"id":"m1","kind":"fact","body":"셋째 기록"}]}`,
	}}
	w := NewWorker(db, m, "fake", time.Minute)
	runFor(t, w)
	s := threeTurns("chat:BD")
	save(w, s)
	if _, err := w.Backfill(s.Key, memory.Own(s), 0, false); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	for _, p := range memory.Own(s) {
		srcs = append(srcs, p.Source)
	}
	if !waitFor(t, func() bool {
		jobs, err := db.CurrentJobs(srcs)
		return err == nil && len(jobs) == 3
	}) {
		t.Fatalf("not all made: calls %d", m.calls())
	}
	k, err := db.KnownFor(srcs)
	if err != nil {
		t.Fatal(err)
	}
	days := map[string]string{}
	for _, x := range k.Memories {
		days[x.Body] = x.Date
	}
	if days["첫째 기록"] != "Day 0" || days["셋째 기록"] != "Day 2" || m.calls() != 3 {
		t.Fatalf("days %v calls %d", days, m.calls())
	}
}
