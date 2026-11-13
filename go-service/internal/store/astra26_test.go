package store

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"strings"
	"testing"
)

func TestA26BackfillSurvivesReload(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	u := lrow("u", "user", "", "입력")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, u, lrow("a", "char", "", "본문")))
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, u, lrow("a", "char", "", "본문"), lrow("n", "user", "", "다음"))) // the next input confirms
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	src := memory.SourceOf(s.Key, s.ActiveTurns()[0])
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	s.Apply(lsnap(lifecycle.SourceLoad, u, lrow("a", "char", "", "편집")))
	s.Apply(lsnap(lifecycle.SourceLoad, u, lrow("a", "char", "g1", "편집")))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := db.LoadSession(s.Key)
	if err != nil || !ok {
		t.Fatalf("load %v %v", ok, err)
	}
	// only the newest text is kept: the edit, with the generation
	// filled in
	revs := loaded.ActiveTurns()[0].Responses[0].Revisions
	if len(revs) != 1 || revs[0].Data != "편집" {
		t.Fatal(revs)
	}
	for _, r := range revs {
		if r.GenerationID != "g1" {
			t.Fatalf("unfilled revision %+v", r)
		}
	}
	if revs[0].Confirmed || !loaded.ActiveTurns()[0].Confirmed {
		t.Fatalf("confirmation changed %+v", revs)
	}
	enriched := src
	enriched.Responses = append([]memory.SourceResponse(nil), src.Responses...)
	enriched.Responses[0].Gen = "g1"
	again, err := db.JobFor(enriched)
	if err != nil || again.ID != j.ID {
		t.Fatalf("job changed %v %v", again, err)
	}
	t.Logf("revisions=%+v job=%d/%d", revs, j.ID, again.ID)
}

func TestA26StoredEffectProtection(t *testing.T) {
	db := testDB(t)
	j, _, _ := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"entities":[{"id":"n1","name":"민재"},{"id":"n2","name":"서진"}],"memories":[{"id":"m1","kind":"event","body":"몰래 받았다","effects":[{"about":"n1","slot":"돈","value":"100냥"}],"hide_from":["n2"]}]}`)
	k, err := db.KnownOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Awareness) != 1 {
		t.Fatal("protection not stored")
	}
	sel := memory.Select(k, "민재가 웃었다", nil)
	r := memory.RenderKnown(sel, memory.Superseded(k))
	t.Log(r)
	if len(sel.Effects) != 1 || !strings.Contains(r, "hide_from") {
		t.Fatal("SQL-to-KNOWN standalone effect lost protection")
	}
}
