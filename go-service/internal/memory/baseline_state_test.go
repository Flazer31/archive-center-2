package memory

import "testing"

func TestBaselineBeforeValueNeedsExplicitReplacement(t *testing.T) {
	k := Known{Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Outcome: "success", Effects: []Effect{{About: "N1", Slot: "value", Value: "old"}}}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Outcome: "success", Effects: []Effect{{About: "N1", Slot: "value", Before: "old", Value: "new"}}}},
	}}
	old := k.Memories[0].ID + ".1"
	if Superseded(k)[old] {
		t.Fatal("a before value alone changed the restored baseline state rule")
	}
	k.Memories[1].Effects[0].Was = Refs{old}
	if !Superseded(k)[old] {
		t.Fatal("explicit replacement stopped working")
	}
}
