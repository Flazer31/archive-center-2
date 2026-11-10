package store

import (
	"archivecenter2/goservice/internal/memory"
	"strings"
	"testing"
)

func assertTransportLines(t *testing.T, d Delivery) {
	t.Helper()
	ordinary, secret := []string{}, []string{}
	lines := strings.Split(strings.TrimSuffix(d.Block, "\n"), "\n")[1:]
	if len(lines) != len(d.IDs) {
		t.Fatal("fixture lines/IDs differ")
	}
	for i, line := range lines {
		if d.SecretIDs[d.IDs[i]] {
			secret = append(secret, line)
		} else {
			ordinary = append(ordinary, line)
		}
	}
	if d.PDFBlock != deliveryBlock(ordinary) || d.RetainedBlock != deliveryBlock(secret) {
		t.Fatal("transport changed, duplicated or reordered selected lines")
	}
}

func TestDeliverTransportLanesVerbatim(t *testing.T) {
	db, s, _ := deliverChat(t)
	for _, budgets := range [][2]int{{2000, 4000}, {2000, 0}, {0, 4000}} {
		d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "리나에게 묻는다", Payload: printsOf(turnSource(s, 3)), Chars: budgets[0], Secret: budgets[1]})
		if err != nil {
			t.Fatal(err)
		}
		if d.Block == "" {
			t.Fatal("fixture selected no records")
		}
		assertTransportLines(t, d)
		if d.Chars > budgets[0] || d.SecretChars > budgets[1] {
			t.Fatal("ceiling changed")
		}
		if budgets[0] == 0 && d.PDFBlock != "" {
			t.Fatal("secret-only contains PDF lines")
		}
		if budgets[1] == 0 && d.RetainedBlock != "" {
			t.Fatal("ordinary-only contains retained lines")
		}
	}
}

func TestDeliverTransportSharedBasisAcrossLanes(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 소문을 들었다", "들음"}, {"걸었다", "길"}}, "서진이 배신자라고?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m0","kind":"event","body":"도윤이 서진의 배신을 말했다"},{"id":"m1","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진이 배신자라고 믿는다","basis":{"via":"report","ref":"m0"}},{"id":"m2","kind":"view","owner":"n3","about":"n2","aspect":"emotion","body":"서진이 배신자라 분노한다","basis":{"via":"inference","ref":"m0"},"hide_from":["n1"]}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진이 배신자라고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	assertTransportLines(t, d)
	if d.PDFBlock == "" || d.RetainedBlock == "" {
		t.Fatalf("must cover mixed lanes: %+v", d)
	}
	union := d.PDFBlock + d.RetainedBlock
	if strings.Count(d.PDFBlock, `[text B1="도윤이 서진의 배신을 말했다"]`) != 1 || strings.Count(union, "based on B1") != 2 || strings.Count(union, "도윤이 서진의 배신을 말했다") != 1 || strings.Contains(union, `based on B1 "`) {
		t.Fatal("shared basis definition/ref changed", union)
	}
	if !strings.Contains(d.RetainedBlock, "hide from: 리나") || strings.Contains(d.PDFBlock, "분노한다") {
		t.Fatal("awareness/lane protection lost")
	}
}
