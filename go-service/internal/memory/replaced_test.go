package memory

import (
	"strings"
	"testing"
)

// A fact that held several values (live chat 2026-11-10: age, money, place
// and skills in one turn-1 fact) is not replaced by records that each change
// one of them under their own slot; the same fact named under one other
// slot, or by a record of its own slot, is replaced as before.
func TestSupersededKeepsAFactChangedInPart(t *testing.T) {
	profile := KnownMemory{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Slot: "현재 상태", Body: "한얼은 18세이고 15냥을 가졌으며 저택에 있다"}}
	money := KnownMemory{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Body: "한 냥을 썼다", Effects: []Effect{{About: "N1", Slot: "소지금", Before: "15냥", Value: "14냥", Was: Refs{"M1"}}}}}
	place := KnownMemory{ID: "M3", Turn: 3, Memory: Memory{Kind: "event", Body: "한양에 갔다", Effects: []Effect{{About: "N1", Slot: "위치", Before: "저택", Value: "한양", Was: Refs{"M1"}}}}}
	skill := KnownMemory{ID: "M4", Turn: 4, Memory: Memory{Kind: "fact", About: "N1", Slot: "습득 능력", Body: "한얼은 서예를 익혔다", Was: Refs{"M1"}}}
	k := Known{Names: []KnownName{{ID: "N1", Name: "한얼"}}, Memories: []KnownMemory{profile, money, place, skill}}
	if Superseded(k)["M1"] {
		t.Fatal("a fact changed under three other slots was replaced whole: the value nobody changed is lost")
	}
	if got := strings.Join(PartlyChanged(k)["M1"], ","); got != "소지금,위치,습득 능력" {
		t.Fatalf("changed slots %q", got)
	}
	// the generation call reads it again as a current fact of the person
	shown := false
	for _, m := range Select(k, "한얼이 걸었다", nil).Memories {
		shown = shown || m.ID == "M1"
	}
	if !shown {
		t.Fatal("the partly changed fact is not read as a current fact")
	}
	// named under one other slot only: replaced, as before (the model often
	// writes the same value's slot differently)
	one := Known{Memories: []KnownMemory{profile, money}}
	if !Superseded(one)["M1"] || len(PartlyChanged(one)) != 0 {
		t.Fatalf("one other slot: superseded %v partly %v", Superseded(one), PartlyChanged(one))
	}
	// a record of its own slot replaces it whatever else names it
	whole := KnownMemory{ID: "M5", Turn: 5, Memory: Memory{Kind: "fact", About: "N1", Slot: "현재 상태", Body: "한얼은 19세이다", Was: Refs{"M1"}}}
	k.Memories = append(k.Memories, whole)
	if !Superseded(k)["M1"] || len(PartlyChanged(k)) != 0 {
		t.Fatalf("own slot: superseded %v partly %v", Superseded(k), PartlyChanged(k))
	}
	// a record without a slot (a view, a slotless fact) replaces as before
	k.Memories[4] = KnownMemory{ID: "M5", Turn: 5, Memory: Memory{Kind: "fact", Body: "한얼의 사정이 달라졌다", Was: Refs{"M1"}}}
	if !Superseded(k)["M1"] {
		t.Fatal("a slotless record's was stopped replacing")
	}
}

// An event that a later effect names stays (it happened); the value it set
// for that slot is replaced, its other values are not. When none of its
// values has that slot, all of them are replaced, as before the change.
func TestSupersededKeepsAnEventNamedByAnEffect(t *testing.T) {
	k := Known{Memories: []KnownMemory{
		{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "약방에서 약재상을 만나 다섯 냥을 치렀다", Effects: []Effect{
			{About: "N1", Slot: "소지금", Before: "26냥", Value: "21냥"}, {About: "N1", Slot: "소지품", Value: "약재"}}}},
		{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Body: "한 냥을 썼다", Effects: []Effect{{About: "N1", Slot: "소지금", Before: "21냥", Value: "20냥", Was: Refs{"M1"}}}}},
	}}
	sup := Superseded(k)
	if sup["M1"] {
		t.Fatal("an event was replaced by a later change of a value it set")
	}
	if !sup["M1.1"] || sup["M1.2"] {
		t.Fatalf("values of the named event: %v", sup)
	}
	k.Memories[1].Effects[0].Slot = "돈"
	if sup = Superseded(k); sup["M1"] || !sup["M1.1"] || !sup["M1.2"] {
		t.Fatalf("no value of that slot: %v", sup)
	}
	// a record that names the event by its own was still replaces it whole
	k.Memories = append(k.Memories, KnownMemory{ID: "M3", Turn: 3, Memory: Memory{Kind: "fact", About: "N1", Slot: "소지금", Body: "20냥이 있다", Was: Refs{"M1"}}})
	if sup = Superseded(k); !sup["M1"] || !sup["M1.2"] {
		t.Fatalf("memory-level was on an event: %v", sup)
	}
}

// The generation call reads a partly changed fact
// with the slots later records changed, as the delivery line does, so its
// old values of those are not taken as current; a fact replaced whole and an
// unchanged fact carry no such note.
func TestRenderKnownMarksAFactChangedInPart(t *testing.T) {
	profile := KnownMemory{ID: "M1", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Slot: "현재 상태", Body: "한얼은 18세이고 15냥을 가졌으며 저택에 있다"}}
	money := KnownMemory{ID: "M2", Turn: 2, Memory: Memory{Kind: "event", Body: "한 냥을 썼다", Effects: []Effect{{About: "N1", Slot: "소지금", Before: "15냥", Value: "14냥", Was: Refs{"M1"}}}}}
	place := KnownMemory{ID: "M3", Turn: 3, Memory: Memory{Kind: "event", Body: "한양에 갔다", Effects: []Effect{{About: "N1", Slot: "위치", Before: "저택", Value: "한양", Was: Refs{"M1"}}}}}
	skill := KnownMemory{ID: "M4", Turn: 4, Memory: Memory{Kind: "fact", About: "N1", Slot: "습득 능력", Body: "한얼은 서예를 익혔다", Was: Refs{"M1"}}}
	old := KnownMemory{ID: "M5", Turn: 1, Memory: Memory{Kind: "fact", About: "N1", Slot: "기분", Body: "한얼은 들떠 있다"}}
	mood := KnownMemory{ID: "M6", Turn: 4, Memory: Memory{Kind: "fact", About: "N1", Slot: "기분", Body: "한얼은 지쳤다", Was: Refs{"M5"}}}
	k := Known{Names: []KnownName{{ID: "N1", Name: "한얼"}}, Memories: []KnownMemory{profile, money, place, skill, old, mood}}
	lineOf := func(text, id string) string {
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, "["+id+"] ") {
				return l
			}
		}
		return ""
	}
	// the earlier value M5 is brought by recall ("들떠"), so it is rendered too
	sel, _ := SelectWithin(k, "한얼이 들떠 걸었다", nil, 0)
	text := RenderKnown(sel, Superseded(k))
	if l := lineOf(text, "M1"); !strings.Contains(l, " | changed later: 소지금, 위치, 습득 능력") {
		t.Fatalf("partly changed fact read without the changed slots: %q\n%s", l, text)
	}
	for _, id := range []string{"M4", "M5", "M6"} {
		l := lineOf(text, id)
		if l == "" {
			t.Fatalf("%s not rendered:\n%s", id, text)
		}
		if strings.Contains(l, "changed later") {
			t.Fatalf("%s carries the partly changed note: %q", id, l)
		}
	}
	if strings.Count(text, "changed later") != 1 {
		t.Fatalf("the note is not on exactly one line:\n%s", text)
	}
}
