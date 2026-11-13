package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/embed"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
)

func TestC16PatchHTTPAndKeys(t *testing.T) {
	db, _, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"entities":[{"id":"n1","name":"raw label"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"one","imp":5},{"id":"m2","kind":"fact","body":"two"}]}`)
	other := u3Save(t, db, srcs[1], `{"memories":[{"id":"m1","kind":"fact","body":"other job"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	keys := func(model string) map[string]string {
		t.Helper()
		v, err := embed.Documents(db, model, []int64{j.ID, other.ID})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	flat, ctx := keys("flat-test"), keys("voyage-context-3")
	worker := generate.NewWorker(db, nil, "", time.Minute)
	worker.Index = func(context.Context, []int64) error { t.Fatal("embedding boundary must not run"); return nil }
	// nil pipeline proves that edit ownership uses C15's single-item reader.
	h := memoriesHandler(nil, db, worker, nil)
	post := func(p string) memAnswer {
		return callMem(t, h, "POST", "/memory/edit", fmt.Sprintf(`{"chat_id":"U3","id":%q,%s}`, ids["memories/0"], p))
	}
	if a := post(`"patch":{"imp":0}`); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	if !reflect.DeepEqual(flat, keys("flat-test")) || !reflect.DeepEqual(ctx, keys("voyage-context-3")) {
		t.Fatal("importance changed document keys")
	}
	if worker.Diagnostics()["index_backlog_chats"] != 0 {
		t.Fatal("imp-only queued reindex")
	}
	// Legacy body/imp calls remain accepted.
	if a := post(`"body":"corrected","imp":3`); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	changed := func(before, after map[string]string) int {
		n := 0
		for id, k := range before {
			if after[id] != k {
				n++
			}
		}
		return n
	}
	if n := changed(flat, keys("flat-test")); n != 1 {
		t.Fatal("flat changed", n)
	}
	if n := changed(ctx, keys("voyage-context-3")); n != 2 {
		t.Fatal("contextual changed", n)
	}
	if worker.Diagnostics()["index_backlog_chats"] != 1 {
		t.Fatal("changed document did not queue reindex")
	}
	if a := post(`"patch":{"about":"` + ids["entities/0"] + `","scope":"local","terms":"if asked"}`); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	detail := callMem(t, memoriesHandler(nil, db, nil, nil), "GET", "/memory/detail?chat_id=U3&turn_id=1&id="+ids["memories/0"], "")
	if detail.Code != 200 {
		t.Fatal(detail.Raw)
	}
	raw := detail.V["memory"].(map[string]any)
	if raw["about"] != ids["entities/0"] || detail.V["labels"].(map[string]any)[ids["entities/0"]] != "raw label" {
		t.Fatal("label stored instead of ID", detail.Raw)
	}
	if a := post(`"patch":{"q":"changed"}`); a.Code != 400 {
		t.Fatal("read-only q", a.Raw)
	}
	if a := callMem(t, h, "POST", "/memory/edit", fmt.Sprintf(`{"chat_id":"OTHER","id":%q,"patch":{"body":"bad"}}`, ids["memories/0"])); a.Code != 404 {
		t.Fatal("cross-chat patch", a.Raw)
	}
	t.Log("C16 embedding-key deltas: imp flat=0 contextual=0; body flat=1 contextual=2; unrelated job=0; provider/embedding boundary calls=0")
}

func TestC17UnmergeHTTP(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"entities":[{"id":"n1","name":"Keep","type":"person"},{"id":"n2","name":"Merge","type":"thing"},{"id":"n3","name":"Third","type":"person"}],"memories":[{"id":"m1","kind":"fact","body":"visible","about":"n2","hide_from":["n2"]},{"id":"m2","kind":"view","body":"same"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	h := namesHandler(pipe, db, nil)
	beforeKeys := map[string]map[string]string{}
	for _, model := range []string{"flat-test", "voyage-context-3"} {
		keys, err := embed.Documents(db, model, []int64{j.ID})
		if err != nil {
			t.Fatal(err)
		}
		beforeKeys[model] = keys
	}
	preview := callMem(t, namesPreviewHandler(db), "POST", "/names/merge/preview", fmt.Sprintf(`{"chat_id":"U3","keep":%q,"merge":%q}`, ids["entities/0"], ids["entities/1"]))
	if preview.Code != 200 {
		t.Fatal(preview.Raw)
	}
	names := callMem(t, h, "GET", "/names?chat_id=U3", "")
	t.Log("C17_REVIEW_NAMES=" + names.Raw)
	t.Log("C17_REVIEW_PREVIEW=" + preview.Raw)
	if a := callMem(t, h, "POST", "/names/merge", fmt.Sprintf(`{"chat_id":"U3","keep":%q,"merge":%q}`, ids["entities/0"], ids["entities/1"])); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	names = callMem(t, h, "GET", "/names?chat_id=U3", "")
	var result struct{ Merges []store.NameMerge }
	json.Unmarshal([]byte(names.Raw), &result)
	if len(result.Merges) != 1 {
		t.Fatal("merge IDs missing", names.Raw)
	}
	op := result.Merges[0].ID
	undoHandler := namesHandler(nil, db, nil) // no session/list loader on undo
	wrong := callMem(t, undoHandler, "POST", "/names/unmerge", fmt.Sprintf(`{"chat_id":"OTHER","merge_id":%d}`, op))
	if wrong.Code != 404 || !strings.Contains(wrong.Raw, "no_record") {
		t.Fatal(wrong.Raw)
	}
	if a := callMem(t, undoHandler, "POST", "/names/unmerge", fmt.Sprintf(`{"chat_id":"U3","merge_id":%d}`, op)); a.Code != 200 {
		t.Fatal(a.Raw)
	}
	known, err := db.KnownFor([]memory.Source{srcs[0]})
	if err != nil || known.Memories[0].About != ids["entities/1"] {
		t.Fatal("HTTP undo failed", known, err)
	}
	for model, before := range beforeKeys {
		keys, err := embed.Documents(db, model, []int64{j.ID})
		if err != nil || !reflect.DeepEqual(before, keys) {
			t.Fatal("undo did not restore reusable document keys", model, keys, err)
		}
	}
	d, err := db.Deliver([]memory.ScopedTurn{{Source: srcs[0]}}, store.DeliveryRequest{Input: "Merge visible", Payload: []store.TextPrint{{Role: "user"}}, Chars: 1000, Secret: 1000})
	if err != nil || !d.SecretIDs[ids["memories/0"]] || !strings.Contains(d.RetainedBlock, "Merge") || strings.Contains(d.Block, "Keep") {
		t.Fatal("undo delivery/protection", d, err)
	}
}

// An index run commits doc_hash on the same memory rows; merge and unmerge
// must run between worker runs or MariaDB rejects them with 1020 (11-06
// upgrade test). A run holding that point (here a maintenance callback,
// the same lock) keeps both waiting.
func TestC17MergeAndUnmergeWaitForWorkerRuns(t *testing.T) {
	db, pipe, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"entities":[{"id":"n1","name":"Keep"},{"id":"n2","name":"Merge"}],"memories":[{"id":"m1","kind":"fact","body":"visible","about":"n2"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	worker := generate.NewWorker(db, nil, "", time.Minute)
	h := namesHandler(pipe, db, worker)
	whileHeld := func(path, body string) {
		t.Helper()
		held, release := make(chan struct{}), make(chan struct{})
		go worker.Maintain(func() error { close(held); <-release; return nil })
		<-held
		done := make(chan memAnswer, 1)
		go func() { done <- callMem(t, h, "POST", path, body) }()
		select {
		case a := <-done:
			t.Fatalf("%s ran while a worker run held the work point: %d", path, a.Code)
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		select {
		case a := <-done:
			if a.Code != 200 {
				t.Fatalf("%s after the run: %s", path, a.Raw)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never ran after the run ended", path)
		}
	}
	whileHeld("/names/merge", fmt.Sprintf(`{"chat_id":"U3","keep":%q,"merge":%q}`, ids["entities/0"], ids["entities/1"]))
	ops, err := db.NameMerges("chat:U3")
	if err != nil || len(ops) != 1 {
		t.Fatal(ops, err)
	}
	known, err := db.KnownFor([]memory.Source{srcs[0]})
	if err != nil || known.Memories[0].About != ids["entities/0"] {
		t.Fatal("merge not applied", known, err)
	}
	whileHeld("/names/unmerge", fmt.Sprintf(`{"chat_id":"U3","merge_id":%d}`, ops[0].ID))
	known, err = db.KnownFor([]memory.Source{srcs[0]})
	if err != nil || known.Memories[0].About != ids["entities/1"] {
		t.Fatal("undo not applied", known, err)
	}
}
