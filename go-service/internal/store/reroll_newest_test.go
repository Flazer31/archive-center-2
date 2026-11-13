package store

import (
	"reflect"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
)

func activeText(t *lifecycle.Turn) string {
	for _, r := range t.Responses {
		if r.State == lifecycle.Active && len(r.Revisions) > 0 {
			return r.Revisions[len(r.Revisions)-1].Data
		}
	}
	return ""
}

// Only the newest reply value is stored: an edit overwrites the reply's
// text, a reroll empties the replaced reply's text (the row stays as a
// marker of its turn), a plain delete keeps the row (deleted) so it can
// still come back, and the stored state reads back as the session holds
// it.
func TestRerollAndEditStoreNewestOnly(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	save := func() {
		t.Helper()
		if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.sql.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	text := func(row string) []string {
		t.Helper()
		var out []string
		rows, err := db.sql.Query(`SELECT data FROM revisions WHERE session_key='chat:A' AND row_key=? ORDER BY seq`, row)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			rows.Scan(&d)
			out = append(out, d)
		}
		return out
	}
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a1", "char", "g1", "첫 답")))
	save()
	// an edit of the reply (same generation): its text is overwritten
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "입력"), lrow("a1", "char", "g1", "고친 답")))
	save()
	if got := text("c:a1"); !reflect.DeepEqual(got, []string{"고친 답"}) {
		t.Fatalf("after edit the stored texts are %q, want only the newest", got)
	}
	// a reroll in the same row (new generation): overwritten too
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a1", "char", "g2", "같은 줄 리롤")))
	save()
	if got := text("c:a1"); !reflect.DeepEqual(got, []string{"같은 줄 리롤"}) {
		t.Fatalf("after a same-row reroll the stored texts are %q", got)
	}
	// a reroll with a new reply row: the replaced row stays as an empty
	// marker (state replaced, one blank text), no text of it is stored
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a2", "char", "g3", "새 답")))
	save()
	if n := count(`SELECT COUNT(*) FROM response_rows WHERE session_key='chat:A' AND row_key='c:a1' AND state='replaced'`); n != 1 {
		t.Fatalf("the replaced reply's marker is stored %d times", n)
	}
	if got := text("c:a1"); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("the replaced reply's text is still stored: %q", got)
	}
	if got := text("c:a2"); !reflect.DeepEqual(got, []string{"새 답"}) {
		t.Fatalf("the new reply is stored as %q", got)
	}
	// a plain delete (no new reply) keeps the row as deleted
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "입력")))
	save()
	var state string
	if err := db.sql.QueryRow(`SELECT state FROM response_rows WHERE session_key='chat:A' AND row_key='c:a2'`).Scan(&state); err != nil || state != "deleted" {
		t.Fatalf("a deleted reply must stay stored as deleted: %q %v", state, err)
	}
	// RisuAI's reroll: delete first, then the new reply replaces it
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a3", "char", "g4", "다시 만든 답")))
	save()
	if n := count(`SELECT COUNT(*) FROM response_rows WHERE session_key='chat:A' AND state='active'`); n != 1 {
		t.Fatalf("after the delete-first reroll %d active reply rows are stored, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM revisions WHERE session_key='chat:A' AND data<>''`); n != 1 {
		t.Fatalf("after the rerolls %d reply texts are stored, want 1", n)
	}
	// the stored order is the session's order (a reload reads the same)
	if n := count(`SELECT COUNT(DISTINCT ord) FROM response_rows WHERE session_key='chat:A'`); n != count(`SELECT COUNT(*) FROM response_rows WHERE session_key='chat:A'`) {
		t.Fatalf("reply rows share an order number")
	}
	got, ok, err := db.LoadSession("chat:A")
	if err != nil || !ok {
		t.Fatalf("load ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got.Turns, s.Turns) {
		t.Fatalf("stored state differs from the session")
	}
	if len(got.Turns) != 1 || len(got.ActiveTurns()) != 1 || activeText(got.Turns[0]) != "다시 만든 답" {
		t.Fatalf("reloaded turn: %+v", got.Turns)
	}
}
