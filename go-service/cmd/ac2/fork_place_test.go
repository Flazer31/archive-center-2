package main

import (
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
)

// A branch made at a reply keeps its place after the parent rerolls that
// reply: the replaced reply's text is gone, its row stays a marker of
// its turn.
func TestForkPlaceAfterBranchReplyRerolled(t *testing.T) {
	row := func(i int, id, role, gen, data string) lifecycle.Row {
		return lifecycle.Row{Index: i, ChatID: id, Role: role, GenerationID: gen, Data: data}
	}
	s := lifecycle.NewSession("chat:P")
	s.Apply(lifecycle.Snapshot{SessionKey: "chat:P", Source: lifecycle.SourceOutputEvent, TargetIndex: -1,
		Rows: []lifecycle.Row{row(0, "u1", "user", "", "하나"), row(1, "a1", "char", "g1", "답 하나"), row(2, "u2", "user", "", "둘"), row(3, "a2", "char", "g2", "답 둘")}})
	if got := forkPlace(s, "c:a2"); got != 2 {
		t.Fatalf("fixture: branch place %d", got)
	}
	s.Apply(lifecycle.Snapshot{SessionKey: "chat:P", Source: lifecycle.SourceOutputEvent, TargetIndex: -1,
		Rows: []lifecycle.Row{row(0, "u1", "user", "", "하나"), row(1, "a1", "char", "g1", "답 하나"), row(2, "u2", "user", "", "둘"), row(3, "a3", "char", "g3", "다시 쓴 답 둘")}})
	if got := forkPlace(s, "c:a2"); got != 2 {
		t.Fatalf("after the parent rerolled the branch reply, the branch is placed at %d, want 2", got)
	}
}
