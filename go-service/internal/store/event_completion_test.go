package store

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Real storage and Deliver, with synthetic text and no provider boundary.
// The two recalled events are candidates, but only the short one fits.
func TestDeliverCompletesDeliveredEventOnly(t *testing.T) {
	db := testDB(t)
	input := "리나가 진홍인장과 녹색혜성을 떠올렸다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"셋째", "세 번째 답"}, {"오늘", "마지막 답"}}, input)
	raw := `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"세계 규칙"},{"id":"n3","name":"도윤"}],"memories":[
 {"id":"m1","kind":"event","body":"진홍인장이 빛났다"},
 {"id":"m2","kind":"fact","about":"n2","at":"m1","body":"봉인은 같은 등급의 열쇠로만 열린다"},
 {"id":"m3","kind":"event","body":"녹색혜성을 보았다. ` + strings.Repeat("하늘의 기록을 길게 읽었다. ", 600) + `"},
 {"id":"m4","kind":"fact","about":"n2","at":"m3","body":"왕가의 보물은 지하에 있다","hide_from":["n3"]},
 {"id":"m5","kind":"fact","about":"n1","body":"리나는 약사다"},
 {"id":"m6","kind":"fact","about":"n1","at":"m3","body":"리나는 왕녀다","hide_from":["n3"]},
 {"id":"m7","kind":"view","owner":"n2","at":"m1","body":"실패가 두렵다","basis":{"ref":"m1"}},
 {"id":"m8","kind":"fact","about":"n2","at":"m2","body":"고대 문헌의 부록"},
 {"id":"m9","kind":"fact","about":"n2","at":"m1","body":"낡은 통행 규칙"},
 {"id":"m10","kind":"fact","about":"n2","body":"새로운 통행 규칙","was":"m9"},
 {"id":"m11","kind":"fact","about":"n2","at":"m1","mode":"의무","status":"fulfilled","body":"문지기에게 열쇠를 돌려준다"}]}`
	_, it, _ := a24Save(t, db, turnSource(s, 0), raw)
	ids := map[string]string{}
	for i, label := range []string{"event", "rule", "large_event", "unrelated_secret", "direct_fact", "direct_secret", "view", "chain", "old", "replacement", "closed"} {
		ids[label] = it.Memories[i].ID
	}
	_, recent, _ := a24Save(t, db, turnSource(s, 3), `{"memories":[{"id":"m1","kind":"fact","about":"`+it.Names[1].ID+`","at":"`+ids["event"]+`","body":"今朝だけの条件"}]}`)
	k, err := db.CurrentKnown(s)
	if err != nil {
		t.Fatal(err)
	}
	selected := memory.Select(k, input, nil)
	for _, m := range selected.Memories {
		if m.ID == ids["rule"] || m.ID == ids["unrelated_secret"] {
			t.Fatalf("fixture: missing fact already in generation candidates: %s", m.ID)
		}
	}
	names := map[string]string{}
	for _, n := range k.Names {
		names[n.ID] = n.Name
	}
	sizes := map[string]int{}
	for _, m := range k.Memories {
		var aware []memory.KnownAwareness
		for _, a := range k.Awareness {
			if a.Target == m.ID {
				aware = append(aware, a)
			}
		}
		sizes[m.ID] = recordSize(memory.RenderKnown(memory.Selection{Memories: []memory.KnownMemory{m}, Awareness: aware}, memory.Superseded(k)), deliveryLine(m, names, "", aware))
	}
	// This ceiling is derived from the exact fixture records, not a fitted
	// score or count. It leaves no room for the independent public fact.
	paired := sizes[ids["event"]] + sizes[ids["rule"]]
	for _, tc := range []struct {
		name                 string
		chars, secret        int
		wantRule, wantSecret bool
	}{
		{"complete", paired, 4000, true, true},
		{"no_secret_budget", paired, 0, true, false},
		{"event_only_fits", sizes[ids["event"]], 4000, false, true},
		{"no_event_fits", 0, 4000, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Chars: tc.chars, Secret: tc.secret})
			if err != nil {
				t.Fatal(err)
			}
			if has(d.IDs, ids["rule"]) != tc.wantRule || has(d.IDs, ids["direct_secret"]) != tc.wantSecret {
				t.Fatalf("completion or independently selected secret lost: %+v", d)
			}
			for _, label := range []string{"large_event", "unrelated_secret", "view", "chain", "old", "closed"} {
				if has(d.IDs, ids[label]) {
					t.Fatalf("unexpected %s: %v", label, d.IDs)
				}
			}
			if has(d.IDs, recent.Memories[0].ID) {
				t.Fatal("recent fact completed despite source being in payload")
			}
			if tc.wantRule && (!strings.Contains(d.Block, "같은 등급의 열쇠") || slices.Index(d.IDs, ids["rule"]) != slices.Index(d.IDs, ids["event"])+1) {
				t.Fatalf("rule not delivered with its event: %v", d.IDs)
			}
			if tc.wantSecret && !strings.Contains(d.Block, "hide from: 도윤") {
				t.Fatal("secret boundary lost")
			}
			if d.Chars > tc.chars || d.SecretChars > tc.secret {
				t.Fatalf("ceiling exceeded: %+v", d)
			}
		})
	}
	// A direct fact remains independent even when its event does not fit.
	// Large spare secret capacity still cannot pull a different secret from
	// that omitted event. No second selection pass is used to fill the space.
	baseReq := DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Chars: paired + sizes[ids["direct_fact"]], Secret: 4000}
	before, err := db.Deliver(memory.ScopeOf(s), baseReq)
	if err != nil {
		t.Fatal(err)
	}
	if !has(before.IDs, ids["direct_fact"]) {
		t.Fatal("independent fact lost")
	}
	// Make many more facts at the omitted event. They must not change even
	// the order of the independent records already being delivered.
	var extra []memory.Memory
	for i := 0; i < 20; i++ {
		extra = append(extra, memory.Memory{ID: "m" + strconv.Itoa(i+1), Kind: "fact", About: it.Names[1].ID, At: ids["large_event"], Body: "별자리의 옛 기록"})
	}
	body, err := json.Marshal(memory.Output{Memories: extra})
	if err != nil {
		t.Fatal(err)
	}
	_, added, _ := a24Save(t, db, turnSource(s, 1), string(body))
	if len(added.Memories) != len(extra) {
		t.Fatalf("fixture stored %d of %d added facts", len(added.Memories), len(extra))
	}
	after, err := db.Deliver(memory.ScopeOf(s), baseReq)
	if err != nil {
		t.Fatal(err)
	}
	if before.Block != after.Block {
		t.Fatalf("omitted event's facts changed the delivered block:\nBEFORE %s\nAFTER %s", before.Block, after.Block)
	}
}

func TestDeliverCompletionKeepsFactAwareness(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"오늘", "마지막 답"}}, "진홍인장을 떠올렸다")
	_, it, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"도윤"}],"memories":[{"id":"m1","kind":"event","body":"진홍인장이 빛났다"},{"id":"m2","kind":"fact","at":"m1","body":"봉인의 해제 방법은 왕녀만 안다","unaware":["n1"]}]}`)
	req := DeliveryRequest{Input: "진홍인장을 떠올렸다", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000}
	d, err := db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	if !has(d.IDs, it.Memories[1].ID) || d.SecretChars == 0 || !strings.Contains(d.Block, "unaware: 도윤") {
		t.Fatalf("completion did not carry secret boundary: %+v", d)
	}
	req.Secret = 0
	d, err = db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	if has(d.IDs, it.Memories[1].ID) || !has(d.IDs, it.Memories[0].ID) || d.SecretChars != 0 {
		t.Fatalf("secret completion leaked into main lane: %+v", d)
	}
}

// Completing an event must not put a weakly matching fact ahead of an
// independently relevant record merely because the event was delivered.
func TestDeliverCompletionPreservesIndependentRanks(t *testing.T) {
	db := testDB(t)
	input := "리나가 진홍인장을 떠올렸다"
	s := chatOf(t, [][2]string{{"처음", "첫 답"}, {"둘째", "두 번째 답"}, {"셋째", "세 번째 답"}, {"오늘", "마지막 답"}}, input)
	_, first, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"세계 규칙"}],"memories":[{"id":"m1","kind":"event","body":"진홍인장이 빛났다"}]}`)
	_, second, _ := a24Save(t, db, turnSource(s, 1), `{"memories":[{"id":"m1","kind":"fact","about":"`+first.Names[0].ID+`","body":"리나에게는 항구 보관소에 맡겨둔 약품 상자가 있으며, 담당자는 서쪽 창고에서 인수증을 확인한 다음 상자를 돌려준다"}]}`)
	hashes := map[string]string{first.Memories[0].ID: TextHash("event"), second.Memories[0].ID: TextHash("independent")}
	if err := db.SetDocHashes(hashes); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbeddings("m", []string{hashes[first.Memories[0].ID], hashes[second.Memories[0].ID]}, [][]float32{{1, 0, 0}, {0.8, 0.6, 0}}); err != nil {
		t.Fatal(err)
	}
	req := DeliveryRequest{Input: input, Payload: printsOf(turnSource(s, 3)), Model: "m", Chars: 2000, Secret: 4000}
	base, err := db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.IDs) != 2 {
		t.Fatalf("fixture: expected event and independent fact, got %v", base.IDs)
	}
	req.Chars = base.Chars // exact cost of the two independently selected records
	vectorReq := req
	vectorReq.Query = []float32{1, 0, 0}
	vectorBase, err := db.Deliver(memory.ScopeOf(s), vectorReq)
	if err != nil {
		t.Fatal(err)
	}
	var extra []memory.Memory
	for i := 0; i < 20; i++ {
		extra = append(extra, memory.Memory{ID: "m" + strconv.Itoa(i+1), Kind: "fact", About: first.Names[1].ID, At: first.Memories[0].ID, Body: "봉인의 오래된 문양은 남쪽을 향한다"})
	}
	raw, err := json.Marshal(memory.Output{Memories: extra})
	if err != nil {
		t.Fatal(err)
	}
	_, third, _ := a24Save(t, db, turnSource(s, 2), string(raw))
	if len(third.Memories) != len(extra) {
		t.Fatal("completion fixture missing records")
	}
	var hs []string
	var vs [][]float32
	for _, m := range third.Memories {
		h := TextHash(m.ID)
		hashes[m.ID] = h
		hs = append(hs, h)
		vs = append(vs, []float32{0, 1, 0})
	}
	if err = db.SetDocHashes(hashes); err != nil {
		t.Fatal(err)
	}
	if err = db.SaveEmbeddings("m", hs, vs); err != nil {
		t.Fatal(err)
	}
	after, err := db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	if after.Block != base.Block {
		t.Fatalf("weak completion displaced independent content:\nBEFORE %s\nAFTER %s", base.Block, after.Block)
	}
	vectorAfter, err := db.Deliver(memory.ScopeOf(s), vectorReq)
	if err != nil {
		t.Fatal(err)
	}
	// Semantic centering deliberately depends on the entire corpus. Test
	// ranking before that projection using its production trace instead.
	for _, root := range []string{first.Memories[0].ID, second.Memories[0].ID} {
		find := func(d Delivery) TraceRow {
			for _, row := range d.Trace {
				if row.ID == root {
					return row
				}
			}
			t.Fatalf("root absent from rank trace: %s", root)
			return TraceRow{}
		}
		before, after := find(vectorBase), find(vectorAfter)
		if before.Rank != after.Rank || before.Sim == nil || after.Sim == nil || *before.Sim != *after.Sim {
			t.Fatalf("completion changed independent rank: %+v / %+v", before, after)
		}
	}
}
