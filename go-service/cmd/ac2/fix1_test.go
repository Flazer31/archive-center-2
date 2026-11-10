package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

func fix1Source(t *testing.T, db *store.DB, key string, place int) memory.Source {
	t.Helper()
	s, found, err := db.LoadSession(key)
	if err != nil || !found {
		t.Fatalf("load %s: found=%v err=%v", key, found, err)
	}
	return memory.Own(s)[place].Source
}

// Compare the two production HTTP handlers, checking the list's membership
// independently against the fixture so equal empty/wrong answers cannot pass.
func fix1Counts(t *testing.T, db *store.DB, pipe *pipeline.Pipeline, chat string, bodies []string) {
	t.Helper()
	h := memoriesHandler(pipe, db, nil, nil)
	counts := map[string]int{"event": 0, "fact": 0, "view": 0, "total": 0}
	var gotBodies []string
	for offset := 0; ; {
		a := callMem(t, h, "GET", fmt.Sprintf("/memory/list?chat_id=%s&offset=%d", chat, offset), "")
		page := listed(t, a)
		for _, m := range page {
			counts[m["kind"].(string)]++
			counts["total"]++
			gotBodies = append(gotBodies, m["body"].(string))
		}
		offset += len(page)
		if offset == int(a.V["total"].(float64)) {
			break
		}
		if len(page) == 0 {
			t.Fatal("list pagination stopped before total")
		}
	}
	sort.Strings(gotBodies)
	wantBodies := append([]string(nil), bodies...)
	sort.Strings(wantBodies)
	if !reflect.DeepEqual(gotBodies, wantBodies) {
		t.Fatalf("list bodies %v, want fixture bodies %v", gotBodies, wantBodies)
	}
	a := callMem(t, chatsHandler(db), "GET", "/chats", "")
	if a.Code != http.StatusOK {
		t.Fatalf("chats %d %s", a.Code, a.Raw)
	}
	for _, v := range a.V["chats"].([]any) {
		c := v.(map[string]any)
		if c["chat_id"] != chat {
			continue
		}
		got := c["memories"].(map[string]any)
		for kind, n := range counts {
			if got[kind] != float64(n) {
				t.Errorf("/chats %s %s=%v, /memory/list=%d", chat, kind, got[kind], n)
			}
		}
		return
	}
	t.Fatalf("chat %s missing: %s", chat, a.Raw)
}

func TestFix1ChatCounts(t *testing.T) {
	for _, scenario := range []string{"normal", "reroll_waiting", "reroll_done", "deleted", "off", "branch", "branch_own_fallback"} {
		t.Run(scenario, func(t *testing.T) {
			db := modelsDB(t)
			pipe := pipeline.New(db)
			rows := []string{memRow(0, "user", "u1", "", "입력 하나"), memRow(1, "char", "a1", "g1", "첫 응답"),
				memRow(2, "user", "u2", "", "입력 둘"), memRow(3, "char", "a2", "g2", "둘째 응답")}
			process := func(seq int, chat string, rs []string) {
				t.Helper()
				if _, err := pipe.Process([]observe.Record{memEvent(seq, "load", chat, rs...)}); err != nil {
					t.Fatal(err)
				}
			}
			process(1, "A", rows)
			first := `{"memories":[{"id":"m1","kind":"event","body":"첫 사건"},{"id":"m2","kind":"fact","body":"첫 사실"},{"id":"m3","kind":"view","body":"첫 생각"}]}`
			if scenario != "branch_own_fallback" {
				u3Save(t, db, fix1Source(t, db, "chat:A", 0), first)
			}
			u3Save(t, db, fix1Source(t, db, "chat:A", 1), `{"memories":[{"id":"m1","kind":"fact","body":"둘째 사실"}]}`)
			want := []string{"첫 사건", "첫 사실", "첫 생각", "둘째 사실"}
			chat := "A"
			switch scenario {
			case "reroll_waiting", "reroll_done":
				rows[1] = memRow(1, "char", "a1", "g1-reroll", "바뀐 응답")
				process(2, "A", rows)
				want = []string{"둘째 사실"}
				if scenario == "reroll_done" {
					u3Save(t, db, fix1Source(t, db, "chat:A", 0), `{"memories":[{"id":"m1","kind":"view","body":"바뀐 생각"}]}`)
					want = append(want, "바뀐 생각")
				}
			case "deleted":
				process(2, "A", rows[:2])
				want = want[:3]
			case "off":
				rows[1] = strings.TrimSuffix(rows[1], "}") + `,"disabled":true}`
				process(2, "A", rows)
				want = []string{"둘째 사실"}
			case "branch", "branch_own_fallback":
				marker := `{"i":2,"role":"char","chat_id":"mk","is_comment":true,"disabled":true,"data":{"text":"{{specialcomment::branchedfrom::A::이름::a1::}}"}}`
				process(2, "B", []string{memRow(0, "user", "ub", "", "입력 하나"), memRow(1, "char", "ab", "g1", "첫 응답"), marker,
					memRow(3, "user", "vb", "", "분기 입력"), memRow(4, "char", "bb", "gb", "분기 응답")})
				s, _, err := db.LoadSession("chat:B")
				if err != nil {
					t.Fatal(err)
				}
				scope := memory.Own(s)
				if !scope[0].Inherited {
					t.Fatal("fixture did not record inheritance")
				}
				if scenario == "branch_own_fallback" {
					u3Save(t, db, scope[0].OwnSource(), first)
				}
				u3Save(t, db, scope[1].Source, `{"memories":[{"id":"m1","kind":"event","body":"분기 사건"}]}`)
				want = append(want[:3], "분기 사건")
				chat = "B"
				page := listed(t, callMem(t, memoriesHandler(pipe, db, nil, nil), "GET", "/memory/list?chat_id=B", ""))
				for _, m := range page {
					if m["body"] == "첫 사실" && m["inherited"] != (scenario == "branch") {
						t.Fatalf("inherited ownership %v", m)
					}
				}
			}
			fix1Counts(t, db, pipe, chat, want)
		})
	}
}

func TestFix1ChatCountsAllPages(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "pages", memRow(0, "user", "u", "", "입력"), memRow(1, "char", "a", "g", "응답"))}); err != nil {
		t.Fatal(err)
	}
	var out memory.Output
	var bodies []string
	for i := 0; i < memoriesPageMax+3; i++ {
		body := fmt.Sprintf("기억 %d", i)
		out.Memories = append(out.Memories, memory.Memory{ID: fmt.Sprintf("m%d", i+1), Kind: []string{"event", "fact", "view"}[i%3], Body: body})
		bodies = append(bodies, body)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	u3Save(t, db, fix1Source(t, db, "chat:pages", 0), string(raw))
	fix1Counts(t, db, pipe, "pages", bodies)
}

func TestFix1MemoryDetails(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "details", memRow(0, "user", "u", "", "약속"), memRow(1, "char", "a", "g", "약속과 생각의 근거"))}); err != nil {
		t.Fatal(err)
	}
	raw := `{
	 "entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"},{"id":"n3","name":"열쇠","type":"thing"},{"id":"n4","name":"주막","type":"place"}],
	 "scenes":[{"id":"s1","change":"open","q":"주막에서"}],
	 "memories":[
	  {"id":"m1","kind":"event","body":"열쇠를 건넸다","q":"리나가 도윤에게 열쇠를 건넸다","time":"1일 아침","outcome":"success","setting":"주막 안",
	   "roles":{"actor":["n1"],"target":["n2"],"object":["n3"],"place":["n4"],"speaker":["n1"],"listener":["n2","낯선 이"]},"seen_by":["n2"],"scene":"s1",
	   "effects":[{"about":"n2","slot":"소지품","before":"없음","value":"열쇠","delta":"+1","status":"open"}],"hide_from":["모르는 사람"]},
	  {"id":"m2","kind":"fact","about":"n1","slot":"약속","body":"열쇠를 돌려받는다","q":"비가 그치면 돌려줘","mode":"약속","scope":"이번 여행","to":["n2","낯선 이"],"terms":"비가 그치면","status":"open","at":"m1","was":["m1.1"],"scene":"s1","imp":0},
	  {"id":"m3","kind":"view","owner":"n2","about":"m1","ref":"m1","body":"리나가 나를 믿는다","q":"나를 믿는 걸까","aspect":"belief","basis":{"via":"전언","from":"n1","ref":"m1","confidence":"추측"},"affect":{"intensity":"강함","persistence":"남음"},"expressed":"속으로","differs":"unknown","unaware":["n1"]},
	  {"id":"m4","kind":"fact","body":"빈 세부 칸"}
	 ]}`
	j := u3Save(t, db, fix1Source(t, db, "chat:details", 0), raw)
	stored, err := db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	h := memoriesHandler(pipe, db, nil, nil)
	page := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=details", ""))
	byBody := map[string]map[string]any{}
	for _, m := range page {
		byBody[m["body"].(string)] = m
	}
	var input memory.Output
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatal(err)
	}
	for _, m := range input.Memories {
		got := byBody[m.Body]
		if got == nil {
			t.Fatalf("memory missing: %s", m.Body)
		}
		// Scalar detail fields must match the written input, including q.
		wantRaw, _ := json.Marshal(m)
		var want map[string]any
		json.Unmarshal(wantRaw, &want)
		for _, field := range []string{"q", "slot", "mode", "scope", "terms", "status", "aspect", "affect", "expressed", "differs", "time", "outcome", "setting", "imp"} {
			if !reflect.DeepEqual(got[field], want[field]) {
				t.Errorf("%s field %s: got=%v want=%v", m.Kind, field, got[field], want[field])
			}
		}
	}
	event, fact, view := byBody[input.Memories[0].Body], byBody[input.Memories[1].Body], byBody[input.Memories[2].Body]
	if fact["about"] != input.Entities[0].Name || view["owner"] != input.Entities[1].Name {
		t.Errorf("name fields fact=%v view=%v", fact, view)
	}
	check := func(field string, got, want any) {
		t.Helper()
		b, _ := json.Marshal(want)
		var decoded any
		json.Unmarshal(b, &decoded)
		if !reflect.DeepEqual(got, decoded) {
			t.Errorf("%s: got=%v want=%v", field, got, decoded)
		}
	}
	check("to", fact["to"], []string{input.Entities[1].Name, "낯선 이"})
	check("seen_by", event["seen_by"], []string{input.Entities[1].Name})
	roles, ok := event["roles"].(map[string]any)
	if !ok {
		t.Errorf("roles missing: %v", event)
	} else {
		for role, want := range map[string][]string{"actor": {input.Entities[0].Name}, "target": {input.Entities[1].Name}, "object": {input.Entities[2].Name}, "place": {input.Entities[3].Name}, "speaker": {input.Entities[0].Name}, "listener": {input.Entities[1].Name, "낯선 이"}} {
			check("roles."+role, roles[role], want)
		}
	}
	basis := *input.Memories[2].Basis
	basis.From, basis.Ref = input.Entities[0].Name, stored.Memories[0].ID
	check("basis", view["basis"], basis)
	effect := input.Memories[0].Effects[0]
	effect.About = input.Entities[1].Name
	check("effects", event["effects"], []memory.Effect{effect})
	check("at", fact["at"], stored.Memories[1].At)
	check("was", fact["was"], stored.Memories[1].Was)
	check("scene", fact["scene"], stored.Memories[1].Scene)
	check("view.about", view["about"], stored.Memories[0].ID)
	check("ref", view["ref"], stored.Memories[2].Ref)
	if event["secret"] != true || view["secret"] != true || fact["secret"] != false {
		t.Errorf("secret flags changed: %v %v %v", event, fact, view)
	}
	check("event.awareness", event["awareness"], []store.ListedAwareness{{Kind: "hide_from", Who: "모르는 사람"}})
	check("view.awareness", view["awareness"], []store.ListedAwareness{{Kind: "unaware", Who: input.Entities[0].Name}})
	for _, field := range []string{"q", "ref", "at", "was", "scene", "roles", "time", "outcome", "setting", "seen_by", "mode", "to", "scope", "terms", "status", "aspect", "basis", "affect", "expressed", "differs"} {
		if _, exists := byBody[input.Memories[3].Body][field]; exists {
			t.Errorf("empty field %s was emitted", field)
		}
	}
	// Listing is a projection, with no writes or name replacement in storage.
	after, err := db.ItemsOf([]int64{j.ID})
	if err != nil || !reflect.DeepEqual(stored, after) {
		t.Errorf("list changed stored records: %v", err)
	}
	other := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=unrelated", ""))
	if len(other) != 0 {
		t.Fatalf("another chat exposed details: %v", other)
	}
	// A branch sees the same stored details and secret/awareness flags,
	// retaining the existing read-only inherited marker.
	marker := `{"i":2,"role":"char","chat_id":"mk","is_comment":true,"disabled":true,"data":{"text":"{{specialcomment::branchedfrom::details::이름::a::}}"}}`
	if _, err := pipe.Process([]observe.Record{memEvent(2, "load", "details-child", memRow(0, "user", "ub", "", "약속"), memRow(1, "char", "ab", "g", "약속과 생각의 근거"), marker, memRow(3, "user", "next", "", "이어서"))}); err != nil {
		t.Fatal(err)
	}
	branch := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=details-child", ""))
	if len(branch) != len(page) {
		t.Fatalf("branch lost details: %v", branch)
	}
	for _, m := range branch {
		want := byBody[m["body"].(string)]
		if m["inherited"] != true {
			t.Errorf("branch lost inherited marker: %v", m)
		}
		delete(m, "inherited")
		wantCopy := make(map[string]any, len(want))
		for k, v := range want {
			if k != "inherited" {
				wantCopy[k] = v
			}
		}
		if !reflect.DeepEqual(m, wantCopy) {
			t.Errorf("inherited details differ: got=%v want=%v", m, wantCopy)
		}
	}
}
