package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

func TestMemoryDetailHTTPCold(t *testing.T) {
	if os.Getenv("AC2_C15_CHILD") == "1" {
		db, err := store.Open(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_models?parseTime=true")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		// nil pipeline/worker is deliberate: this read must never load a
		// whole session, warm the cache, generate or enqueue vector work.
		a := callMem(t, memoriesHandler(nil, db, nil, nil), "GET", os.Getenv("AC2_C15_PATH"), "")
		if a.Code != 200 {
			t.Fatalf("cold: %d %s", a.Code, a.Raw)
		}
		t.Log("C15_COLD_JSON=" + strings.TrimSpace(a.Raw))
		return
	}
	db := modelsDB(t)
	pipe := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "원본 입력"), memRow(1, "char", "a1", "g1", "원본 응답"), memRow(2, "user", "u2", "", "다음 입력")}
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "C15", rows...)}); err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	if err := pipe.WithSession("chat:C15", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.ActiveTurns()[0]) }); err != nil {
		t.Fatal(err)
	}
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"entities":[{"id":"n1","name":"비밀 보호 이름"}],"memories":[{"id":"m1","kind":"event","body":"비밀 기억 본문","q":"모델 인용","hide_from":["n1"]}]}`
	o, notes := memory.Parse(raw)
	if len(notes) != 0 {
		t.Fatal(notes)
	}
	if _, err = db.SaveGeneration(j, store.Generation{Request: "모델 요청", Response: raw, Output: o}); err != nil {
		t.Fatal(err)
	}
	it, err := db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	h := memoriesHandler(nil, db, nil, nil)
	url := "/memory/detail?chat_id=C15&turn_id=" + strconv.Itoa(src.Turn) + "&id=" + it.Memories[0].ID + "&limit=1"
	a := callMem(t, h, "GET", url, "")
	if a.Code != 200 || a.V["secret"] != true || a.V["turn_id"] != float64(src.Turn) {
		t.Fatalf("detail: %d %s", a.Code, a.Raw)
	}
	var want store.MemoryDetail
	if err = json.Unmarshal([]byte(a.Raw), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want.Source, src) || want.Memory.ID != it.Memories[0].ID || want.Labels[it.Names[0].ID] != it.Names[0].Name {
		t.Fatalf("source/raw/labels: %s", a.Raw)
	}
	for _, c := range []struct {
		url     string
		code    int
		message string
	}{
		{"/memory/detail?turn_id=1&id=M1", 400, "chat_required"},
		{"/memory/detail?chat_id=C15&id=M1", 400, "bad_request"},
		{"/memory/detail?chat_id=C15&turn_id=1&id=M999999", 404, "memory_not_found"},
		{strings.Replace(url, "chat_id=C15", "chat_id=OTHER", 1), 404, "memory_not_found"},
		{url + "&cursor=bad", 400, "bad_cursor"},
	} {
		v := callMem(t, h, "GET", c.url, "")
		if v.Code != c.code || !strings.Contains(v.Raw, c.message) {
			t.Fatalf("bad request: %s => %d %s", c.url, v.Code, v.Raw)
		}
	}
	// Exercise the registered public-server auth owner with synthetic
	// credentials only. The detail route has no separate token mechanism.
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.Routes = map[string]http.HandlerFunc{"/memory/detail": h}
	srv.Token, _, err = observe.LoadToken(filepath.Join(t.TempDir(), "c15-synthetic-token"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := srv.Token.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "wrong", token} {
		r := httptest.NewRequest("GET", url, nil)
		r.Header.Set(observe.TokenHeader, value)
		w := httptest.NewRecorder()
		srv.Public().ServeHTTP(w, r)
		if value == token {
			if w.Code != 200 || !strings.Contains(w.Body.String(), "비밀 기억 본문") {
				t.Fatal("authorized management detail lost")
			}
		} else if w.Code != 401 || strings.Contains(w.Body.String(), "비밀") {
			t.Fatal("unauthorized detail leak")
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMemoryDetailHTTPCold$", "-test.v")
	cmd.Env = append(os.Environ(), "AC2_C15_CHILD=1", "AC2_C15_PATH="+url)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cold child: %v %s", err, output)
	}
	var got store.MemoryDetail
	found := false
	for _, line := range strings.Split(string(output), "\n") {
		if _, raw, ok := strings.Cut(line, "C15_COLD_JSON="); ok {
			if err = json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("new-process cold detail changed: %s", output)
	}
}
