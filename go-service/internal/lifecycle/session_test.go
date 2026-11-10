package lifecycle

import (
	"fmt"
	"testing"
)

// Snapshots are built in the shape RisuAI stores chats (Message.chatId,
// role, data, generationInfo.generationId). Live-host recording is pending
// (STEP1-TEST-LOG.md), so these are source-shaped sequences, not recordings.

func u(id, data string) Row      { return Row{ChatID: id, Role: "user", Data: data} }
func c(id, gen, data string) Row { return Row{ChatID: id, Role: "char", GenerationID: gen, Data: data} }

func snap(src Source, rows ...Row) Snapshot {
	for i := range rows {
		rows[i].Index = i
	}
	return Snapshot{SessionKey: "chat-1", Source: src, Rows: rows}
}

func activeTexts(t *Turn) []string {
	out := []string{}
	for _, r := range t.Responses {
		if r.State == Active {
			out = append(out, r.Revisions[len(r.Revisions)-1].Data)
		}
	}
	return out
}

// goOn is the next input after rows (a new user row each time): it
// confirms the turn before it.
func goOn(s *Session, rows ...Row) {
	nextInputs++
	s.Apply(snap(SourceBeforeRequest, append(append([]Row{}, rows...), u(fmt.Sprintf("next%d", nextInputs), "다음"))...))
}

var nextInputs int

func mustTurns(t *testing.T, s *Session, n int) []*Turn {
	t.Helper()
	got := s.ActiveTurns()
	if len(got) != n {
		t.Fatalf("active turns = %d, want %d (events %+v)", len(got), n, s.Events)
	}
	return got
}

// AGENTS case: normal new turn appends.
func TestNormalNewTurnAppends(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "hi"), c("a1", "g1", "hello")))
	s.Apply(snap(SourceOutputEvent, u("u1", "hi"), c("a1", "g1", "hello"), u("u2", "next"), c("a2", "g2", "reply")))
	turns := mustTurns(t, s, 2)
	if turns[0].ID == turns[1].ID || activeTexts(turns[1])[0] != "reply" {
		t.Fatalf("second input must be its own turn: %+v", turns)
	}
}

// AGENTS case: a genuinely new host user row with identical text appends.
func TestSameTextNewRowAppends(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "again"), c("a1", "g1", "one")))
	s.Apply(snap(SourceOutputEvent, u("u1", "again"), c("a1", "g1", "one"), u("u2", "again"), c("a2", "g2", "two")))
	mustTurns(t, s, 2)
}

// AGENTS case: rerolling the same host user row replaces, even when optional
// coordinates (generation id, index) change.
func TestRerollSameRowReplaces(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "hi"), c("a1", "g1", "first")))
	s.Apply(snap(SourceOutputEvent, u("u1", "hi"), c("a9", "g9", "second")))
	turns := mustTurns(t, s, 1)
	if got := activeTexts(turns[0]); len(got) != 1 || got[0] != "second" {
		t.Fatalf("reroll must replace, active = %v", got)
	}
	if turns[0].Responses[0].State != Replaced {
		t.Fatalf("old response must be kept as replaced, got %s", turns[0].Responses[0].State)
	}
}

// AGENTS case: assistant deleted, same user row edited, regenerated -> replace.
func TestEditAfterDeleteRegenerateReplaces(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "hi"), c("a1", "g1", "first")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "hi, edited"))) // assistant deleted + user edited, before regenerate
	s.Apply(snap(SourceOutputEvent, u("u1", "hi, edited"), c("a2", "g2", "regenerated")))
	turns := mustTurns(t, s, 1)
	if got := activeTexts(turns[0]); len(got) != 1 || got[0] != "regenerated" {
		t.Fatalf("regenerate after edit must replace on the same turn, active = %v", got)
	}
	if turns[0].Members[0].Data != "hi, edited" {
		t.Fatalf("member edit not recorded: %q", turns[0].Members[0].Data)
	}
}

// AGENTS case: same-request provider retry (beforeRequest again, no new user
// row, no new response) does not create a turn or confirm anything.
func TestProviderRetryDoesNotAddTurn(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "hi"), c("a1", "g1", "one")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "hi"), c("a1", "g1", "one"), u("u2", "go")))
	before := len(s.ActiveTurns())
	confirmedBefore := s.ActiveTurns()[0].Confirmed
	s.Apply(snap(SourceBeforeRequest, u("u1", "hi"), c("a1", "g1", "one"), u("u2", "go"))) // retry
	if len(s.ActiveTurns()) != before {
		t.Fatalf("retry added a turn")
	}
	if s.ActiveTurns()[0].Confirmed != confirmedBefore {
		t.Fatalf("retry changed confirmation")
	}
}

// Two or three user rows before one reply are one turn, in order.
func TestMultipleInputsJoinOneTurn(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), u("u2", "b"), u("u3", "c"), c("a1", "g1", "reply")))
	turns := mustTurns(t, s, 1)
	if len(turns[0].Members) != 3 || turns[0].Members[0].Data != "a" || turns[0].Members[2].Data != "c" {
		t.Fatalf("members = %+v", turns[0].Members)
	}
}

// Deleting the first member keeps the same turn (identity is Go-issued).
// A member gone from inside a turn is deleted; the turn stays (11-08: the
// chat's very first row gone is a front cut and is kept, below).
func TestFirstMemberDeletedKeepsTurn(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u0", "z"), c("a0", "g0", "r0"), u("u1", "a"), u("u2", "b"), c("a1", "g1", "reply")))
	id := s.ActiveTurns()[1].ID
	s.Apply(snap(SourceOutputEvent, u("u0", "z"), c("a0", "g0", "r0"), u("u2", "b"), c("a1", "g1", "reply")))
	turns := mustTurns(t, s, 2)
	if turns[1].ID != id || turns[1].Members[0].State != Deleted || turns[1].Members[1].State != Active {
		t.Fatalf("turn identity changed or member not marked: %+v", turns[1])
	}
}

// RisuAI's /cut and /del take the front of a chat away to save browser
// memory: rows gone from the front while later rows stay keep their text and
// turns (user 11-08), at a turn boundary or inside a turn, and stay kept as
// the chat goes on. A screen delete after the front (one message, or a
// message and all after it) still deletes; a chat whose every known row is
// gone has no front.
func TestFrontCutKeepsRows(t *testing.T) {
	full := []Row{u("u1", "a"), c("a1", "g1", "r1"), u("u2", "b"), c("a2", "g2", "r2"), u("u3", "c"), c("a3", "g3", "r3")}
	for name, keep := range map[string][]Row{
		"boundary (/del 4)":      {u("u2", "b"), c("a2", "g2", "r2"), u("u3", "c"), c("a3", "g3", "r3")},
		"inside a turn (/del 3)": {c("a2", "g2", "r2"), u("u3", "c"), c("a3", "g3", "r3")},
	} {
		s := NewSession("chat-1")
		s.Apply(snap(SourceOutputEvent, full...))
		ids := []int{s.ActiveTurns()[0].ID, s.ActiveTurns()[1].ID, s.ActiveTurns()[2].ID}
		s.Apply(snap(SourceOutputEvent, keep...))
		goOn(s, keep...)
		turns := mustTurns(t, s, 4)
		for i, want := range []string{"r1", "r2", "r3"} {
			if turns[i].ID != ids[i] || len(activeTexts(turns[i])) != 1 || activeTexts(turns[i])[0] != want || turns[i].Members[0].State != Active {
				t.Fatalf("%s: turn %d lost by the cut: %+v %v", name, ids[i], turns[i], s.Events)
			}
		}
		// after the cut, a screen delete of the last turn still deletes it
		s.Apply(snap(SourceBeforeRequest, keep[:len(keep)-2]...))
		if got := s.ActiveTurns(); len(got) != 2 || got[0].ID != ids[0] || got[1].ID != ids[1] {
			t.Fatalf("%s: tail delete after a cut: %d turns", name, len(got))
		}
	}
	// a row gone after the first row still there is a screen delete
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, full...))
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "r1"), u("u3", "c"), c("a3", "g3", "r3")))
	if got := s.ActiveTurns(); len(got) != 2 || activeTexts(got[1])[0] != "r3" {
		t.Fatalf("middle delete kept: %d turns", len(got))
	}
}

// User-only delete keeps the remaining reply on its own turn (no re-pairing).
func TestUserOnlyDeleteDoesNotRepair(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "r1"), u("u2", "b"), c("a2", "g2", "r2")))
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "r1"), c("a2", "g2", "r2")))
	turns := mustTurns(t, s, 2)
	if activeTexts(turns[0])[0] != "r1" || len(activeTexts(turns[0])) != 1 {
		t.Fatalf("r2 was re-attached to turn 1: %v", activeTexts(turns[0]))
	}
	if activeTexts(turns[1])[0] != "r2" {
		t.Fatalf("turn 2 lost its reply")
	}
}

// Tail delete marks the removed turn inactive and keeps history.
func TestTailDeleteDeactivatesTurn(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "r1"), u("u2", "b"), c("a2", "g2", "r2")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g1", "r1")))
	mustTurns(t, s, 1)
	if !s.Turns[len(s.Turns)-1].Inactive {
		t.Fatalf("deleted tail turn must be inactive")
	}
}

// Choosing a stored reroll candidate or editing changes the body of the same
// row (same generation id): kept as a new revision of the same response.
func TestSameRowBodyChangeIsRevision(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "v1")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g1", "v2")))
	r := mustTurns(t, s, 1)[0].Responses[0]
	if len(r.Revisions) != 2 || r.Revisions[1].Data != "v2" || r.State != Active {
		t.Fatalf("revision not recorded: %+v", r)
	}
}

// Several char rows after one input are parts of the reply, not alternatives.
func TestContinueRowsArePartsNotAlternatives(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "part1"), c("a2", "g2", "part2")))
	if got := activeTexts(mustTurns(t, s, 1)[0]); len(got) != 2 {
		t.Fatalf("parts lost: %v", got)
	}
}

// Greeting rows before the first user row are turn 0, branch markers are not responses,
// and empty char rows are skipped.
func TestGreetingBranchMarkerAndEmptyRows(t *testing.T) {
	s := NewSession("chat-1")
	marker := Row{ChatID: "m1", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::x}}"}
	s.Apply(snap(SourceOutputEvent, c("g0", "", "greeting"), marker, u("u1", "a"), c("e1", "ge", "  "), c("a1", "g1", "r1")))
	turns := mustTurns(t, s, 1)
	if got := activeTexts(turns[0]); len(got) != 1 || got[0] != "r1" {
		t.Fatalf("turn 1 responses = %v", got)
	}
	if !s.Turns[0].Greeting || activeTexts(s.Turns[0])[0] != "greeting" {
		t.Fatalf("greeting turn missing: %+v", s.Turns[0])
	}
}

// Next-input mode confirms the previous pair only when a new user row appears.
func TestNextInputModeConfirmsOnNewInputOnly(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "r1")))
	if s.ActiveTurns()[0].Confirmed {
		t.Fatalf("confirmed before next input")
	}
	s.Apply(snap(SourceLoad, u("u1", "a"), c("a1", "g1", "r1"))) // reload alone
	if s.ActiveTurns()[0].Confirmed {
		t.Fatalf("reload confirmed the pair")
	}
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g1", "r1"), u("u2", "b")))
	if !s.ActiveTurns()[0].Confirmed {
		t.Fatalf("new input did not confirm previous pair")
	}
}

// Rows without chatId still reconcile (index/time/hash fallback) and are flagged.
func TestRowsWithoutChatIDAreFlaggedNotRejected(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, Row{Role: "user", Data: "a"}, Row{Role: "char", Data: "r"}))
	mustTurns(t, s, 1)
	flagged := false
	for _, e := range s.Events {
		if e.Kind == "row_without_chat_id" {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("missing-id rows must be flagged")
	}
}

// The output event alone does not confirm, nor does a retried beforeRequest
// that already shows a streamed reply; the next input does.
func TestOutputAndRetryDoNotConfirm(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g1", "partial")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g1", "partial")))
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "full")))
	if mustTurns(t, s, 1)[0].Confirmed {
		t.Fatalf("output or a retry confirmed the reply")
	}
	goOn(s, u("u1", "a"), c("a1", "g1", "full"))
	if !mustTurns(t, s, 2)[0].Confirmed {
		t.Fatalf("the next input did not confirm")
	}
}

// ---- Astra 15 review counterexamples (ASTRA-STEP2-CORE-15.md F1-F6), kept as regressions ----
func TestAstra15MissingIDEditKeepsLogicalID(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("", "first"), c("a1", "g1", "reply")))
	id := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest, u("", "edited")))
	s.Apply(snap(SourceOutputEvent, u("", "edited"), c("a2", "g2", "new reply")))
	got := mustTurns(t, s, 1)[0]
	if got.ID != id {
		t.Fatalf("same observed row edit changed logical ID: %d -> %d", id, got.ID)
	}
}

func TestAstra15ChangedMemberIDKeepsGroup(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "first"), u("u2", "second"), c("a1", "g1", "reply")))
	id := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest, u("u1-new", "first"), u("u2", "second"), c("a1", "g1", "reply")))
	got := mustTurns(t, s, 1)[0]
	if got.ID != id {
		t.Fatalf("group identity changed: %d -> %d", id, got.ID)
	}
}

func TestAstra15InsertedMemberUsesExistingGroup(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "first"), u("u2", "second"), c("a1", "g1", "reply")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "first"), u("ux", "inserted"), u("u2", "second"), c("a1", "g1", "reply")))
	got := mustTurns(t, s, 1)[0]
	if len(got.Members) != 3 {
		t.Fatalf("missing group member: %+v", got.Members)
	}
}

func TestAstra15UserDeletionThenContinueKeepsResponseOwner(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "one"), c("a1", "g1", "r1"), u("u2", "two"), c("a2", "g2", "r2")))
	owner := s.respTurn["c:a2"]
	s.Apply(snap(SourceOutputEvent, u("u1", "one"), c("a1", "g1", "r1"), c("a2", "g2", "r2"), c("a3", "g3", "continued r2")))
	if s.respTurn["c:a3"] != owner {
		t.Fatalf("continuation moved to turn %d, expected original response owner %d", s.respTurn["c:a3"].ID, owner.ID)
	}
}

func TestAstra15DeletingComponentIsNotReplacement(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "one"), c("a1", "g1", "part1"), c("a2", "g2", "part2")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "one"), c("a1", "g1", "part1")))
	if got := s.respRow["c:a2"].State; got != Deleted {
		t.Fatalf("deleted component is %s, expected deleted; no new response exists", got)
	}
}

func TestAstra15FreshLoadDoesNotFinalize(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceLoad, u("u1", "one"), c("a1", "g1", "r1"), u("u2", "two"), c("a2", "g2", "r2")))
	for _, turn := range s.Turns {
		if turn.Confirmed {
			t.Fatalf("load alone confirmed turn %d", turn.ID)
		}
	}
}

func TestAstra15RerollPartialDoesNotInheritConfirmation(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "one"), c("a1", "g1", "completed")))
	goOn(s, u("u1", "one"), c("a1", "g1", "completed"))
	s.Apply(snap(SourceBeforeRequest, u("u1", "one"), c("a1", "g2", "new partial")))
	if s.ActiveTurns()[0].Confirmed {
		t.Fatalf("new partial generation inherited old completion")
	}
}

func TestAstra15RestoredRowsAreActive(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "one"), c("a1", "g1", "reply")))
	id := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest))
	s.Apply(snap(SourceLoad, u("u1", "one"), c("a1", "g1", "reply")))
	got := mustTurns(t, s, 1)[0]
	if got.ID != id {
		t.Fatalf("restored rows lost original identity")
	}
}

func TestAstra15RerollAndRegeneratePreserveID(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "reroll", true: "edit_regenerate"}[edit], func(t *testing.T) {
			s := NewSession("chat-1")
			s.Apply(snap(SourceOutputEvent, u("u1", "one"), c("a1", "g1", "first")))
			id := s.ActiveTurns()[0].ID
			body := "one"
			if edit {
				body = "edited"
				s.Apply(snap(SourceBeforeRequest, u("u1", body)))
			}
			s.Apply(snap(SourceOutputEvent, u("u1", body), c("a2", "g2", "second")))
			if got := mustTurns(t, s, 1)[0].ID; got != id {
				t.Fatalf("replacement changed ID: %d -> %d", id, got)
			}
		})
	}
}

func TestAstra15MiddleDeletionKeepsLaterTurn(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "one"), c("a1", "g1", "r1"), u("u2", "two"), c("a2", "g2", "r2"), u("u3", "three"), c("a3", "g3", "r3")))
	last := s.respTurn["c:a3"]
	s.Apply(snap(SourceBeforeRequest, u("u1", "one"), c("a1", "g1", "r1"), u("u3", "three"), c("a3", "g3", "r3")))
	mustTurns(t, s, 2)
	if s.respTurn["c:a3"] != last || activeTexts(last)[0] != "r3" {
		t.Fatalf("later turn changed")
	}
}

func TestAstra15PreparationFailureThenInput(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceBeforeRequest, u("u1", "one")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "one"), u("u2", "two")))
	s.Apply(snap(SourceOutputEvent, u("u1", "one"), u("u2", "two"), c("a1", "g1", "reply")))
	if len(mustTurns(t, s, 1)[0].Members) != 2 {
		t.Fatalf("lost input member")
	}
}

// ---- added with the F1-F6 fixes ----

// A row whose text could not be read is present, not deleted or replaced.
func TestUnreadableRowIsPresent(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "orig")))
	goOn(s, u("u1", "a"), c("a1", "g1", "orig"))
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), Row{ChatID: "a1", Role: "char", GenerationID: "g1", Unreadable: true}))
	r := s.respRow["c:a1"]
	if r.State != Active || len(r.Revisions) != 1 || r.Revisions[0].Data != "orig" {
		t.Fatalf("unreadable row changed: %+v", r)
	}
	if !mustTurns(t, s, 1)[0].Confirmed {
		t.Fatalf("an unreadable observation must not undo the completion")
	}
}

// Editing the confirmed body (same generation) keeps the completion and
// records a revision; only a new generation clears it.
func TestSameGenerationEditKeepsConfirmation(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "v1")))
	goOn(s, u("u1", "a"), c("a1", "g1", "v1"))
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g1", "v1 edited")))
	tr := mustTurns(t, s, 1)[0]
	if !tr.Confirmed || len(tr.Responses[0].Revisions) != 2 {
		t.Fatalf("edit of confirmed body: confirmed=%v revisions=%d", tr.Confirmed, len(tr.Responses[0].Revisions))
	}
	if !tr.Responses[0].Revisions[0].Confirmed || tr.Responses[0].Revisions[1].Confirmed {
		t.Fatalf("confirmed flag must stay on the body that was confirmed")
	}
}

// Without chatIds, deleting a middle turn keeps later turns on their own IDs.
func TestNoChatIDMiddleDeleteKeepsLaterIDs(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("", "one"), c("", "", "r1"), u("", "two"), c("", "", "r2"), u("", "three"), c("", "", "r3")))
	ids := []int{s.ActiveTurns()[0].ID, s.ActiveTurns()[1].ID, s.ActiveTurns()[2].ID}
	s.Apply(snap(SourceBeforeRequest, u("", "one"), c("", "", "r1"), u("", "three"), c("", "", "r3")))
	got := mustTurns(t, s, 2)
	if got[0].ID != ids[0] || got[1].ID != ids[2] || activeTexts(got[1])[0] != "r3" {
		t.Fatalf("ids %d %d, want %d %d", got[0].ID, got[1].ID, ids[0], ids[2])
	}
}

// A genuinely new user row (new chatId) after the old turn was deleted is a
// new turn, not a continuation of the deleted one.
func TestNewRowAfterDeletionIsNewTurn(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "same"), c("a1", "g1", "r1")))
	old := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest, u("u9", "same")))
	if got := mustTurns(t, s, 1)[0].ID; got == old {
		t.Fatalf("new row took the deleted turn's ID %d", old)
	}
}

// A response first seen while unreadable is linked; its readable text later
// becomes the first revision of the same row, not a second response.
func TestNewUnreadableRowLinkedThenRevised(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), Row{ChatID: "a1", Role: "char", GenerationID: "g1", Unreadable: true}))
	if n := activeResponses(mustTurns(t, s, 1)[0]); n != 1 {
		t.Fatalf("unreadable reply row not linked: %d active responses", n)
	}
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "orig")))
	tr := mustTurns(t, s, 1)[0]
	if len(tr.Responses) != 1 || len(tr.Responses[0].Revisions) != 1 || tr.Responses[0].Revisions[0].Data != "orig" {
		t.Fatalf("responses %+v", tr.Responses)
	}
}

// The same body under a new generation id is a new generation: recorded,
// and not covered by the earlier completion.
func TestSameBodyNewGenerationIsRecorded(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "same")))
	goOn(s, u("u1", "a"), c("a1", "g1", "same"))
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g2", "same")))
	tr := mustTurns(t, s, 1)[0]
	if len(tr.Responses[0].Revisions) != 2 || tr.Responses[0].Revisions[1].GenerationID != "g2" || tr.Confirmed {
		t.Fatalf("revisions %+v confirmed %v", tr.Responses[0].Revisions, tr.Confirmed)
	}
}

// A restored session continues IDs and links: the same rows replace, new
// rows get new IDs, and nothing is confirmed by the restore itself.
func TestRestoreContinuesIDsAndLinks(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "r1"), u("u2", "b"), c("a2", "g2", "r2")))
	ids := []int{s.ActiveTurns()[0].ID, s.ActiveTurns()[1].ID}
	r := Restore("chat-1", s.Turns, nil)
	r.Apply(snap(SourceLoad, u("u1", "a"), c("a1", "g1", "r1"), u("u2", "b"), c("a2", "g2", "r2")))
	got := mustTurns(t, r, 2)
	if got[0].ID != ids[0] || got[1].ID != ids[1] || got[1].Confirmed {
		t.Fatalf("after restore: ids %d %d confirmed %v", got[0].ID, got[1].ID, got[1].Confirmed)
	}
	r.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g1", "r1"), u("u2", "b"), c("a2", "g2", "r2"), u("u3", "c")))
	got = mustTurns(t, r, 3)
	if got[2].ID <= ids[1] || !got[1].Confirmed {
		t.Fatalf("new turn id %d (prev %d), prev confirmed %v", got[2].ID, ids[1], got[1].Confirmed)
	}
	if len(r.Turns[1].Responses[0].Revisions) != 1 {
		t.Fatalf("restore re-added revisions")
	}
}

// RisuAI reroll removes the old reply before the request: the old row is
// seen deleted first, then the new reply replaces it on the same turn.
func TestRerollDeleteFirstThenNewReplyReplaces(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "r1")))
	id := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest, u("u1", "a")))
	if s.respRow["c:a1"].State != Deleted {
		t.Fatalf("old reply before the new one arrives: %s", s.respRow["c:a1"].State)
	}
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a2", "g2", "r2")))
	got := mustTurns(t, s, 1)[0]
	if got.ID != id || s.respRow["c:a1"].State != Replaced || activeTexts(got)[0] != "r2" {
		t.Fatalf("id %d->%d, old %s, active %v", id, got.ID, s.respRow["c:a1"].State, activeTexts(got))
	}
}

// A new part added while another part is still active does not turn an
// earlier deleted part into a replacement.
func TestNewPartBesideActiveKeepsDeletedPart(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "p1"), c("a2", "g2", "p2")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "a"), c("a1", "g1", "p1")))
	s.Apply(snap(SourceOutputEvent, u("u1", "a"), c("a1", "g1", "p1"), c("a3", "g3", "p3")))
	if s.respRow["c:a2"].State != Deleted {
		t.Fatalf("deleted part became %s", s.respRow["c:a2"].State)
	}
}

// ---- Astra 16 recheck counterexamples (ASTRA-RECHECK-16.md R1, R2, N1-N3), kept as regressions ----
func TestAstra16NoIDEditWithNewIdentifiedResponse(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("", "before"), c("a1", "g1", "reply")))
	id := s.ActiveTurns()[0].ID
	// No edit callback: the next observation can already contain regeneration.
	s.Apply(snap(SourceOutputEvent, u("", "edited"), c("a2", "g2", "regenerated")))
	if got := mustTurns(t, s, 1)[0].ID; got != id {
		t.Fatalf("same user edited/regenerated: logical ID %d -> %d", id, got)
	}
}

func TestAstra16UnreadableMemberPreservesKnownText(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "original input"), c("a1", "g1", "reply")))
	s.Apply(snap(SourceBeforeRequest, Row{ChatID: "u1", Role: "user", Unreadable: true}, c("a1", "g1", "reply")))
	m := mustTurns(t, s, 1)[0].Members[0]
	if m.Data != "original input" {
		t.Fatalf("unreadable user overwrote known body with %q", m.Data)
	}
}

func TestAstra16UnreadableNoIDResponsePreservesRow(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), c("", "g1", "original")))
	goOn(s, u("u1", "input"), c("", "g1", "original"))
	tr := s.ActiveTurns()[0]
	old := tr.Responses[0]
	s.Apply(snap(SourceBeforeRequest, u("u1", "input"), Row{Role: "char", GenerationID: "g1", Unreadable: true}))
	if old.State != Active || len(tr.Responses) != 1 || !tr.Confirmed {
		t.Fatalf("unreadable response: old=%s rows=%d confirmed=%v", old.State, len(tr.Responses), tr.Confirmed)
	}
}

func TestAstra16UnreadableNewGenerationClearsCompletion(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), c("a1", "g1", "complete")))
	goOn(s, u("u1", "input"), c("a1", "g1", "complete"))
	s.Apply(snap(SourceBeforeRequest, u("u1", "input"), Row{ChatID: "a1", Role: "char", GenerationID: "g2", Unreadable: true}))
	if s.ActiveTurns()[0].Confirmed {
		t.Fatal("known new generation g2 inherited g1 completion because text is unreadable")
	}
}

func TestAstra16LateOriginalRetainsObservedOutputCompletion(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), Row{ChatID: "a1", Role: "char", GenerationID: "g1", Unreadable: true}))
	goOn(s, u("u1", "input"), Row{ChatID: "a1", Role: "char", GenerationID: "g1", Unreadable: true})
	if !s.ActiveTurns()[0].Confirmed {
		t.Fatal("setup next input did not confirm")
	}
	s.Apply(snap(SourceOutputReread, u("u1", "input"), c("a1", "g1", "original now available")))
	if !s.ActiveTurns()[0].Confirmed {
		t.Fatal("same generation source arrival cleared observed output completion")
	}
}

func TestAstra16MissingGenerationIsNotNewGeneration(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), c("a1", "g1", "complete")))
	goOn(s, u("u1", "input"), c("a1", "g1", "complete"))
	s.Apply(snap(SourceBeforeRequest, u("u1", "input"), c("a1", "", "complete")))
	if !s.ActiveTurns()[0].Confirmed {
		t.Fatal("missing optional generation ID interpreted as a new generation")
	}
}

func TestAstra16NoIDEditIsNotNextInput(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceLoad, u("", "old input"), c("a1", "g1", "reply1"), u("u2", "two"), c("a2", "g2", "reply2")))
	s.Apply(snap(SourceBeforeRequest, u("", "edited input"), c("a1", "g1", "reply1"), u("u2", "two"), c("a2", "g2", "reply2")))
	if s.ActiveTurns()[0].Confirmed {
		t.Fatal("editing existing no-ID member confirmed a turn as if new input arrived")
	}
}

func TestAstra16EmptyEditedResponseDoesNotKeepOldBody(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), c("a1", "g1", "old reply")))
	s.Apply(snap(SourceBeforeRequest, u("u1", "input"), c("a1", "g1", "")))
	r := s.respRow["c:a1"]
	if r.State == Active && r.Revisions[len(r.Revisions)-1].Data == "old reply" {
		t.Fatal("readable empty current row retains previous non-empty active body")
	}
}

func TestAstra16SameGenerationCandidateControl(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), c("a1", "g1", "candidate1")))
	goOn(s, u("u1", "input"), c("a1", "g1", "candidate1"))
	s.Apply(snap(SourceBeforeRequest, u("u1", "input"), c("a1", "g1", "candidate2")))
	tr := mustTurns(t, s, 1)[0]
	if !tr.Confirmed || activeTexts(tr)[0] != "candidate2" {
		t.Fatal("same generation candidate edit not preserved")
	}
}

// A chat first observed without chatIds (load) gets them at the first send
// (RisuAI index.svelte.ts assigns chatId while building the request): the
// same rows keep their turns instead of becoming new turns.
func TestRowsGainingChatIDKeepTurns(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceLoad, Row{Role: "user", Time: 5, Data: "one"}, Row{Role: "char", Data: "r1"}))
	id := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest, Row{ChatID: "u1", Role: "user", Time: 5, Data: "one"}, c("a1", "", "r1"), u("u2", "two")))

	turns := mustTurns(t, s, 2)
	if turns[0].ID != id || len(turns[0].Members) != 1 || len(turns[0].Responses) != 1 || turns[0].Members[0].Key != "c:u1" {
		t.Fatalf("turn after chatId assignment: %+v", turns[0])
	}
	if len(s.TakeRenames()) != 2 {
		t.Fatalf("renames not recorded")
	}
}

// Observation notes are reported once per session, not on every snapshot.
func TestRepeatedSnapshotsDoNotRepeatNotes(t *testing.T) {
	s := NewSession("chat-1")
	marker := Row{ChatID: "m1", Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::P::n::x1::}}"}
	for i := 0; i < 3; i++ {
		s.Apply(snap(SourceBeforeRequest, marker, Row{Role: "user", Data: "a"}))
	}
	n := map[string]int{}
	for _, e := range s.TakeEvents() {
		n[e.Kind]++
	}
	if n["row_without_chat_id"] != 1 || n["branch_marker"] != 1 || n["branch_of"] != 1 || s.BranchOf != [2]string{"chat:P", "c:x1"} {
		t.Fatalf("events %v branch %v", n, s.BranchOf)
	}
}

// ---- Astra 17 recheck counterexamples (ASTRA-RECHECK-17.md), kept as regressions ----
func TestAstra17LoadEditThenAssignIDKeepsTurn(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceLoad, Row{Role: "user", Time: 5, Data: "before"}, c("a1", "g1", "reply")))
	id := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest, Row{Role: "user", ChatID: "u1", Time: 5, Data: "edited"}))
	s.Apply(snap(SourceOutputEvent, Row{Role: "user", ChatID: "u1", Time: 5, Data: "edited"}, c("a2", "g2", "new reply")))
	if got := mustTurns(t, s, 1)[0].ID; got != id {
		t.Fatalf("same loaded user edit+regenerate: turn %d -> %d", id, got)
	}
}
func TestAstra17DeletedFallbackCannotOwnNewID(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceLoad, u("", "same"), c("a1", "g1", "old reply")))
	old := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceLoad))
	s.Apply(snap(SourceBeforeRequest, u("new-user", "same")))
	if got := mustTurns(t, s, 1)[0].ID; got == old {
		t.Fatalf("new identified row resurrected deleted no-ID turn %d", old)
	}
}
func TestAstra17NoIDUnreadableUserKeepsMember(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceLoad, u("", "known input"), c("a1", "g1", "reply")))
	tr := s.ActiveTurns()[0]
	old := tr.Members[0]
	s.Apply(snap(SourceBeforeRequest, Row{Role: "user", Unreadable: true}, c("a1", "g1", "reply")))
	if old.State != Active || len(tr.Members) != 1 || tr.Members[0].Data != "known input" {
		t.Fatalf("unreadable existing member: old=%s members=%d current=%q", old.State, len(tr.Members), tr.Members[len(tr.Members)-1].Data)
	}
}
func TestAstra17NoIDLateOriginalKeepsCompletion(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), Row{Role: "char", GenerationID: "g1", Unreadable: true}))
	goOn(s, u("u1", "input"), Row{Role: "char", GenerationID: "g1", Unreadable: true})
	tr := s.ActiveTurns()[0]
	s.Apply(snap(SourceOutputReread, u("u1", "input"), c("", "g1", "original")))
	if len(tr.Responses) != 1 || !tr.Confirmed {
		t.Fatalf("same no-ID generation original arrival: rows=%d confirmed=%v", len(tr.Responses), tr.Confirmed)
	}
}
func TestAstra17HistoricalPartDeletionNotReroll(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), c("p1", "g1", "part one"), c("p2", "g2", "part two")))
	s.Apply(snap(SourceLoad, u("u1", "input"), c("p2", "g2", "part two")))
	if s.respRow["c:p1"].State != Deleted {
		t.Fatal("setup: deleted first part")
	}
	s.Apply(snap(SourceBeforeRequest, u("u1", "input")))
	s.Apply(snap(SourceOutputEvent, u("u1", "input"), c("p3", "g3", "rerolled second part")))
	if s.respRow["c:p1"].State != Deleted {
		t.Fatalf("independently deleted part rewritten to %s", s.respRow["c:p1"].State)
	}
}
func TestAstra17MiddleUserDeletionAndRerollOwner(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("u1", "one"), c("a1", "g1", "one reply"), u("u2", "two"), c("a2", "g2", "two reply"), u("u3", "three"), c("a3", "g3", "three reply")))
	owner := s.respTurn["c:a2"].ID
	s.Apply(snap(SourceLoad, u("u1", "one"), c("a1", "g1", "one reply"), c("a2", "g2", "two reply"), u("u3", "three"), c("a3", "g3", "three reply")))
	if s.respTurn["c:a2"].ID != owner || s.respRow["c:a2"].State != Active {
		t.Fatal("orphan response moved")
	}
	s.Apply(snap(SourceLoad, u("u1", "one"), c("a1", "g1", "one reply"), u("u3", "three"), c("a3", "g3", "three reply")))
	if s.respRow["c:a2"].State != Deleted || s.respTurn["c:a3"].ID == owner {
		t.Fatal("middle deletion altered later turn")
	}
}

// ---- Astra 18 recheck counterexamples (ASTRA-RECHECK-18.md), kept as regressions ----
func a18snap(rows ...Row) Snapshot {
	for i := range rows {
		rows[i].Index = i
	}
	return Snapshot{SessionKey: "A", Source: SourceBeforeRequest, Rows: rows, TargetIndex: -1}
}
func TestAstra18PartialRestoreContinuation(t *testing.T) {
	s := NewSession("A")
	u := Row{Role: "user", ChatID: "u", Data: "q"}
	a := Row{Role: "char", ChatID: "a", Data: "A"}
	b := Row{Role: "char", ChatID: "b", Data: "B"}
	c := Row{Role: "char", ChatID: "c", Data: "C"}
	s.Apply(a18snap(u, a, b))
	s.Apply(a18snap(u))
	s.Apply(a18snap(u, a))
	s.Apply(a18snap(u, a, c))
	if s.respRow["c:b"].State != Deleted {
		t.Fatalf("absent part B should remain Deleted during continuation: %+v", s.respRow["c:b"])
	}
	if s.respRow["c:b"].AwaitingReplacement {
		t.Fatal("stale pending flag survives continuation")
	}
}
func TestAstra18NoIDLateOriginalGainsID(t *testing.T) {
	s := NewSession("A")
	u := Row{Role: "user", ChatID: "u", Data: "q"}
	unknown := Row{Role: "char", Unreadable: true, GenerationID: "g1"}
	x := a18snap(u, unknown)
	x.Source = SourceOutputEvent
	x.TargetIndex = 1
	s.Apply(x)
	s.Apply(a18snap(u, unknown, Row{Role: "user", ChatID: "next", Data: "다음"})) // the next input
	x = a18snap(u, Row{Role: "char", ChatID: "a", Data: "original", GenerationID: "g1"})
	x.Source = SourceOutputReread
	s.Apply(x)
	if !s.Turns[0].Confirmed || len(s.Turns[0].Responses) != 1 {
		t.Fatalf("late original plus first ID lost row/completion: confirmed=%v rows=%d", s.Turns[0].Confirmed, len(s.Turns[0].Responses))
	}
}

// An edit of the chat's first row without chatId changes its fallback key:
// the old row is gone from the front, but its turn got the edited row in
// the same snapshot, so it is an edit, not a front cut — the old text does
// not stay beside the new one.
func TestFirstRowEditIsNotAFrontCut(t *testing.T) {
	s := NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, u("", "one"), c("", "", "r1"), u("", "two"), c("", "", "r2")))
	ids := []int{s.ActiveTurns()[0].ID, s.ActiveTurns()[1].ID}
	s.Apply(snap(SourceBeforeRequest, u("", "ONE edited"), c("", "", "r1"), u("", "two"), c("", "", "r2")))
	got := mustTurns(t, s, 2)
	if got[0].ID != ids[0] || got[1].ID != ids[1] {
		t.Fatalf("ids %d %d, want %v", got[0].ID, got[1].ID, ids)
	}
	var active []string
	for _, m := range got[0].Members {
		if m.State == Active {
			active = append(active, m.Data)
		}
	}
	if len(active) != 1 || active[0] != "ONE edited" {
		t.Fatalf("first turn's active rows %q", active)
	}
}

// After a front cut (review 21): an edit of a remaining row without chatId
// is continued by its own turn, not by the cut one; a new row with the text
// of a cut row without chatId is a new turn; a cut row stays cut when every
// remaining row changes later (it is no longer on the screen to delete).
func TestFrontCutLaterSnapshots(t *testing.T) {
	s := NewSession("chat-1")
	u2 := Row{Role: "user", Time: 20, Data: "second input"}
	a2 := c("a2", "g2", "second answer")
	s.Apply(snap(SourceOutputEvent, u("u1", "first input"), c("a1", "g1", "first answer"), u2, a2))
	ids := []int{s.ActiveTurns()[0].ID, s.ActiveTurns()[1].ID}
	s.Apply(snap(SourceBeforeRequest, u2, a2)) // /del 2
	u2.Data = "edited second input"
	s.Apply(snap(SourceBeforeRequest, u2))
	s.Apply(snap(SourceOutputEvent, u2, c("new-answer", "gnew", "regenerated")))
	got := mustTurns(t, s, 2)
	if got[0].ID != ids[0] || activeTexts(got[0])[0] != "first answer" || got[1].ID != ids[1] || activeTexts(got[1])[0] != "regenerated" {
		t.Fatalf("edit after a cut: %+v %+v %v", got[0], got[1], s.Events)
	}

	s = NewSession("chat-1")
	s.Apply(snap(SourceOutputEvent, Row{Role: "user", Data: "repeat"}, c("a1", "g1", "first answer"), u("u2", "second"), c("a2", "g2", "second answer")))
	cut := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest, u("u2", "second"), c("a2", "g2", "second answer")))
	s.Apply(snap(SourceOutputEvent, u("u2", "second"), c("a2", "g2", "second answer"), u("new", "repeat"), c("new-a", "gn", "new answer")))
	got = mustTurns(t, s, 3)
	if got[0].ID != cut || activeTexts(got[0])[0] != "first answer" || activeTexts(got[2])[0] != "new answer" {
		t.Fatalf("same text after a cut: %v", s.Events)
	}
}

// The first turn's reply deleted and its input (without chatId) edited,
// then regenerated, with a later turn still there (review 21/22 A-4): the
// edited input stays on the first turn and replaces its old input; the
// later turn keeps its own rows.
func TestEditWithoutChatIDBeforeALaterTurn(t *testing.T) {
	s := NewSession("chat-1")
	oldUser := Row{Role: "user", Time: 10, Data: "old input"}
	later := []Row{u("u2", "later input"), c("a2", "g2", "later answer")}
	s.Apply(snap(SourceOutputEvent, append([]Row{oldUser, c("a1", "g1", "old answer")}, later...)...))
	ids := []int{s.ActiveTurns()[0].ID, s.ActiveTurns()[1].ID}
	edit := oldUser
	edit.Data = "edited input"
	s.Apply(snap(SourceBeforeRequest, append([]Row{edit}, later...)...))
	s.Apply(snap(SourceOutputEvent, append([]Row{edit, c("a-new", "gnew", "regenerated")}, later...)...))
	got := mustTurns(t, s, 2)
	var first []string
	for _, m := range got[0].Members {
		if m.State == Active {
			first = append(first, m.Data)
		}
	}
	if got[0].ID != ids[0] || len(first) != 1 || first[0] != "edited input" || activeTexts(got[0])[0] != "regenerated" {
		t.Fatalf("first turn: %v %v %v", first, activeTexts(got[0]), s.Events)
	}
	if got[1].ID != ids[1] || len(got[1].Members) != 1 || activeTexts(got[1])[0] != "later answer" {
		t.Fatalf("later turn took the edit: %+v", got[1].Members)
	}
}

// Review 23: two edited inputs of one turn move to that turn together, never
// further back into a turn cut away in the same snapshot; an edit that got
// a chatId on the way is still an edit of its own turn.
func TestEditOfSeveralInputsAndAssignedID(t *testing.T) {
	s := NewSession("chat-1")
	a := Row{Role: "user", Time: 20, Data: "input A"}
	b := Row{Role: "user", Time: 21, Data: "input B"}
	s.Apply(snap(SourceOutputEvent, u("u1", "prefix"), c("a1", "g1", "prefix answer"), a, b, c("a2", "g2", "answer 2"), u("u3", "third"), c("a3", "g3", "answer 3")))
	ids := []int{s.ActiveTurns()[0].ID, s.ActiveTurns()[1].ID, s.ActiveTurns()[2].ID}
	a.Data, b.Data = "edited A", "edited B"
	s.Apply(snap(SourceBeforeRequest, a, b, u("u3", "third"), c("a3", "g3", "answer 3"))) // turn 1 cut, turn 2's answer deleted, A/B edited
	got := mustTurns(t, s, 3)
	if got[0].ID != ids[0] || activeTexts(got[0])[0] != "prefix answer" || got[0].Members[0].State != Active {
		t.Fatalf("the cut turn lost its rows: %+v %v", got[0], s.Events)
	}
	var second []string
	for _, m := range got[1].Members {
		if m.State == Active {
			second = append(second, m.Data)
		}
	}
	if got[1].ID != ids[1] || len(second) != 2 || second[0] != "edited A" || second[1] != "edited B" {
		t.Fatalf("edited inputs: %v", second)
	}

	s = NewSession("chat-1")
	old := Row{Role: "user", Time: 10, Data: "old input"}
	s.Apply(snap(SourceOutputEvent, old, c("a1", "g1", "old answer"), u("u2", "later"), c("a2", "g2", "later answer")))
	first := s.ActiveTurns()[0].ID
	s.Apply(snap(SourceBeforeRequest, u("given", "edited input"), u("u2", "later"), c("a2", "g2", "later answer")))
	s.Apply(snap(SourceOutputEvent, u("given", "edited input"), c("a-new", "gn", "regenerated"), u("u2", "later"), c("a2", "g2", "later answer")))
	got = mustTurns(t, s, 2)
	if got[0].ID != first || activeTexts(got[0])[0] != "regenerated" || len(got[1].Members) != 1 {
		t.Fatalf("edit with a new chatId: %v", s.Events)
	}
}
