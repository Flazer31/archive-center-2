package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

func TestU3NamesEditMergeAndSearch(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"entities":[{"id":"n1","name":"첫이름","type":"person"},{"id":"n2","name":"옛별명","type":"person","knows":[{"who":"n2","from":"n2"}]},{"id":"n3","name":"제삼자","type":"person","same_as":"n2","how":"disguise"}],"memories":[{"id":"m1","kind":"fact","about":"n2","slot":"직업","body":"옛별명은 약사다","q":"원문 옛별명","to":["n2"],"seen_by":["n2"],"knows":[{"who":"n2","from":"n2","ref":"n2"}],"hide_from":["n2"],"unaware":["n2"],"allow_to":["n2"]},{"id":"m2","kind":"event","body":"물건을 건넸다","roles":{"actor":["n2"],"target":["n2"],"object":["n2"],"place":["n2"],"speaker":["n2"],"listener":["n2"]},"effects":[{"about":"n2","slot":"돈","value":"3냥"}]},{"id":"m3","kind":"view","owner":"n2","about":"n2","body":"다르게 기억한다","basis":{"from":"n2","ref":"m2"}},{"ref":"n2","q":"별명을 다시 불렀다"}],"links":[{"id":"l1","from":"n2","rel":"recalls","to":"m1"}]}`)
	ids, err := db.ItemIDs(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	keep, merge := ids["entities/0"], ids["entities/1"]
	if keep == "" || merge == "" {
		t.Fatal(ids)
	}
	j2 := u3Save(t, db, srcs[1], fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","about":%q,"slot":"취미","body":"독서를 좋아한다"}]}`, merge))
	h := namesHandler(pipe, db, nil)
	names := func() []map[string]any {
		a := callMem(t, h, "GET", "/names?chat_id=U3", "")
		if a.Code != 200 {
			t.Fatal(a.Raw)
		}
		var v struct{ Names []map[string]any }
		if err := json.Unmarshal([]byte(a.Raw), &v); err != nil {
			t.Fatal(err)
		}
		return v.Names
	}
	if all := names(); len(all) != 3 {
		t.Fatalf("names %v", all)
	}
	post := func(path string, v any) {
		b, _ := json.Marshal(v)
		a := callMem(t, h, "POST", path, string(b))
		if a.Code != 200 {
			t.Fatalf("%s: %s", path, a.Raw)
		}
	}
	post("/names/edit", map[string]any{"chat_id": "U3", "id": keep, "name": "새이름"})
	post("/names/merge", map[string]any{"chat_id": "U3", "keep": keep, "merge": merge})
	all := names()
	var kept map[string]any
	for _, n := range all {
		if n["id"] == keep {
			kept = n
		}
		if n["id"] == merge {
			t.Fatalf("merged name still separate %v", all)
		}
	}
	if kept == nil || kept["name"] != "새이름" || kept["type"] != "person" || kept["memories"].(float64) != 4 {
		t.Fatalf("kept %+v", kept)
	}
	aliases := map[string]bool{}
	for _, a := range kept["aliases"].([]any) {
		aliases[a.(string)] = true
	}
	if !aliases["첫이름"] || !aliases["옛별명"] {
		t.Fatalf("lost aliases %v", kept)
	}
	listedAll := listed(t, callMem(t, memoriesHandler(pipe, db, nil, nil), "GET", "/memory/list?chat_id=U3", ""))
	for _, m := range listedAll {
		if m["kind"] == "fact" && m["about"] != "새이름" {
			t.Fatalf("stale listed name %v", m)
		}
		if m["kind"] == "view" && (m["about"] != "새이름" || m["owner"] != "새이름") {
			t.Fatalf("stale view %v", m)
		}
	}
	// Production candidate search must reach the fact through either name,
	// without relying on its original body to contain the new name.
	var session *lifecycle.Session
	if err := pipe.WithSession("chat:U3", func(s *lifecycle.Session) { session = s }); err != nil {
		t.Fatal(err)
	}
	for _, surface := range []string{"새이름", "첫이름", "옛별명"} {
		sel, _, err := db.Candidates(session, surface+"에게 묻는다", 0)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, m := range sel.Memories {
			found[m.ID] = true
		}
		secondIDs, _ := db.ItemIDs(j2.ID)
		if !found[ids["memories/0"]] || !found[secondIDs["memories/0"]] {
			t.Fatalf("%s did not find both facts: %+v", surface, sel.Memories)
		}
	}
	it, err := db.ItemsOf([]int64{j.ID, j2.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range it.Memories {
		for _, ref := range memory.MemoryRefs(m.Memory) {
			if ref.Value == merge {
				t.Fatalf("stale detail ref %+v", m)
			}
		}
		b, _ := json.Marshal(m.Awareness)
		if strings.Contains(string(b), `"`+merge+`"`) {
			t.Fatalf("stale awareness %s", b)
		}
	}
	if it.Memories[0].Body != "옛별명은 약사다" || it.Memories[0].Q != "원문 옛별명" {
		t.Fatalf("source prose was rewritten: %+v", it.Memories[0])
	}
	raw := dbSQL(t, db)
	for _, query := range []string{
		`SELECT COUNT(*) FROM memories WHERE about=? OR owner=?`,
		`SELECT COUNT(*) FROM effects WHERE about=? OR about=?`,
		`SELECT COUNT(*) FROM search_refs WHERE ref=? OR ref=?`,
		`SELECT COUNT(*) FROM awareness WHERE who=? OR from_who=?`,
		`SELECT COUNT(*) FROM awareness WHERE target=? OR ref=?`,
		`SELECT COUNT(*) FROM mentions WHERE target=? OR target=?`,
		`SELECT COUNT(*) FROM links WHERE from_ref=? OR to_ref=?`,
	} {
		var n int
		if err := raw.QueryRow(query, merge, merge).Scan(&n); err != nil || n != 0 {
			t.Fatalf("stale mirror %s: %d %v", query, n, err)
		}
	}
	var refs int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM search_refs WHERE item=? AND field='about' AND ref=?`, ids["memories/0"], keep).Scan(&refs); err != nil || refs != 1 {
		t.Fatalf("rebuilt refs %d %v", refs, err)
	}
	var mentionWords int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM search_words WHERE item=? AND field='mention'`, keep).Scan(&mentionWords); err != nil || mentionWords == 0 {
		t.Fatalf("mention index not moved: %d %v", mentionWords, err)
	}
	// A failed merge changes no reference. A repeated spelling remains a
	// separate name until explicitly merged.
	a := callMem(t, h, "POST", "/names/merge", fmt.Sprintf(`{"chat_id":"U3","keep":%q,"merge":"N99999999"}`, keep))
	if a.Code != 404 {
		t.Fatal(a.Raw)
	}
	if a = callMem(t, h, "POST", "/names/edit", fmt.Sprintf(`{"chat_id":"U3","id":%q,"name":" "}`, keep)); a.Code != 400 {
		t.Fatal(a.Raw)
	}
	// A later critic can refer to the former persistent id. Explicit user
	// aliases must resolve on the normal generation writer too.
	later := u3Save(t, db, srcs[2], fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","about":%q,"slot":"도구","body":"청진기를 쓴다"}]}`, merge))
	laterItems, err := db.ItemsOf([]int64{later.ID})
	if err != nil || len(laterItems.Memories) != 1 || laterItems.Memories[0].About != keep {
		t.Fatalf("later alias resolution %+v %v", laterItems, err)
	}
}

func TestU3NameEditReindexesChangedName(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"entities":[{"id":"n1","name":"예전이름","type":"thing"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"재질","body":"은으로 만들었다"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	raw := dbSQL(t, db)
	if _, err := raw.Exec(`UPDATE memories SET doc_hash=? WHERE job_id=?`, strings.Repeat("a", 64), j.ID); err != nil {
		t.Fatal(err)
	}
	h := namesHandler(pipe, db, nil)
	a := callMem(t, h, "POST", "/names/edit", fmt.Sprintf(`{"chat_id":"U3","id":%q,"name":"고친이름"}`, ids["entities/0"]))
	if a.Code != 200 {
		t.Fatal(a.Raw)
	}
	var hash string
	if err := raw.QueryRow(`SELECT doc_hash FROM memories WHERE job_id=?`, j.ID).Scan(&hash); err != nil || hash != "" {
		t.Fatalf("stale vector %q %v", hash, err)
	}
	var s *lifecycle.Session
	pipe.WithSession("chat:U3", func(v *lifecycle.Session) { s = v })
	for _, name := range []string{"예전이름", "고친이름"} {
		sel, _, err := db.Candidates(s, name, 0)
		if err != nil || len(sel.Memories) != 1 || sel.Memories[0].ID != ids["memories/0"] {
			t.Fatalf("edit lookup %s: %+v %v", name, sel, err)
		}
	}
	list := listed(t, callMem(t, memoriesHandler(pipe, db, nil, nil), "GET", "/memory/list?chat_id=U3", ""))
	if len(list) != 1 || list[0]["about"] != "고친이름" {
		t.Fatal(list)
	}
}
