package store

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestA25GenerationIDEnrichment(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A25")
	u := lrow("u", "user", "", "입력")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, u, lrow("a", "char", "", "본문")))
	before := turnSource(s, 0)
	j, _, _ := a24Save(t, db, before, `{"memories":[{"id":"m1","kind":"fact","body":"사실"}]}`)
	s.Apply(lsnap(lifecycle.SourceLoad, u, lrow("a", "char", "g1", "본문")))
	after := turnSource(s, 0)
	next, err := db.JobFor(after)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before=%+v after=%+v jobs=%d/%d", before, after, j.ID, next.ID)
	if before.Responses[0].Seq != after.Responses[0].Seq || before.Turn != after.Turn {
		t.Fatal("fixture not enrichment")
	}
	if next.ID != j.ID {
		t.Fatal("optional GenerationID enrichment duplicates job")
	}
}

func TestA25GenerationAfterIDlessRevision(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A25")
	u := lrow("u", "user", "", "입력")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, u, lrow("a", "char", "g1", "첫 본문")))
	s.Apply(lsnap(lifecycle.SourceLoad, u, lrow("a", "char", "", "편집 본문")))
	before := turnSource(s, 0)
	a24Save(t, db, before, `{"memories":[{"id":"m1","kind":"fact","body":"편집 사실"}]}`)
	unread := lrow("a", "char", "g2", "")
	unread.Unreadable = true
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, u, unread))
	s.Apply(lsnap(lifecycle.SourceOutputEvent, u, unread))
	after := turnSource(s, 0)
	t.Logf("before=%+v after=%+v", before, after)
	if after.Readable() || after.Responses[0].Data != "" {
		t.Error("known new generation exposes old ID-less revision")
	}
	s.Apply(lsnap(lifecycle.SourceLoad, u, lrow("a", "char", "g2", "편집 본문")))
	late := turnSource(s, 0)
	if late.Responses[0].Seq == before.Responses[0].Seq {
		t.Error("late new generation did not get its own revision")
	}
}

func TestA25KnownNameEvidenceAndLinks(t *testing.T) {
	db := testDB(t)
	j, it, _ := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"entities":[{"id":"n1","name":"민재","q":"첫째 민재"},{"id":"n2","name":"민재","q":"둘째 민재"}],"memories":[{"id":"m1","kind":"event","body":"거래","roles":{"actor":["n1"]}},{"id":"m2","kind":"fact","body":"거래 조건","about":"n1"}],"links":[{"id":"l1","from":"m2","rel":"condition_of","to":"m1","q":"비 오면","hide_from":["n2"]}]}`)
	if len(it.Links) != 1 || it.Names[0].Q == "" {
		t.Fatal("storage fixture lost fields")
	}
	k, err := db.KnownOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	rendered := memory.RenderKnown(memory.Select(k, "민재", nil), memory.Superseded(k))
	t.Logf("render=%s", rendered)
	for _, s := range []string{"첫째 민재", "둘째 민재", "condition_of", it.Links[0].ID, "hide_from"} {
		if !strings.Contains(rendered, s) {
			t.Errorf("stored meaning absent from input: %s", s)
		}
	}
}

func TestA25SceneOrderAfterReverseCompletion(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	early, late := turnSource(s, 0), turnSource(s, 1)
	j2, _, _ := a24Save(t, db, late, `{"scenes":[{"ref":"S1","change":"close"}]}`)
	j1, _, _ := a24Save(t, db, early, `{"scenes":[{"id":"s1","change":"open"}]}`)
	k, err := db.KnownOf([]int64{j1.ID, j2.ID})
	if err != nil {
		t.Fatal(err)
	}
	sel := memory.Select(k, "", nil)
	t.Logf("scenes=%+v current=%q", k.Scenes, sel.Scene)
	if sel.Scene != "" {
		t.Fatal("late close processed before early open because source turn lost")
	}
}

func TestA25AmbiguityPreservesBodiesAndReferences(t *testing.T) {
	db := testDB(t)
	_, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"memories":[{"id":"m1","kind":"fact","body":"첫째"},{"id":"m1","kind":"fact","body":"둘째"}],"links":[{"from":"m1","rel":"same_fact","to":"M99"}]}`)
	if len(it.Memories) != 2 || it.Memories[0].Body != "첫째" || it.Memories[1].Body != "둘째" || it.Links[0].From != "m1" || len(notes) == 0 {
		t.Fatalf("body/ref preservation: %+v %v", it, notes)
	}
	for _, m := range it.Memories {
		if m.ID != m.Memory.ID {
			t.Fatal("own id corrupted")
		}
	}
}

func TestA25EffectWasArraySQLRoundtrip(t *testing.T) {
	db := testDB(t)
	j, it, notes := a24Save(t, db, turnSource(twoTurnSession(), 0), `{"memories":[{"id":"m1","kind":"event","body":"앞선 변화","effects":[{"value":"10"},{"value":"20"}]},{"id":"m2","kind":"event","body":"두 값을 바꿈","effects":[{"value":"30","was":["m1.1","m1.2"]}]}]}`)
	want := memory.Refs{it.Memories[0].ID + ".1", it.Memories[0].ID + ".2"}
	if !reflect.DeepEqual(it.Memories[1].Effects[0].Was, want) || len(notes) != 0 {
		t.Fatalf("detail/notes %+v %v", it, notes)
	}
	var raw string
	if err := db.sql.QueryRow(`SELECT e.was FROM effects e JOIN memories m ON m.id=e.memory_id WHERE m.job_id=? AND m.body='두 값을 바꿈' AND e.k=1`, j.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got memory.Refs
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SQL effect refs %v want %v", got, want)
	}
}
