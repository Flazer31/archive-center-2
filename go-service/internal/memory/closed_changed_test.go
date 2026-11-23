package memory

import (
	"strings"
	"testing"
)

// 1.4: the closing rule offers "changed" for a plan another one replaced. It
// closes the record like the other ending statuses; before 1.4 it left the
// record open for good (L 183 turns: M578, M609 closed as changed stayed OPEN).
func TestClosedAsChangedIsNotOpen(t *testing.T) {
	out, _ := Parse(`{"closed":[{"ref":"M12","status":"changed","how":"약속이 일주일 뒤로 바뀌었다.","q":"일주일 뒤에"}]}`)
	got, notes := Fold(out, Known{})
	if len(got.Memories) != 1 || len(notes) != 0 {
		t.Fatalf("carrier: %+v notes %v", got.Memories, notes)
	}
	k := Known{Memories: []KnownMemory{
		{ID: "M12", Turn: 1, Memory: Memory{Kind: "fact", Mode: "의무", About: "N1", Body: "가람은 내일 데리러 와야 한다."}},
		{ID: "M13", Turn: 1, Memory: Memory{Kind: "fact", Mode: "의무", About: "N2", Body: "다온은 성문을 지켜야 한다."}},
		{ID: "M20", Turn: 3, Memory: got.Memories[0]},
		// a revised promise written with its own status "changed" still binds
		{ID: "M21", Turn: 3, Memory: Memory{Kind: "fact", Mode: "의무", About: "N1", Body: "가람은 다음 주에 데리러 와야 한다.", Status: "changed"}},
	}}
	open := map[string]bool{}
	for _, m := range CurrentFacts(k) {
		open[m.ID] = true
	}
	if open["M12"] {
		t.Fatalf("M12 still open after it was closed as changed")
	}
	if !open["M13"] {
		t.Fatalf("M13 has no status effect and must stay open")
	}
	if !open["M21"] {
		t.Fatalf("M21 is a revised promise (own status changed) and must stay open")
	}
}

// The closing rule tells the critic to close records the story went past, and
// keeps a standing rule open when it is broken or kept once, and never closes
// by age or silence alone (v1 closed three standing address rules as missed).
// v4 (Sol's independent judgment of v3): the scene only moving on is not the
// story going past it, a line with several parts stays open while a part is
// left, and a policy or long-term goal carried out once or partly reached
// stays open.
func TestClosingRuleCoversIndirectEnds(t *testing.T) {
	for _, want := range []string{"the story has gone past it", "A condition that starts or triggers it", "someone else or another event did it", "does not end when someone breaks it, keeps it, carries it out once or reaches part of it", "Never close a line only because it is old or not mentioned", "the scene only moving on to other things", "ends only when every part has ended"} {
		if !strings.Contains(StructureRules, want) {
			t.Fatalf("closing rule lacks %q", want)
		}
	}
}
