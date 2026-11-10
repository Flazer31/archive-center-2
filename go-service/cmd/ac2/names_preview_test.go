package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/embed"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/store"
)

func TestC17PreviewHTTP(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"entities":[{"id":"n1","name":"공개 이름","type":"person"},{"id":"n2","name":"감춰진 이름","type":"thing","hide_from":["독자"]}],"memories":[{"id":"m1","kind":"fact","about":"n2","body":"비밀 본문"}]}`)
	ids, err := db.ItemIDs(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"chat_id":"U3","keep":%q,"merge":%q}`, ids["entities/0"], ids["entities/1"])
	h := namesPreviewHandler(db) // no pipeline or worker to invoke, even cold
	a := callMem(t, h, "POST", "/names/merge/preview", body)
	if a.Code != 200 {
		t.Fatalf("%d %s", a.Code, a.Raw)
	}
	var p store.NameMergePreview
	if err = json.Unmarshal([]byte(a.Raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.Counts["memories_fact"].Count == nil || *p.Counts["memories_fact"].Count != 1 || p.Keep.Type != "person" || p.Merge.Type != "thing" {
		t.Fatal(a.Raw)
	}
	for _, v := range []string{"공개 이름", "감춰진 이름", "비밀 본문"} {
		if strings.Contains(a.Raw, v) {
			t.Fatal("preview leaked text", a.Raw)
		}
	}
	for _, c := range []struct {
		method, body string
		status       int
		code         string
	}{
		{"GET", "", 405, "method_not_allowed"}, {"POST", "{", 400, "bad_request"}, {"POST", `{"keep":"N1","merge":"N2"}`, 400, "chat_required"},
		{"POST", strings.Replace(body, "U3", "OTHER", 1), 404, "name_not_found"},
		{"POST", fmt.Sprintf(`{"chat_id":"U3","keep":%q,"merge":"N9999999"}`, ids["entities/0"]), 404, "name_not_found"},
	} {
		v := callMem(t, h, c.method, "/names/merge/preview", c.body)
		if v.Code != c.status || !strings.Contains(v.Raw, c.code) {
			t.Fatal(v.Raw)
		}
	}
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.Routes = map[string]http.HandlerFunc{"/names/merge/preview": h}
	srv.Token, _, err = observe.LoadToken(filepath.Join(t.TempDir(), "c17-synthetic-token"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := srv.Token.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"", "wrong", token} {
		r := httptest.NewRequest("POST", "/names/merge/preview", strings.NewReader(body))
		r.Header.Set(observe.TokenHeader, v)
		w := httptest.NewRecorder()
		srv.Public().ServeHTTP(w, r)
		if v == token {
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
		} else if w.Code != 401 || strings.Contains(w.Body.String(), "counts") {
			t.Fatal("auth boundary failed", w.Code, w.Body.String())
		}
	}
	// No preceding preview is required by the existing merge endpoint.
	j2 := u3Save(t, db, srcs[1], `{"entities":[{"id":"n1","name":"fresh keep"},{"id":"n2","name":"fresh merge"}]}`)
	fresh, err := db.ItemIDs(j2.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf(`{"chat_id":"U3","keep":%q,"merge":%q}`, fresh["entities/0"], fresh["entities/1"])
	if v := callMem(t, namesHandler(pipe, db, nil), "POST", "/names/merge", request); v.Code != 200 {
		t.Fatal("merge acquired a preview prerequisite", v.Raw)
	}
}

func TestC17PreviewEmbeddingKeys(t *testing.T) {
	db, _, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"entities":[{"id":"n1","name":"A"},{"id":"n2","name":"B"}],"memories":[{"id":"m1","kind":"fact","about":"n2","body":"changed"},{"id":"m2","kind":"view","body":"same"}]}`)
	ids, err := db.ItemIDs(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := db.PreviewMergeNames(srcs[0].Session, ids["entities/0"], ids["entities/1"])
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"flat-test", "voyage-context-3"}
	before := map[string]map[string]string{}
	for _, model := range models {
		before[model], err = embed.Documents(db, model, []int64{j.ID})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.MergeNames(srcs[0].Session, ids["entities/0"], ids["entities/1"]); err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		after, err := embed.Documents(db, model, []int64{j.ID})
		if err != nil {
			t.Fatal(err)
		}
		var changed int64
		for id, key := range before[model] {
			if after[id] != key {
				changed++
			}
		}
		field := "flat_documents"
		if model == "voyage-context-3" {
			field = "contextual_documents"
		}
		if p.Counts[field].Count == nil || *p.Counts[field].Count != changed {
			t.Fatalf("%s key changes=%d preview=%+v", model, changed, p.Counts[field])
		}
		t.Logf("%s actual key changes=%d", model, changed)
	}
}

// Emit a real /memory/detail response for the read-only JS review. No JS
// fixture is allowed to silently rename Go fields or substitute q for source.
func TestC17ReviewDetailHTTPFixture(t *testing.T) {
	db := modelsDB(t)
	tr := &lifecycle.Turn{ID: 7, Confirmed: true, Members: []*lifecycle.Member{
		{Key: "u7", Data: "first input " + strings.Repeat("long original <&> 😀\n", 1024), State: lifecycle.Active},
		{Key: "ub7", Data: "second input", State: lifecycle.Active}}, Responses: []*lifecycle.ResponseRow{
		{Key: "a7", State: lifecycle.Active, Gen: "g1", Revisions: []lifecycle.Revision{{Seq: 1, Data: "first response", GenerationID: "g1"}}},
		{Key: "ab7", State: lifecycle.Active, Gen: "g2", Revisions: []lifecycle.Revision{{Seq: 1, Data: "second response", GenerationID: "g2"}}}}}
	var parentTurns []*lifecycle.Turn
	for id := 1; id < 7; id++ {
		parentTurns = append(parentTurns, &lifecycle.Turn{ID: id, Confirmed: true, Members: []*lifecycle.Member{{Key: fmt.Sprintf("earlier%d", id), Data: "earlier input", State: lifecycle.Active}}})
	}
	parentTurns = append(parentTurns, tr)
	parent := lifecycle.Restore("chat:PARENT", parentTurns, nil)
	if err := db.SaveSession(parent, parent.TakeEvents(), parent.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	src := memory.SourceOf(parent.Key, tr)
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"entities":[{"id":"n1","name":"이름 label"}],"memories":[{"id":"m1","kind":"event","body":"event body","effects":[{"about":"n1","slot":"balance","before":"2","value":"1"}]},{"id":"m2","kind":"view","owner":"n1","body":"selected view","at":"m1.1","basis":{"ref":"M9999999","via":"inference"},"was":["m3"],"hide_from":["n1"],"allow_to":["n1"],"knows":[{"who":"n1"}]},{"id":"m3","kind":"fact","body":"deleted record"}],"links":[{"from":"m2","to":"m1","rel":"recalls"}]}`
	o, notes := memory.Parse(raw)
	if len(notes) > 0 {
		t.Fatal(notes)
	}
	if _, err = db.SaveGeneration(j, store.Generation{Request: "NOT ORIGINAL REQUEST", Response: "NOT ORIGINAL MODEL OUTPUT", Output: o}); err != nil {
		t.Fatal(err)
	}
	ids, err := db.ItemIDs(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.DeleteMemories(parent.Key, []string{ids["memories/2"]}); err != nil {
		t.Fatal(err)
	}
	outside := u3Save(t, db, memory.Source{Session: "chat:OUTSIDE", Turn: 1}, `{"memories":[{"id":"m1","kind":"fact","body":"OUT OF SCOPE BODY MUST NOT APPEAR"}]}`)
	outIDs, err := db.ItemIDs(outside.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbSQL(t, db).Exec(`INSERT INTO links(session_key,job_id,from_ref,rel,to_ref,q) VALUES(?,?,?,?,?,?)`, parent.Key, j.ID, ids["memories/1"], "recalls", outIDs["memories/0"], "synthetic outside endpoint"); err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	selected := it.Memories[1].Memory
	selected.Was = append(selected.Was, outIDs["memories/0"])
	detail, err := json.Marshal(selected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbSQL(t, db).Exec(`UPDATE memories SET detail=? WHERE id=?`, string(detail), strings.TrimPrefix(ids["memories/1"], "M")); err != nil {
		t.Fatal(err)
	}
	childTurn := *tr
	childTurn.ID = 3
	child := lifecycle.Restore("chat:REVIEW", []*lifecycle.Turn{&childTurn}, nil)
	child.BranchOf = [2]string{parent.Key, "ab7"}
	frozen, _ := json.Marshal(src)
	child.RecordCopies(child.BranchOf, map[int]lifecycle.Copy{3: {Own: memory.WholeSourceOf(child.Key, &childTurn).Key(), Source: string(frozen)}})
	if err = db.SaveSession(child, child.TakeEvents(), child.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	tr.Responses = append(tr.Responses, &lifecycle.ResponseRow{Key: "off7", State: lifecycle.Active, Off: true, Revisions: []lifecycle.Revision{{Seq: 1, Data: "OFF RESPONSE MUST NOT APPEAR"}}})
	if err = db.SaveSession(parent, parent.TakeEvents(), parent.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/memory/detail?chat_id=REVIEW&turn_id=3&id=%s&limit=2", ids["memories/1"])
	for {
		a := callMem(t, memoriesHandler(nil, db, nil, nil), "GET", url, "")
		if a.Code != 200 {
			t.Fatalf("%d %s", a.Code, a.Raw)
		}
		t.Log("C17_REVIEW_DETAIL=" + strings.TrimSpace(a.Raw))
		var d store.MemoryDetail
		if err = json.Unmarshal([]byte(a.Raw), &d); err != nil {
			t.Fatal(err)
		}
		if !d.Inherited || len(d.Source.Members) != 2 || len(d.Source.Responses) != 2 || d.SourceTurn != 7 || d.Turn != 1 {
			t.Fatal("fixture source/locator mismatch")
		}
		if d.NextCursor == nil {
			break
		}
		url = fmt.Sprintf("/memory/detail?chat_id=REVIEW&turn_id=3&id=%s&limit=2&cursor=%s", ids["memories/1"], *d.NextCursor)
	}
}
