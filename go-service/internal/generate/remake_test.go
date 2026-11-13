package generate

import (
	"errors"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
)

// This boundary forces both response storage paths to fail for the old
// job. Parsing, prompts, worker recovery and the new job use production
// code and MariaDB. It detects reusing the old held answer in a remake.
type u3FailOldStore struct {
	*store.DB
	old int64
}

func (d *u3FailOldStore) KeepResponse(j store.Job, model, request, items, response string) error {
	if j.ID == d.old {
		return errors.New("old response store unavailable")
	}
	return d.DB.KeepResponse(j, model, request, items, response)
}
func (d *u3FailOldStore) SaveGeneration(j store.Job, g store.Generation) ([]string, error) {
	if j.ID == d.old {
		return nil, errors.New("old generation store unavailable")
	}
	return d.DB.SaveGeneration(j, g)
}

func TestU3RemakeDoesNotReuseRetiredHeldAnswer(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceLoad, row("u1", "user", "", "ask"), row("a1", "char", "g1", "reply")))
	src := memory.Own(s)[0].Source
	if err := db.Grant(s.Key, []memory.Source{src}); err != nil {
		t.Fatal(err)
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	d := &u3FailOldStore{DB: db, old: j.ID}
	m := &fakeModel{}
	w := NewWorker(d, m, "fake", time.Second)
	run(w, s)
	if m.calls() != 1 || len(w.held) == 0 {
		t.Fatalf("held-answer setup failed calls %d held %v", m.calls(), w.held)
	}
	if err := db.RemakeGeneration(src); err != nil {
		t.Fatal(err)
	}
	run(w, s)
	if m.calls() != 2 {
		t.Fatalf("retired answer reused: calls %d", m.calls())
	}
	current, err := db.JobFor(src)
	if err != nil || current.ID == j.ID || current.State != "done" {
		t.Fatalf("current %+v %v", current, err)
	}
	if len(w.held) != 0 {
		t.Fatalf("obsolete held answer retained: %v", w.held)
	}
}
