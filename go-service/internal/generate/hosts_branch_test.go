package generate

import (
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// Branches as the hosts make them (PocketRisu Chat.svelte:1072-1088 with
// reissueMessageIds; RisuAI Chat.svelte:963-977 keeps the ids): the copied
// messages up to the branch point, then the marker. A branch of a branch
// also holds the copied marker of the earlier branch.

func marker(parent, msg, id string) lifecycle.Row {
	return lifecycle.Row{ChatID: id, Role: "char", IsComment: true, Disabled: "true", Data: "{{specialcomment::branchedfrom::" + parent + "::이름::" + msg + "::}}"}
}

func savedRun(t *testing.T, w *Worker, s *lifecycle.Session) {
	t.Helper()
	run(w, s)
}

// The branch shares the parent's past (A: T1 T2, branched at a2) with new
// message ids: the copies are the parent's memory (once each in the
// branch's current memory), and only the branch's new turn is made.
func TestBranchWithNewIDsReadsTheParentsPast(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	a := lifecycle.NewSession("chat:A")
	a.Apply(a28Snap(a.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째")))
	a.Apply(a28Snap(a.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"), row("u2", "user", "", "둘")))
	a.Apply(a28Snap(a.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "부모 둘째")))
	goOn(a, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "부모 둘째"))
	savedRun(t, w, a)
	if m.calls() != 2 {
		t.Fatalf("parent calls %d", m.calls())
	}
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceOutputEvent,
		row("x1", "user", "", "하나"), row("y1", "char", "g1", "부모 첫째"), row("x2", "user", "", "둘"), row("y2", "char", "g2", "부모 둘째"),
		marker("A", "a2", "mk"), row("x3", "user", "", "셋"), row("y3", "char", "g3", "분기 셋째")))
	goOn(c, row("x1", "user", "", "하나"), row("y1", "char", "g1", "부모 첫째"), row("x2", "user", "", "둘"), row("y2", "char", "g2", "부모 둘째"),
		marker("A", "a2", "mk"), row("x3", "user", "", "셋"), row("y3", "char", "g3", "분기 셋째"))
	if c.BranchOf != [2]string{"chat:A", "c:a2"} {
		t.Fatalf("branch of %v", c.BranchOf)
	}
	savedRun(t, w, c)
	if m.calls() != 3 {
		t.Fatalf("branch made %d turns, want only its own", m.calls()-2)
	}
	got := a28Bodies(t, db, c)
	if strings.Count(got, "부모 첫째 기록") != 1 || strings.Count(got, "부모 둘째 기록") != 1 || !strings.Contains(got, "분기 셋째 기록") {
		t.Fatalf("branch memory %q", got)
	}
}

// With new ids, matching goes in order and stops at the first copy that
// differs: an edited first turn is the branch's own, and the order is the
// branch's (the edited turn first).
func TestBranchWithNewIDsStopsAtAnEditedCopy(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	a := lifecycle.NewSession("chat:A")
	a.Apply(a28Snap(a.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째")))
	goOn(a, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"))
	savedRun(t, w, a)
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceLoad, row("x1", "user", "", "하나"), row("y1", "char", "g9", "분기에서 고친 첫째"), marker("A", "a1", "mk")))
	if err := db.SaveSession(c, nil, nil); err != nil {
		t.Fatal(err)
	}
	recordBranch(db, c)
	scope, err := memory.ScopeOf(c), error(nil)
	if err != nil || len(scope) != 1 || scope[0].Inherited {
		t.Fatalf("scope %+v %v", scope, err)
	}
	if strings.Contains(a28Bodies(t, db, c), "부모 첫째") {
		t.Fatal("the edited copy read the parent's memory")
	}
}

// A branch of a branch holds the copied earlier marker before its own: the
// parent is the direct one (the last marker), and with new ids at each
// branch the grandchild still reads the first chat's memory.
func TestBranchOfABranchWithNewIDs(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	a := lifecycle.NewSession("chat:A")
	a.Apply(a28Snap(a.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "공유 과거")))
	goOn(a, row("u1", "user", "", "하나"), row("a1", "char", "g1", "공유 과거"))
	savedRun(t, w, a)
	b := lifecycle.NewSession("chat:B")
	b.Apply(a28Snap(b.Key, lifecycle.SourceOutputEvent, row("x1", "user", "", "하나"), row("y1", "char", "g1", "공유 과거"), marker("A", "a1", "mkA"),
		row("x2", "user", "", "둘"), row("y2", "char", "g2", "B 이후")))
	goOn(b, row("x1", "user", "", "하나"), row("y1", "char", "g1", "공유 과거"), marker("A", "a1", "mkA"),
		row("x2", "user", "", "둘"), row("y2", "char", "g2", "B 이후"))
	savedRun(t, w, b)
	g := lifecycle.NewSession("chat:G")
	g.Apply(a28Snap(g.Key, lifecycle.SourceLoad, row("z1", "user", "", "하나"), row("w1", "char", "g1", "공유 과거"), marker("A", "a1", "mkA2"),
		row("z2", "user", "", "둘"), row("w2", "char", "g2", "B 이후"), marker("B", "y2", "mkB")))
	if g.BranchOf != [2]string{"chat:B", "c:y2"} {
		t.Fatalf("branch of %v (want the direct parent B)", g.BranchOf)
	}
	recordBranch(db, g)
	if err := db.SaveSession(g, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := a28Bodies(t, db, g)
	if !strings.Contains(got, "공유 과거 기록") || !strings.Contains(got, "B 이후 기록") || m.calls() != 2 {
		t.Fatalf("grandchild memory %q calls %d", got, m.calls())
	}
}

// RisuAI keeps the ids: a branch that edited its first copy keeps its own
// order (its edited turn, then the inherited second turn).
func TestBranchKeepingIDsKeepsTheBranchsOrder(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	a := lifecycle.NewSession("chat:A")
	a.Apply(a28Snap(a.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째")))
	a.Apply(a28Snap(a.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"), row("u2", "user", "", "둘")))
	a.Apply(a28Snap(a.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "부모 둘째")))
	goOn(a, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 첫째"), row("u2", "user", "", "둘"), row("a2", "char", "g2", "부모 둘째"))
	savedRun(t, w, a)
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g9", "분기에서 고친 첫째"),
		row("u2", "user", "", "둘"), row("a2", "char", "g2", "부모 둘째"), marker("A", "a2", "mk")))
	recordBranch(db, c)
	scope, err := memory.ScopeOf(c), error(nil)
	if err != nil || len(scope) != 2 || scope[0].Inherited || !scope[1].Inherited || scope[0].Source.Turn != 1 {
		t.Fatalf("scope %+v %v", scope, err)
	}
}

// The markers already seen are stored: after a restart, a snapshot that
// shows only the ancestor's marker (the direct parent's taken off for a
// PocketRisu reroll) keeps the direct parent.
func TestSeenMarkersSurviveARestart(t *testing.T) {
	db := testDB(t)
	g := lifecycle.NewSession("chat:G")
	rows := []lifecycle.Row{row("z1", "user", "", "하나"), row("w1", "char", "g1", "공유 과거"), marker("A", "a1", "mkA"), row("z2", "user", "", "둘"), row("w2", "char", "g2", "B 이후")}
	g.Apply(a28Snap(g.Key, lifecycle.SourceLoad, append(append([]lifecycle.Row{}, rows...), marker("B", "y2", "mkB"))...))
	if err := db.SaveSession(g, nil, nil); err != nil {
		t.Fatal(err)
	}
	re, _, err := db.LoadSession(g.Key)
	if err != nil {
		t.Fatal(err)
	}
	re.Apply(a28Snap(g.Key, lifecycle.SourceLoad, rows...))
	if re.BranchOf != [2]string{"chat:B", "c:y2"} {
		t.Fatalf("branch of %v after a restart (want the direct parent B)", re.BranchOf)
	}
}

// A branch from the parent's last reply, which the parent has not made yet
// (no next input there): the branch's first input confirms its copy, and
// the branch makes it as its own memory (as AC1, a branch never makes the
// parent's; ASTRA-RECHECK-53 2 gap). The parent makes its own when it goes
// on. Both id shapes.
func TestBranchFromTheParentsPendingReply(t *testing.T) {
	for _, ids := range []string{"kept", "new"} {
		t.Run(ids, func(t *testing.T) {
			db := testDB(t)
			m := &fakeModel{}
			w := NewWorker(db, m, "fake", time.Minute)
			p := lifecycle.NewSession("chat:P")
			p.Apply(a28Snap(p.Key, lifecycle.SourceBeforeRequest, row("u1", "user", "", "하나")))
			p.Apply(a28Snap(p.Key, lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 마지막 답")))
			savedRun(t, w, p)
			if m.calls() != 0 {
				t.Fatalf("parent made its last reply without a next input")
			}
			cu, ca := "u1", "a1"
			if ids == "new" {
				cu, ca = "x1", "y1"
			}
			c := lifecycle.NewSession("chat:C")
			// the branch's first observation: the copies, the marker and the first input
			c.Apply(a28Snap(c.Key, lifecycle.SourceBeforeRequest, row(cu, "user", "", "하나"), row(ca, "char", "g1", "부모 마지막 답"), marker("P", "a1", "mk"), row("x2", "user", "", "분기 첫 입력")))
			ok, err := memory.RecordCopies(c, func(k string) (*lifecycle.Session, bool, error) { return db.LoadSession(k) })
			if err != nil || !ok || len(c.Copies) != 1 {
				t.Fatalf("record %v %v %v", c.Copies, ok, err)
			}
			savedRun(t, w, c)
			if got := a28Bodies(t, db, c); m.calls() != 1 || !strings.Contains(got, "부모 마지막 답 기록") {
				t.Fatalf("branch: calls %d memory %q", m.calls(), got)
			}
			if j, _ := db.JobFor(memory.SourceOf(p.Key, p.ActiveTurns()[0])); j.State == "done" {
				t.Fatal("the branch made the parent's memory")
			}
			goOn(p, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 마지막 답"))
			savedRun(t, w, p)
			if got := a28Bodies(t, db, p); m.calls() != 2 || !strings.Contains(got, "부모 마지막 답 기록") {
				t.Fatalf("parent went on: calls %d memory %q", m.calls(), got)
			}
			// the branch now reads the parent's memory of the copy
			if got := a28Bodies(t, db, c); strings.Count(got, "부모 마지막 답 기록") != 1 {
				t.Fatalf("branch after the parent made it: %q", got)
			}
		})
	}
}

// Copies recorded before a restart are read back as they were.
func TestRecordedCopiesSurviveARestart(t *testing.T) {
	db := testDB(t)
	p := lifecycle.NewSession("chat:P")
	p.Apply(a28Snap(p.Key, lifecycle.SourceLoad, row("u1", "user", "", "하나"), row("a1", "char", "g1", "부모 답")))
	if err := db.SaveSession(p, nil, nil); err != nil {
		t.Fatal(err)
	}
	c := lifecycle.NewSession("chat:C")
	c.Apply(a28Snap(c.Key, lifecycle.SourceLoad, row("x1", "user", "", "하나"), row("y1", "char", "g1", "부모 답"), marker("P", "a1", "mk")))
	ok, err := memory.RecordCopies(c, func(k string) (*lifecycle.Session, bool, error) { return db.LoadSession(k) })
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := db.SaveSession(c, c.TakeEvents(), nil); err != nil {
		t.Fatal(err)
	}
	re, _, err := db.LoadSession(c.Key)
	if err != nil || re.CopiesOf != c.BranchOf || len(re.Copies) != 1 || re.Copies[1] != c.Copies[1] {
		t.Fatalf("restored copies %v %+v %v", re.CopiesOf, re.Copies, err)
	}
	if own := memory.Own(re); len(own) != 1 || !own[0].Inherited || own[0].Source.Session != "chat:P" {
		t.Fatalf("restored own %+v", own)
	}
}
