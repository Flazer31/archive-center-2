package memory

import (
	"strings"
	"testing"
)

// A fact or view that names an event by was does not hide the event (11-14:
// in two re-asks of 68 stored turns, place, clothes and state facts named
// past sparring, meetings and trips by was; what happened did not change).
// An event naming an event still replaces it.
func TestFactNamingAnEventByWasKeepsTheEvent(t *testing.T) {
	spar := KnownMemory{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "서준이 도윤과 목검 대련을 했다", Effects: []Effect{{About: "N1", Slot: "HP", Before: "62", Value: "52"}}}}
	place := KnownMemory{ID: "M2", Turn: 2, Memory: Memory{Kind: "fact", Slot: "위치", Body: "서준은 사랑채 마루에 있다", Was: Refs{"M1"}}}
	feel := KnownMemory{ID: "M3", Turn: 3, Memory: Memory{Kind: "view", Body: "서준은 대련이 분하다", Was: Refs{"M1"}}}
	k := Known{Memories: []KnownMemory{spar, place, feel}}
	if Superseded(k)["M1"] {
		t.Fatal("a fact or view naming the event by was hid what happened")
	}
	if !Superseded(k)["M1.1"] {
		t.Fatal("the value the event set is still current after a later record named it")
	}
	// the critic reads the event again when the turn recalls it (the words
	// the fact and view repeat are not an earlier value's)
	picked := false
	for _, m := range Select(k, "서준이 대련을 떠올렸다", nil).Memories {
		picked = picked || m.ID == "M1"
	}
	if !picked {
		t.Fatal("the event is not selected for the critic")
	}
	// the fact it names is still replaced by a fact
	moved := KnownMemory{ID: "M4", Turn: 4, Memory: Memory{Kind: "fact", Slot: "위치", Body: "서준은 침실에 있다", Was: Refs{"M2"}}}
	// an event that rewrites the event still replaces it
	redo := KnownMemory{ID: "M5", Turn: 5, Memory: Memory{Kind: "event", Body: "대련은 사실 진검 승부였다", Was: Refs{"M1"}}}
	k.Memories = append(k.Memories, moved, redo)
	sup := Superseded(k)
	if !sup["M2"] || !sup["M1"] {
		t.Fatalf("replacements of the same kind stopped: %v", sup)
	}
}

// The generation prompt asks a whole replacement to carry what is still
// true (11-14: important loss 47% -> 17% over two re-asks of 106 replaced
// facts). Source-text check only: the effect was measured with the model.
func TestPromptAsksAReplacementToCarryWhatStaysTrue(t *testing.T) {
	p := PromptWithText(Source{Turn: 1}, "", Instructions, "x")
	if !strings.Contains(p, "every detail of the KNOWN record that is still true") {
		t.Fatal("the carry instruction is not in the generation prompt")
	}
}
