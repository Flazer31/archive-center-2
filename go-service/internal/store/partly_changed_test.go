package store

import (
	"encoding/json"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// Live chat 2026-11-10: a turn-1 fact held the age, the money and the place;
// later turns changed the money and the place and named the fact, and the
// age was never delivered again. Deliver now delivers such a fact with the
// slots that changed later; a fact named under one other slot is still not
// delivered.
func TestDeliverFactChangedInPart(t *testing.T) {
	deliverProfile(t, `{"about":"%N","slot":"소지금","before":"15냥","value":"14냥","was":"%M"},{"about":"%N","slot":"위치","before":"약국","value":"장터","was":"%M"}`, true)
}

func TestDeliverFactNamedUnderOneOtherSlot(t *testing.T) {
	deliverProfile(t, `{"about":"%N","slot":"소지금","before":"15냥","value":"14냥","was":"%M"}`, false)
}

func deliverProfile(t *testing.T, effects string, want bool) {
	t.Helper()
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	var rows []lifecycle.Row
	for i, txt := range []string{"리나를 만났다", "리나가 장터에 갔다", "비가 왔다"} {
		n := string(rune('1' + i))
		rows = append(rows, lrow("u"+n, "user", "", txt), lrow("a"+n, "char", "g"+n, txt+" (답)"))
		s.Apply(lsnap(lifecycle.SourceOutputEvent, append([]lifecycle.Row{}, rows...)...))
	}
	s.Apply(lsnap(lifecycle.SourceOutputEvent, append(append([]lifecycle.Row{}, rows...), lrow("u4", "user", "", "리나에게 묻는다"))...))
	_, it1, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나","type":"person"}],
	 "memories":[{"id":"m1","kind":"fact","about":"n1","slot":"현재 상태","mode":"상태","body":"리나는 18세 약사이고 15냥을 가졌으며 약국에 있다"}]}`)
	lina, profile := it1.Names[0].ID, it1.Memories[0].ID
	fx := strings.NewReplacer("%N", lina, "%M", profile).Replace(effects)
	a24Save(t, db, turnSource(s, 1), `{"memories":[{"id":"m1","kind":"event","body":"리나가 장터에 가서 한 냥을 썼다","roles":{"actor":["`+lina+`"]},"outcome":"success","effects":[`+fx+`]}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "리나에게 묻는다", Payload: printsOf(turnSource(s, 2)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	line := ""
	for _, l := range strings.Split(d.Block, "\n") {
		if strings.Contains(l, "18세") {
			line = l
		}
	}
	if got := has(d.IDs, profile); got != want {
		t.Fatalf("profile fact delivered %v, want %v: %v\n%s", got, want, d.IDs, d.Block)
	}
	if want && !strings.HasSuffix(line, " | changed later: 소지금, 위치") {
		t.Fatalf("delivered line does not say what changed later: %q", line)
	}
	// review F1: the memory screen's item says the same (JSON "changed")
	list, err := db.ChatMemories("chat:A", memory.ScopeOf(s))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, m := range list {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(m.Changed, ", ")
		if m.ID == profile {
			seen++
			if want && (got != "소지금, 위치" || !strings.Contains(string(raw), `"changed":["소지금","위치"]`)) {
				t.Fatalf("listed partly changed fact: changed %q\n%s", got, raw)
			}
			if !want && (got != "" || !m.Replaced) {
				t.Fatalf("listed fact replaced whole: changed %q replaced %v", got, m.Replaced)
			}
		} else if got != "" {
			t.Fatalf("listed %s (%s) carries changed %q", m.ID, m.Kind, got)
		}
		if m.Changed == nil && strings.Contains(string(raw), `"changed"`) {
			t.Fatalf("empty changed is sent: %s", raw)
		}
	}
	if seen != 1 {
		t.Fatalf("profile fact listed %d times: %+v", seen, list)
	}
}
