package generate

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
	"context"
	"strings"
	"testing"
	"time"
)

// The branch records both old sources before a later edit in the parent.
// The parent's second source has not been generated. No storage/network failure.

type a58MissingOwner struct{ *store.DB }

func (d *a58MissingOwner) LoadSession(key string) (*lifecycle.Session, bool, error) {
	if key == "chat:P" {
		return nil, false, nil
	}
	return d.DB.LoadSession(key)
}

func TestA58NewerDoesNotRejectReceivedOrHeld(t *testing.T) {
	for _, mode := range []string{"fresh", "received", "held"} {
		t.Run(mode, func(t *testing.T) {
			db := testDB(t)
			m := &fakeModel{}
			s := lifecycle.NewSession("chat:A")
			s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, row("u", "user", "", "question"), row("a", "char", "g", "answer")))
			src := memory.ScopeOf(s)[0].Source
			j, e := db.JobFor(src)
			if e != nil {
				t.Fatal(e)
			}
			answer := &Answer{Model: "fake", Request: "original prompt", ReadItems: "original reads", Response: `{"memories":[{"id":"m1","kind":"fact","body":"kept answer"}]}`}
			var held *Answer
			if mode == "received" {
				if e = db.KeepResponse(j, answer.Model, answer.Request, answer.ReadItems, answer.Response); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "held" {
				held = answer
			}
			checks := 0
			r, e := turn(context.Background(), db, m, "fake", nil, 0, src, held, func() bool { checks++; return true }, 0)
			j, _ = db.JobFor(src)
			t.Logf("mode=%s calls=%d checks=%d state=%s attempts=%d reused=%v err=%v", mode, m.calls(), checks, j.State, j.Attempts, r.Reused, e)
			if m.calls() != 0 || j.Attempts != 0 {
				t.Fatal("unexpected attempt")
			}
			if mode == "fresh" {
				if e != errNewer || checks != 1 {
					t.Fatal("fresh call not handed over")
				}
			} else if e != nil || j.State != "done" || checks != 0 || !r.Reused {
				t.Fatal("answer in hand rejected")
			}
		})
	}
}

func TestA58NoticeDuringKnownEdit(t *testing.T) {
	db := testDB(t)
	d := &a57Store{DB: db}
	m := &fakeModel{}
	w := NewWorker(d, m, "fake", time.Second)
	s := lifecycle.NewSession("chat:A")
	rows := []lifecycle.Row{row("u1", "user", "", "first"), row("a1", "char", "g1", "first answer"), row("u2", "user", "", "second"), row("a2", "char", "g2", "old second"), row("u3", "user", "", "next")}
	s.Apply(a28Snap(s.Key, lifecycle.SourceLoad, rows[:4]...))
	s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, rows...))
	old := memory.ScopeOf(s)[1].Source
	if _, e := w.Backfill(s.Key, memory.Own(s), 0, false); e != nil {
		t.Fatal(e)
	}
	var fresh memory.Source
	changed := make(chan struct{})
	injected := false
	d.afterKnown = func(earlier []memory.Source) {
		if injected || len(earlier) != 1 {
			return
		}
		injected = true
		rows[3].Data = "chosen second"
		s.Apply(a28Snap(s.Key, lifecycle.SourceBeforeRequest, rows...))
		fresh = memory.ScopeOf(s)[1].Source
		notify(w, s)
		close(changed)
	}
	notify(w, s)
	stop := running(t, w)
	defer stop()
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("no interleaving")
	}
	waitUntil(t, "edited source saved", func() bool { return a57Done(db, fresh) })
	stop()
	j, _ := db.JobFor(old)
	t.Logf("calls=%d old_state=%s old_attempts=%d latestDone=%v", m.calls(), j.State, j.Attempts, a57Done(db, fresh))
	if m.calls() != 2 || j.Attempts != 0 || !a57Done(db, fresh) {
		t.Fatal("old call made or edited turn lost")
	}
	for _, p := range m.prompts {
		if strings.Contains(p, "REPLY: old second") {
			t.Fatal("old revision called")
		}
	}
}
