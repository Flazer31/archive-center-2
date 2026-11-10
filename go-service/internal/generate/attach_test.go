package generate

import (
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// C22: a stored chat attached as a chat's past is read by the chat's
// generation (its memory is in the earlier memory the model sees) and is
// never made by the chat, also a past turn that chat has not made. Both the
// notice path and the reload path (retry, resume) read it.
func TestAttachedPastIsReadNotMade(t *testing.T) {
	for _, reload := range []bool{false, true} {
		name := "notice"
		if reload {
			name = "reload"
		}
		t.Run(name, func(t *testing.T) {
			db := testDB(t)
			m := &fakeModel{}
			w := NewWorker(db, m, "fake", time.Minute)
			at := func(key string, src lifecycle.Source, rows ...lifecycle.Row) lifecycle.Snapshot {
				sn := snap(src, rows...)
				sn.SessionKey = key
				return sn
			}
			// the stored chat: its first turn made, its second not confirmed
			// (never made there)
			old := lifecycle.NewSession("chat:A")
			old.Apply(at("chat:A", lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "옛 첫째")))
			old.Apply(at("chat:A", lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나"), row("a1", "char", "g1", "옛 첫째"), row("u2", "user", "", "둘")))
			old.Apply(at("chat:A", lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "옛 첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "옛 둘째")))
			run(w, old)
			if m.calls() != 1 {
				t.Fatalf("old chat made %d turns", m.calls())
			}
			// the new chat attaches it, then goes on with its own turn
			c := lifecycle.NewSession("chat:C")
			c.Past = memory.PastOf(old)
			if len(c.Past) != 2 {
				t.Fatalf("past %d turns", len(c.Past))
			}
			c.Apply(at("chat:C", lifecycle.SourceOutputEvent, row("v1", "user", "", "새"), row("b1", "char", "g9", "옛 첫째 기록을 떠올린 새 이야기")))
			goOn(c, row("v1", "user", "", "새"), row("b1", "char", "g9", "옛 첫째 기록을 떠올린 새 이야기"))
			save(w, c)
			if err := db.SetPast(c.Key, c.Past); err != nil {
				t.Fatal(err)
			}
			n := notice{turns: memory.WithPast(c)}
			if reload {
				n = notice{reload: true}
			}
			chatNow(w, c.Key, n)
			if m.calls() != 2 {
				t.Fatalf("new chat made %d turns, want only its own", m.calls()-1)
			}
			p := m.prompts[1]
			known := p[strings.LastIndex(p, "\nKNOWN:"):]
			known = known[:strings.Index(known, "\nTHIS TURN (")]
			if !strings.Contains(p, "새 이야기") || !strings.Contains(known, "옛 첫째 기록") {
				t.Fatalf("the past was not read:\n%s", known)
			}
			// what generation reads is the chat's scope: the past before its own
			got := memory.ScopeOf(c)
			if len(got) != 4 || !got[0].Past || !got[1].Past || got[2].Past || got[2].Source.Session != "chat:C" || got[0].Source.Session != "chat:A" {
				t.Fatalf("scope %+v", got)
			}
			if asked, err := w.Backfill(c.Key, memory.WithPast(c), 0, false); err != nil || len(asked) != 0 {
				t.Fatalf("backfill asked %v %v", asked, err)
			}
			if done, err := db.DoneKeys("chat:A"); err != nil || len(done) != 1 {
				t.Fatalf("the old chat's unmade turn was made by the new chat: %v %v", done, err)
			}
		})
	}
}
