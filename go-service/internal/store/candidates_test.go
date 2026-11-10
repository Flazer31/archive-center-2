package store

import (
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
)

// The injection search reads the chat's current memory with generation's
// Select over the request text. A record recalled by words the recent turns
// repeat is found when those turns are the request's window, and word
// rarity is not lowered by counting the window's own turns again.
func TestCandidatesWindowRarity(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:C")
	t1 := []lifecycle.Row{lrow("u1", "user", "", "은방울 열쇠를 숨겨라"), lrow("a1", "char", "g1", "그는 은방울 열쇠를 숨겼다")}
	t2 := append(append([]lifecycle.Row{}, t1...), lrow("u2", "user", "", "다음"), lrow("a2", "char", "g2", "비가 왔다"))
	t3 := append(append([]lifecycle.Row{}, t2...), lrow("u3", "user", "", "은방울 이야기"), lrow("a3", "char", "g3", "은방울이 울렸다"))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, t1...))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, t2...))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, t3...))
	if n := len(s.ActiveTurns()); n != 3 {
		t.Fatalf("turns %d", n)
	}
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"memories":[{"id":"m1","kind":"event","body":"은방울 열쇠 숨김"}]}`)
	id := it.Memories[0].ID
	request := "열쇠는 어디 있지\n은방울 이야기\n은방울이 울렸다"
	has := func(window int) bool {
		sel, k, err := db.Candidates(s, request, window)
		if err != nil {
			t.Fatal(err)
		}
		if len(k.Memories) != 1 {
			t.Fatalf("current memory %d records", len(k.Memories))
		}
		for _, m := range sel.Memories {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	if !has(1) {
		t.Fatal("record recalled by the request's words was not found")
	}
	// the same text with the window's turn counted as an earlier turn makes
	// its words look common: the record is no longer recalled
	if has(0) {
		t.Fatal("window turns do not change word rarity; the test does not exercise it")
	}
	if sel, _, _ := db.Candidates(s, "날씨 이야기", 1); len(sel.Memories) != 0 {
		t.Fatalf("an unrelated request found %d records", len(sel.Memories))
	}
}
