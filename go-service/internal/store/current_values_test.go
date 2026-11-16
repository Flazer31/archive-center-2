package store

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// An event a later fact named by was stays, without the values it set
// (11-14 replay: a kept event line showed "HP 48 -> 52" with no newer HP
// line). An event whose one value a later effect replaced keeps its line as
// before (hiding those too changed which records fit: 13 -> 11 of 39).
func TestDeliveredEventDropsReplacedValues(t *testing.T) {
	k := memory.Known{Memories: []memory.KnownMemory{
		{ID: "M1", Turn: 1, Memory: memory.Memory{Kind: "event", Body: "서준이 도윤과 목검 대련을 했다", Effects: []memory.Effect{
			{About: "N1", Slot: "HP", Before: "48", Value: "52"}, {About: "N1", Slot: "소지품", Value: "목검"}}}},
		{ID: "M2", Turn: 2, Memory: memory.Memory{Kind: "event", Body: "약을 먹었다", Effects: []memory.Effect{{About: "N1", Slot: "HP", Before: "52", Value: "60", Was: memory.Refs{"M1.1"}}}}},
	}}
	names := map[string]string{"N1": "서준"}
	line := func(m memory.KnownMemory) string {
		return deliveryLine(currentValues(m, memory.KeptEvents(k), memory.Superseded(k)), names, "", nil)
	}
	if got, want := line(k.Memories[0]), deliveryLine(k.Memories[0], names, "", nil); got != want {
		t.Fatalf("a value replaced by an effect changed the line:\n%s\n%s", got, want)
	}
	// a fact names the whole event: it stays, its values are not shown
	k.Memories = append(k.Memories, memory.KnownMemory{ID: "M3", Turn: 3, Memory: memory.Memory{Kind: "fact", Slot: "위치", Body: "서준은 사랑채 마루에 있다", Was: memory.Refs{"M1"}}})
	if l := line(k.Memories[0]); strings.Contains(l, "->") || !strings.Contains(l, "목검 대련") {
		t.Fatalf("kept event: %s", l)
	}
	// an event naming it is not a kept event: replaced whole, line as before
	if got, want := line(k.Memories[1]), deliveryLine(k.Memories[1], names, "", nil); got != want {
		t.Fatalf("unreplaced record changed:\n%s\n%s", got, want)
	}
}

// Through the production Deliver (only SQL substituted): the kept event is
// delivered without the value the fact replaced.
func TestDeliverShowsAKeptEventWithoutItsReplacedValue(t *testing.T) {
	mems := []memory.Memory{
		{Kind: "event", Body: "서준이 도윤과 목검 대련을 했다", Roles: &memory.Roles{Actor: memory.Who{"N1"}}, Effects: []memory.Effect{{About: "N1", Slot: "HP", Before: "48", Value: "52"}}},
		{Kind: "fact", About: "N1", Slot: "위치", Body: "서준은 사랑채 마루에 있다", Was: memory.Refs{"M1"}},
	}
	for i := range mems {
		mems[i].Scene = "S1"
	}
	db := fixtureDB(t, &contrastFixture{memories: mems})
	req := syntheticRequest()
	req.Query = nil
	got, err := db.Deliver(syntheticScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Block, "목검 대련") || strings.Contains(got.Block, "48 -> 52") {
		t.Fatalf("delivered:\n%s", got.Block)
	}
}
