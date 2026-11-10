package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// The real Deliver path keeps each record and its boundary. Only the repeated
// body of the exact stored basis is shared; a different occurrence is not.
func TestDeliverSharesSelectedBasisBody(t *testing.T) {
	testDeliverSharesSelectedBasisBody(t, false, false)
}

func TestDeliverSharesEarlierSelectedBasisBody(t *testing.T) {
	testDeliverSharesSelectedBasisBody(t, true, false)
}

func TestDeliverSharingKeepsOtherOccurrence(t *testing.T) {
	testDeliverSharesSelectedBasisBody(t, false, true)
}

func TestDeliverSharingKeepsEarlierOtherOccurrence(t *testing.T) {
	testDeliverSharesSelectedBasisBody(t, true, true)
}

func testDeliverSharesSelectedBasisBody(t *testing.T, sourceFirst, sameTextOtherID bool) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"첫 소문", "들음"}, {"둘째 소문", "들음"}, {"걸었다", "길"}}, "서진이 배신자라고?")
	sourceTurn, viewTurn := 0, 1
	if sourceFirst {
		sourceTurn, viewTurn = 1, 0
	}
	body := `도윤이 "서진은 배신자"라는 소문을 전했으나 진위는 아직 확인하지 못했다.`
	quoted, _ := json.Marshal(body)
	_, source, _ := a24Save(t, db, turnSource(s, sourceTurn), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m0","kind":"event","body":`+string(quoted)+`,"roles":{"speaker":["n3"],"listener":["n1"]},"time":"첫날 밤","scope":"국경 검문 중","terms":"확인 전에는 단정하지 않는다","hide_from":["n2"]}]}`)
	ref := source.Memories[0].ID
	n1, n2, n3 := source.Names[0].ID, source.Names[1].ID, source.Names[2].ID
	extra := ""
	if sameTextOtherID {
		extra = fmt.Sprintf(`,{"id":"m3","kind":"event","body":%s,"time":"다른 날 밤","roles":{"speaker":[%q],"listener":[%q]}}`, quoted, n3, n2)
	}
	_, views, _ := a24Save(t, db, turnSource(s, viewTurn), fmt.Sprintf(`{"memories":[{"id":"m1","kind":"view","owner":%q,"about":%q,"aspect":"belief","body":"배신 가능성을 의심한다","basis":{"via":"소문","from":%q,"ref":%q,"confidence":"낮음"},"affect":{"intensity":"강함","persistence":"남음"},"differs":"unknown"},{"id":"m2","kind":"view","owner":%q,"about":%q,"aspect":"emotion","body":"서진이 배신자라 불안하다","basis":{"via":"추론","ref":%q}}%s]}`, n1, n2, n3, ref, n3, n2, ref, extra))
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진이 배신자라고?", Payload: printsOf(turnSource(s, 2)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	si, vi := slices.Index(d.IDs, ref), slices.Index(d.IDs, views.Memories[0].ID)
	if si < 0 || vi < 0 || !has(d.IDs, views.Memories[1].ID) || (si < vi) != sourceFirst {
		t.Fatalf("fixture order or selected records changed: %v", d.IDs)
	}
	wantCopies := 1
	if sameTextOtherID {
		wantCopies++
		if !has(d.IDs, views.Memories[2].ID) {
			t.Fatal("different occurrence was dropped")
		}
	}
	if got := strings.Count(d.Block, body); got != wantCopies {
		t.Fatalf("body repeated or a different occurrence merged: got %d, want %d\n%s", got, wantCopies, d.Block)
	}
	lines := strings.Split(strings.TrimSpace(d.Block), "\n")[1:]
	sourceBody, viewBasis := "[text B1]", `based on B1 "`+body+`"`
	if sourceFirst {
		sourceBody, viewBasis = `[text B1="`+body+`"]`, "based on B1"
		if strings.Contains(lines[vi], `based on B1 "`) {
			t.Fatalf("forward definition repeated on view: %q", lines[vi])
		}
	}
	for _, want := range []string{sourceBody, "speaker 도윤", "listener 리나", "첫날 밤", "국경 검문 중", "확인 전에는 단정하지 않는다", "hide from: 서진"} {
		if !strings.Contains(lines[si], want) {
			t.Fatalf("source qualifier lost: %q in %q", want, lines[si])
		}
	}
	for _, want := range []string{viewBasis, "confidence 낮음", "feeling 강함 남음", "differs from the facts: unknown", "hide from: 서진"} {
		if !strings.Contains(lines[vi], want) {
			t.Fatalf("view or basis qualifier lost: %q in %q", want, lines[vi])
		}
	}
}

func TestDeliverSharingDoesNotExpandShortBody(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"첫 소문", "들음"}, {"걸었다", "길"}}, "서진에 대한 소문")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"}],"memories":[{"id":"m0","kind":"fact","about":"n2","body":"옴"},{"id":"m1","kind":"view","owner":"n1","about":"n2","body":"서진에 대한 소문을 믿는다","basis":{"ref":"m0"}}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진에 대한 소문", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.IDs) != 2 || strings.Contains(d.Block, "[text B") || !strings.Contains(d.Block, `based on B1 "옴"`) {
		t.Fatalf("short body expanded or basis omitted: %q", d.Block)
	}
}
