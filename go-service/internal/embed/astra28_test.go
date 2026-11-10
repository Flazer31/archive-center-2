package embed

import (
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestA28PriorNameStableAcrossIndexBatch(t *testing.T) {
	db := testDB(t)
	first := saveTurn(t, db, `{"entities":[{"id":"n1","name":"민재"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"주인공"}]}`)
	k, err := db.KnownOf([]int64{first})
	if err != nil {
		t.Fatal(err)
	}
	id := k.Names[0].ID
	src := memory.Source{Session: "chat:E", Turn: 2, Members: []memory.SourceMember{{Key: "u2", Data: "다음"}}, Responses: []memory.SourceResponse{{Key: "a2", Seq: 1, Data: "돈 20냥"}}}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	var o memory.Output
	json.Unmarshal([]byte(`{"memories":[{"id":"m1","kind":"fact","about":"`+id+`","slot":"돈","body":"20냥"}]}`), &o)
	if _, err = db.SaveGeneration(j, store.Generation{Output: o}); err != nil {
		t.Fatal(err)
	}
	f := &fakeVoyage{}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "fake", Key: "test-key", HTTP: srv.Client()}
	one, err := Index(context.Background(), db, v, "fake", []int64{j.ID}, 64)
	if err != nil {
		t.Fatal(err)
	}
	single := f.requests[0]["input"].([]any)[0].(string)
	all, err := Index(context.Background(), db, v, "fake", []int64{first, j.ID}, 64)
	if err != nil {
		t.Fatal(err)
	}
	texts := f.requests[1]["input"]
	t.Logf("single_job_document=%q first_report=%+v all_jobs_report=%+v all_jobs_documents=%v", single, one, all, texts)
	if !strings.Contains(single, "민재") {
		t.Fatal("per-turn indexing cannot resolve a stored earlier name; same memory gets another hash in backlog batch")
	}
}

func TestA28DocTextPreservesOnlyDistinctRetrievalCues(t *testing.T) {
	names := map[string]string{"N1": "민재", "N2": "서진"}
	cases := []struct {
		name string
		a, b memory.Memory
	}{
		{"terms", memory.Memory{Kind: "fact", Mode: "의무", About: "N1", Body: "갚기로 했다", Terms: "비가 오면 내일 쌀로 갚는다"}, memory.Memory{Kind: "fact", Mode: "의무", About: "N1", Body: "갚기로 했다", Terms: "맑으면 내달 돈으로 갚는다"}},
		{"setting_roles", memory.Memory{Kind: "event", Body: "이야기했다", Setting: "흐린 날", Roles: &memory.Roles{Speaker: []string{"N1"}}}, memory.Memory{Kind: "event", Body: "이야기했다", Setting: "맑은 날", Roles: &memory.Roles{Speaker: []string{"N2"}}}},
		{"status_effect", memory.Memory{Kind: "event", Body: "약속에 관한 일", Effects: []memory.Effect{{About: "M1", Status: "fulfilled"}}}, memory.Memory{Kind: "event", Body: "약속에 관한 일", Effects: []memory.Effect{{About: "M1", Status: "refused"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := memory.DocText(memory.KnownMemory{Memory: c.a}, names)
			b := memory.DocText(memory.KnownMemory{Memory: c.b}, names)
			t.Logf("left=%q right=%q same_hash=%v", a, b, store.TextHash(a) == store.TextHash(b))
			if a == b {
				t.Error("distinct contract content has identical vector document")
			}
		})
	}
}

func TestA28ProtectedMemoryKeepsCanonicalAwareness(t *testing.T) {
	db := testDB(t)
	job := saveTurn(t, db, `{"entities":[{"id":"n1","name":"민재"},{"id":"n2","name":"서진"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"숨긴 금고 암호는 파랑","hide_from":["n2"]}]}`)
	f := &fakeVoyage{}
	srv := f.server(t)
	defer srv.Close()
	v := provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "fake", Key: "test-key", HTTP: srv.Client()}
	if _, e := Index(context.Background(), db, v, "fake", []int64{job}, 64); e != nil {
		t.Fatal(e)
	}
	k, e := db.KnownOf([]int64{job})
	if e != nil {
		t.Fatal(e)
	}
	text := f.requests[0]["input"].([]any)[0].(string)
	t.Logf("provider_document=%q canonical_awareness=%+v", text, k.Awareness)
	if len(k.Awareness) != 1 || k.Awareness[0].Kind != "hide_from" {
		t.Fatal("index erased canonical protection")
	}
}
