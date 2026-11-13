package memory

import (
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
)

// An emptied reply's marker that shows again without its text (a
// fingerprint, a translation original not read) is unread, not a blank
// reply: no memory is made from it until its text comes, and then the text
// is read.
func TestBlankMarkerRevivedUnreadIsUnread(t *testing.T) {
	row := func(i int, id, role, gen, data string, unread bool) lifecycle.Row {
		return lifecycle.Row{Index: i, ChatID: id, Role: role, GenerationID: gen, Data: data, Unreadable: unread}
	}
	apply := func(s *lifecycle.Session, src lifecycle.Source, rows ...lifecycle.Row) {
		s.Apply(lifecycle.Snapshot{SessionKey: s.Key, Source: src, TargetIndex: -1, Rows: rows})
	}
	s := lifecycle.NewSession("chat:A")
	apply(s, lifecycle.SourceOutputEvent, row(0, "u1", "user", "", "q", false), row(1, "a1", "char", "g1", "A", false))
	apply(s, lifecycle.SourceOutputEvent, row(0, "u1", "user", "", "q", false), row(1, "a2", "char", "g2", "B", false))
	apply(s, lifecycle.SourceLoad, row(0, "u1", "user", "", "q", false), row(1, "a1", "char", "g1", "", true))
	turn := s.ActiveTurns()[0]
	if src := SourceOf(s.Key, turn); src.Readable() {
		t.Fatalf("a revived marker without its text was read: %+v", src)
	}
	// the next input confirms the turn; the blank marker is not marked as
	// the confirmed text, and the turn stays unread
	apply(s, lifecycle.SourceBeforeRequest, row(0, "u1", "user", "", "q", false), row(1, "a1", "char", "g1", "", true), row(2, "u2", "user", "", "next", false))
	if r := turn.Responses[0]; !turn.Confirmed || r.Revisions[len(r.Revisions)-1].Confirmed || SourceOf(s.Key, turn).Readable() {
		t.Fatalf("after the next input: confirmed %v, marker %+v", turn.Confirmed, r.Revisions)
	}
	apply(s, lifecycle.SourceLoad, row(0, "u1", "user", "", "q", false), row(1, "a1", "char", "g1", "A", false))
	if src := SourceOf(s.Key, turn); !src.Readable() || src.Responses[0].Data != "A" {
		t.Fatalf("the text that came was not read: %+v", src)
	}
}
