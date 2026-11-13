package main

import (
	"fmt"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// The memory editor sends only what the user changed. A card drawn before
// another save ended sends the other fields alone, which must keep the saved
// body and importance; body/imp and fields together apply in one call.
func TestMemoryEditSendsOnlyChangedFieldsKeepsSavedBodyAndImportance(t *testing.T) {
	db, _, srcs := u3Fixture(t)
	j := u3Save(t, db, srcs[0], `{"memories":[{"id":"m1","kind":"fact","body":"original body","imp":2,"status":"old"}]}`)
	ids, _ := db.ItemIDs(j.ID)
	h := memoriesHandler(nil, db, nil, nil)
	post := func(p string) {
		t.Helper()
		if a := callMem(t, h, "POST", "/memory/edit", fmt.Sprintf(`{"chat_id":"U3","id":%q,%s}`, ids["memories/0"], p)); a.Code != 200 {
			t.Fatal(p, a.Raw)
		}
	}
	stored := func() memory.Memory {
		t.Helper()
		k, err := db.KnownFor([]memory.Source{srcs[0]})
		if err != nil || len(k.Memories) != 1 {
			t.Fatal(k, err)
		}
		return k.Memories[0].Memory
	}
	imp := func(m memory.Memory) float64 {
		if m.Imp == nil {
			return -1
		}
		return *m.Imp
	}
	post(`"body":"accepted body","imp":7`) // the first editor's save
	post(`"patch":{"status":"new"}`)       // the stale card, body and importance untouched
	if m := stored(); m.Body != "accepted body" || imp(m) != 7 || m.Status != "new" {
		t.Fatalf("a field-only save changed body/importance: %+v", m)
	}
	post(`"imp":0`) // 0 is a value, not "keep"
	if m := stored(); m.Body != "accepted body" || imp(m) != 0 {
		t.Fatalf("importance-only save: %+v", m)
	}
	post(`"body":"typed body","patch":{"status":"later"}`)
	if m := stored(); m.Body != "typed body" || imp(m) != 0 || m.Status != "later" {
		t.Fatalf("body with fields: %+v", m)
	}
}
