package generate

import (
	"database/sql"
	"os"
	"regexp"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// 1.4 I260: a history backfill asks for a pair not yet confirmed, and while
// its model call is under way the next input confirms the same pair. Both
// reach the production worker; the pair gets one job and one model call,
// and its memory is stored once.
func TestBackfillAndNextInputMakeOnePairOnce(t *testing.T) {
	db := testDB(t)
	m := concurrencyModel(map[string]string{"history": "A"})
	gate := make(chan struct{})
	m.gates["history"] = gate
	w := NewWorker(db, m, "synthetic", time.Minute)
	runFor(t, w)
	s := lifecycle.NewSession("chat:A")
	u, a := row("u1", "user", "", "input"), row("a1", "char", "g1", "history")
	s.Apply(a28Snap(s.Key, lifecycle.SourceOutputEvent, u, a))
	save(w, s)
	asked, err := w.Backfill(s.Key, memory.Own(s), 0, false)
	if err != nil || len(asked) != 1 {
		t.Fatalf("the backfill must ask for the unconfirmed pair: %v %v", asked, err)
	}
	waitUntil(t, "the backfill's call under way", func() bool { return m.count("history") == 1 })
	s.Apply(goOn(s, u, a)) // the next input confirms the same pair meanwhile
	notify(w, s)
	close(gate)
	src := concurrencySource(s, 0)
	waitUntil(t, "the pair stored", func() bool { return a57Done(db, src) })
	waitUntil(t, "chat idle", func() bool { return !w.Chat(s.Key).Running && !w.Chat(s.Key).Waiting })
	if n := m.count("history"); n != 1 {
		t.Fatalf("model calls for the pair: %d, want 1", n)
	}
	raw, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_gen_"+regexp.MustCompile(`\W`).ReplaceAllString(t.Name(), "_"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var jobs int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM gen_jobs WHERE session_key=? AND turn_id=?`, s.Key, s.ActiveTurns()[0].ID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("jobs for the pair: %d, want 1", jobs)
	}
	if got := bodies(t, db, s); len(got) != 1 || len(got[1]) != 1 {
		t.Fatalf("stored memory: %v, want one item once", got)
	}
}
