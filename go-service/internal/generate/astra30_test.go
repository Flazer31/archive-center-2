package generate

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
	"context"
	"fmt"
	"testing"
	"time"
)

// Memory-only fake: no SQL writes, HTTP, provider or embedding calls.
type a30Store struct {
	jobs     map[string]store.Job
	sources  map[int64]memory.Source
	sessions map[string]*lifecycle.Session
	made     []memory.Source
	grants   map[string]map[int]bool
}

func a30New() *a30Store {
	return &a30Store{jobs: map[string]store.Job{}, sources: map[int64]memory.Source{}, sessions: map[string]*lifecycle.Session{}, grants: map[string]map[int]bool{}}
}
func (d *a30Store) JobFor(s memory.Source) (store.Job, error) {
	if j, ok := d.jobs[s.Key()]; ok {
		return j, nil
	}
	j := store.Job{ID: int64(len(d.jobs) + 1), Session: s.Session, Turn: s.Turn, SourceKey: s.Key(), State: "requested"}
	d.jobs[s.Key()] = j
	d.sources[j.ID] = s
	return j, nil
}
func (d *a30Store) CurrentJobs(s []memory.Source) ([]int64, error) {
	var out []int64
	for _, x := range s {
		if j, ok := d.jobs[x.Key()]; ok && j.State == "done" {
			out = append(out, j.ID)
		}
	}
	return out, nil
}
func (d *a30Store) KnownFor(s []memory.Source) (memory.Known, error) { return memory.Known{}, nil }
func (d *a30Store) SaveGeneration(j store.Job, g store.Generation) ([]string, error) {
	j.State = "done"
	d.jobs[j.SourceKey] = j
	d.made = append(d.made, d.sources[j.ID])
	return nil, nil
}
func (d *a30Store) FailJob(j store.Job, m, r, i, e string, at time.Time) error { return nil }
func (d *a30Store) KeepResponse(j store.Job, m, r, i, a string) error          { return nil }
func (d *a30Store) StartAttempt(j store.Job) error                             { return nil }
func (d *a30Store) KeepUnreadable(j store.Job, m, r, i, a string, n []string, at time.Time) error {
	return nil
}
func (d *a30Store) SessionKeys() ([]string, error)  { return nil, nil }
func (d *a30Store) RearmFailedJobs() (int64, error) { return 0, nil }
func (d *a30Store) Grant(s string, turns []int) error {
	if d.grants[s] == nil {
		d.grants[s] = map[int]bool{}
	}
	for _, t := range turns {
		d.grants[s][t] = true
	}
	return nil
}
func (d *a30Store) Grants(s string) (map[int]bool, error) {
	out := map[int]bool{}
	for t := range d.grants[s] {
		out[t] = true
	}
	return out, nil
}
func (d *a30Store) Ungrant(s string, t int) error { delete(d.grants[s], t); return nil }
func (d *a30Store) DoneKeys(s string) (map[string]bool, error) {
	out := map[string]bool{}
	for k, j := range d.jobs {
		if j.Session == s && j.State == "done" {
			out[k] = true
		}
	}
	return out, nil
}
func (d *a30Store) LoadSession(k string) (*lifecycle.Session, bool, error) {
	s, ok := d.sessions[k]
	return s, ok, nil
}

type a30Model struct{}

func (a30Model) Chat(context.Context, string) (provider.Result, error) {
	return provider.Result{Content: `{"memories":[]}`}, nil
}
func a30Session(key string, rows ...lifecycle.Row) *lifecycle.Session {
	s := lifecycle.NewSession(key)
	for i := range rows {
		rows[i].Index = i
	}
	s.Apply(lifecycle.Snapshot{SessionKey: key, Source: lifecycle.SourceLoad, Rows: rows, TargetIndex: -1})
	return s
}
func a30Rows(n int) []lifecycle.Row {
	var r []lifecycle.Row
	for i := 1; i <= n; i++ {
		r = append(r, lifecycle.Row{ChatID: fmt.Sprintf("u%d", i), Role: "user", Data: fmt.Sprintf("질문%d", i)}, lifecycle.Row{ChatID: fmt.Sprintf("a%d", i), Role: "char", Data: fmt.Sprintf("답%d", i)})
	}
	return r
}
func TestA30BranchQuotaAndLingeringGrant(t *testing.T) {
	d := a30New()
	p := a30Session("chat:P", a30Rows(1)...)
	d.sessions[p.Key] = p
	rows := a30Rows(2)
	rows = append(rows, lifecycle.Row{ChatID: "marker", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::P::parent::a1::}}"})
	c := a30Session("chat:C", rows...)
	recordBranch(d, c)
	w := NewWorker(d, a30Model{}, "fake", time.Second)
	asked, e := w.Backfill(c.Key, memory.Own(c), 1, false)
	if e != nil {
		t.Fatal(e)
	}
	chatNow(w, c.Key, notice{turns: memory.Own(c)})
	t.Logf("asked=%v generated=%d grants=%v branch=%v", asked, len(d.made), d.grants[c.Key], c.BranchOf)
	if len(asked) != 1 || asked[0] != 2 || len(d.made) != 1 || d.made[0].Turn != 2 || d.grants[c.Key][1] {
		t.Fatal("a copied parent turn took the request")
	}
	// Same copied historical turn is later edited, still unconfirmed. No new Backfill request.
	rows[1].Data = "자식이 편집한 과거"
	for i := range rows {
		rows[i].Index = i
	}
	c.Apply(lifecycle.Snapshot{SessionKey: c.Key, Source: lifecycle.SourceLoad, Rows: rows, TargetIndex: -1})
	chatNow(w, c.Key, notice{turns: memory.Own(c)})
	t.Logf("after unconfirmed child edit generated=%d source=%+v", len(d.made), d.made)
	if len(d.made) != 1 {
		t.Fatal("an edit was made from a grant nobody asked for")
	}
}
func TestA30RecentSelectVsExecution(t *testing.T) {
	d := a30New()
	s := a30Session("chat:H", a30Rows(3)...)
	w := NewWorker(d, a30Model{}, "fake", time.Second)
	asked, _ := w.Backfill(s.Key, memory.Own(s), 2, true)
	chatNow(w, s.Key, notice{turns: memory.Own(s)})
	var turns []int
	for _, x := range d.made {
		turns = append(turns, x.Turn)
	}
	t.Logf("asked=%v execution=%v", asked, turns)
	if fmt.Sprint(asked) != "[3 2]" || fmt.Sprint(turns) != "[2 3]" {
		t.Fatal("order observation changed")
	}
}
func TestA30BackfillCanOverwriteNewerNotice(t *testing.T) {
	d := a30New()
	s := a30Session("chat:H", a30Rows(1)...)
	old := memory.Own(s)
	rows := a30Rows(1)
	rows[1].Data = "편집된 최신 응답"
	s = a30Session(s.Key, rows...)
	fresh := memory.Own(s)
	w := NewWorker(d, a30Model{}, "fake", time.Second)
	w.Notify(s.Key, fresh)
	w.Backfill(s.Key, old, 1, false)
	key, n, ok := w.next(true)
	if !ok {
		t.Fatal("no notice")
	}
	chatNow(w, key, n)
	t.Logf("latest=%q generated=%q pending=%d", fresh[0].Source.Responses[0].Data, d.made[0].Responses[0].Data, len(w.order))
	if d.made[0].Responses[0].Data != "편집된 최신 응답" || len(w.order) != 0 {
		t.Fatal("an older request replaced the newer notice")
	}
}
func TestA30ReadinessControls(t *testing.T) {
	d := a30New()
	s := lifecycle.NewSession("chat:H")
	rows := a30Rows(3)
	for i := range rows {
		rows[i].Index = i
	}
	s.Apply(lifecycle.Snapshot{SessionKey: s.Key, Source: lifecycle.SourceLoad, Rows: rows, TargetIndex: -1})
	w := NewWorker(d, a30Model{}, "fake", time.Second)
	chatNow(w, s.Key, notice{turns: memory.Own(s)})
	if len(d.made) != 0 {
		t.Fatal("loaded history ran automatically")
	}
	rows = append(rows, lifecycle.Row{Index: 6, ChatID: "u4", Role: "user", Data: "새 입력"})
	s.Apply(lifecycle.Snapshot{SessionKey: s.Key, Source: lifecycle.SourceBeforeRequest, Rows: rows, TargetIndex: -1})
	chatNow(w, s.Key, notice{turns: memory.Own(s)})
	if len(d.made) != 1 {
		t.Fatalf("generated=%+v", d.made)
	}
	t.Logf("loaded history: zero; after the next input: generated turn %d", d.made[0].Turn)
}
