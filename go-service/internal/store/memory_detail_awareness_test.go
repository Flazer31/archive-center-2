package store

import (
	"fmt"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

func TestMemoryDetailAwarenessOriginAndEffects(t *testing.T) {
	db := testDB(t)
	first, next := detailFixtureTurn(1, "사건"), detailFixtureTurn(5, "앎 기록")
	s := lifecycle.Restore("chat:AWARE", []*lifecycle.Turn{first, next}, nil)
	detailSaveSession(t, db, s)
	_, items := detailSave(t, db, memory.SourceOf(s.Key, first), `{"entities":[{"id":"n1","name":"보호 대상"}],"memories":[{"id":"m1","kind":"event","body":"상태가 바뀐 사건","effects":[{"about":"n1","slot":"상태","value":"변경"}]}]}`)
	id, name := items.Memories[0].ID, items.Names[0].ID
	j, _ := detailSave(t, db, memory.SourceOf(s.Key, next), fmt.Sprintf(`{"memories":[{"ref":%q,"hide_from":[%q],"knows":[{"who":%q,"via":"전언","from":%q,"q":"전언 인용"}],"allow_to":[%q]}]}`, id+".1", name, name, name, name))
	v := detailGet(t, db, s.Key, first.ID, id)
	if !v.Secret {
		t.Fatal("effect disclosure did not mark the memory protected")
	}
	counts := map[string]int{}
	for _, r := range v.Related {
		if r.Effect != nil {
			if !r.Secret || r.ID != id+".1" {
				t.Fatal("effect protection missing")
			}
		}
		if r.Awareness != nil {
			a := r.Awareness
			counts[a.Kind]++
			if a.Job != j.ID || a.Target != id+".1" || r.Locator.TurnID != next.ID || r.Locator.SourceTurn != next.ID {
				t.Fatalf("awareness source collapsed into selected job: %+v", r)
			}
			if a.Kind == "knows" && (a.Via != "전언" || a.From != name || a.Q != "전언 인용") {
				t.Fatalf("awareness detail lost: %+v", a)
			}
		}
	}
	for _, kind := range []string{"hide_from", "knows", "allow_to"} {
		if counts[kind] != 1 {
			t.Fatalf("awareness kinds mixed: %v", counts)
		}
	}
	list, err := db.ChatMemories(s.Key, memory.Own(s))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Secret != v.Secret {
		t.Fatal("management disclosure rule diverged")
	}
	next.Off = true
	detailSaveSession(t, db, s)
	v = detailGet(t, db, s.Key, first.ID, id)
	if v.Secret || len(v.Related) != 1 || v.Related[0].Effect == nil || v.Related[0].Secret {
		t.Fatalf("disabled source awareness remained visible: %+v", v)
	}
}
