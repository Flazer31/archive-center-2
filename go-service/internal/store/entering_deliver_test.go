package store

import (
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Through production Deliver (1.1 B1): with room for one record, 리나's fact
// is delivered when the input names her and the window does not hold her;
// when the window holds her too, the rank order decides as before.
func TestDeliverEnteringFirst(t *testing.T) {
	db := testDB(t)
	input := "리나와 도윤이 장터에 돌아왔다"
	raw := `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[
	 {"id":"m1","kind":"fact","about":"n2","slot":"일","mode":"상태","body":"도윤은 장터에서 대장간을 한다"},
	 {"id":"m2","kind":"fact","about":"n1","slot":"일","mode":"상태","body":"리나는 장터에서 약방을 한다"}]}`
	deliver := func(last string, chars int) (Delivery, map[string]string) {
		t.Helper()
		s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"오늘", last}}, input)
		_, it, _ := a24Save(t, db, turnSource(s, 0), raw)
		d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 2)), Chars: chars, Secret: 4000})
		if err != nil {
			t.Fatal(err)
		}
		if d.Window != 1 {
			t.Fatalf("fixture: window %d", d.Window)
		}
		return d, map[string]string{"도윤": it.Memories[0].ID, "리나": it.Memories[1].ID}
	}
	// sizes and the rank order with room for both, 리나 held by the window
	all, ids := deliver("도윤과 리나의 마지막 답", 4000)
	size := map[string]int{}
	for _, r := range all.Trace {
		size[r.ID] = r.Size
	}
	if len(all.IDs) != 2 || all.IDs[0] != ids["도윤"] {
		t.Fatalf("fixture: 도윤's fact ranks first when no name comes back: %v %v", all.IDs, ids)
	}
	one := max(size[ids["도윤"]], size[ids["리나"]])
	if size[ids["도윤"]]+size[ids["리나"]] <= one {
		t.Fatal("fixture: both fit in the room for one")
	}
	// 리나 held by the window: the rank order, 도윤's fact
	held, ids := deliver("도윤과 리나의 마지막 답", one)
	if len(held.IDs) != 1 || held.IDs[0] != ids["도윤"] {
		t.Fatalf("held: %v, want 도윤's fact %s", held.IDs, ids["도윤"])
	}
	// 리나 not in the window: her fact first
	back, ids := deliver("도윤의 마지막 답", one)
	if len(back.IDs) != 1 || back.IDs[0] != ids["리나"] {
		t.Fatalf("coming back: %v, want 리나's fact %s", back.IDs, ids["리나"])
	}
}
