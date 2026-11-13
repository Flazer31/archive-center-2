package main

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// Two names merged are one person for body tracking: the facts written
// under either are one state; undoing the merge reads them apart
// again.
func TestBodyFollowsNameMerge(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	h := bodyHandler(p, db)
	bodyCall(t, h, "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
	seq := 0
	apply := func(rows ...string) []memory.ScopedTurn {
		t.Helper()
		seq++
		if _, err := p.Process([]observe.Record{attEvent(seq, "output", "M", rows...)}); err != nil {
			t.Fatal(err)
		}
		var own []memory.ScopedTurn
		p.WithSession("chat:M", func(s *lifecycle.Session) { own = memory.Own(s) })
		return own
	}
	u1, a1 := memRow(0, "user", "u1", "", "첫날"), memRow(1, "char", "a1", "g1", "리나가 아팠다")
	own := apply(u1, a1)
	u3Save(t, db, own[0].Source, `{"elapsed_days":0,"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"리나가 아팠다"}],"body_events":[{"kind":"period_start","who":"n1","q":"리나"}]}`)
	u2, a2 := memRow(2, "user", "u2", "", "다음"), memRow(3, "char", "a2", "g2", "린이 쉬었다")
	own = apply(u1, a1, u2, a2)
	u3Save(t, db, own[1].Source, `{"elapsed_days":3,"entities":[{"id":"n1","name":"린","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"린이 쉬었다"}],"body_events":[{"kind":"recovery_started","who":"n1","q":"린"}]}`)
	v := bodyGet(t, h, "M")
	if len(v.People) != 2 {
		t.Fatalf("fixture: two names, two people: %v", bodyKinds(v))
	}
	keep, merge := v.People[0].Who, v.People[1].Who
	if _, err := db.MergeNames("chat:M", keep, merge); err != nil {
		t.Fatal(err)
	}
	v = bodyGet(t, h, "M")
	if len(v.People) != 1 || v.People[0].Who != keep || len(v.People[0].Facts) != 2 {
		t.Fatalf("after the merge one person holds both facts: %v", bodyKinds(v))
	}
	merges, err := db.NameMerges("chat:M")
	if err != nil || len(merges) != 1 {
		t.Fatalf("merges %v %v", merges, err)
	}
	if _, err := db.UnmergeNames("chat:M", merges[0].ID); err != nil {
		t.Fatal(err)
	}
	if v = bodyGet(t, h, "M"); len(v.People) != 2 {
		t.Fatalf("undoing the merge reads them apart: %s", strings.Join(bodyKinds(v), ","))
	}
}
