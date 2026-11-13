package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

type downModel struct{}

func (downModel) Chat(context.Context, string) (provider.Result, error) {
	return provider.Result{}, errors.New("provider 429: rate limited")
}

// Bug report 11-12: a cold start waiting after a failed call showed as
// running. The progress says it waits, with the next call's time, and is
// not running.
func TestBackfillProgressShowsRetryWait(t *testing.T) {
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, _ := sql.Open("mysql", base)
	boot.Exec("DROP DATABASE IF EXISTS ac2_test_backfill_retry")
	if _, err := boot.Exec("CREATE DATABASE ac2_test_backfill_retry CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE ac2_test_backfill_retry"); boot.Close() })
	db, err := store.Open(base + "ac2_test_backfill_retry?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pipe := pipeline.New(db)
	worker := generate.NewWorker(db, downModel{}, "down", time.Minute)
	worker.Retries = 2
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	row := func(i int, role, id, text string) string {
		b, _ := json.Marshal(map[string]any{"i": i, "role": role, "chat_id": id, "data": map[string]any{"text": text}})
		return string(b)
	}
	ev := `{"boot_id":"b","seq":1,"hook":"load","stored_chat":{"chat_id":"R","rows":[` + strings.Join([]string{
		row(0, "user", "u1", "하나"), row(1, "char", "a1", "첫째"), row(2, "user", "u2", "둘"), row(3, "char", "a2", "둘째"), row(4, "user", "u3", "셋"), row(5, "char", "a3", "셋째")}, ",") + `]}}`
	if _, err := pipe.Process([]observe.Record{{Event: json.RawMessage(ev)}}); err != nil {
		t.Fatal(err)
	}
	h := backfillHandler(pipe, worker, db)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/memory/backfill", strings.NewReader(`{"chat_id":"R","max_turns":0,"order":"oldest"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("post %d %s", rec.Code, rec.Body.String())
	}
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/memory/backfill?chat_id=R", nil))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	var p map[string]any
	// the run ends just after it sets the wait: read once it has ended
	for i := 0; i < 100; i++ {
		if p = get(); p["retry_waiting"] == true && p["running"] == false {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	at, _ := time.Parse(time.RFC3339, func() string { s, _ := p["retry_at"].(string); return s }())
	if p["retry_waiting"] != true || p["running"] != false || p["waiting"] != float64(3) || at.Before(time.Now().Add(30*time.Second)) {
		t.Fatalf("progress while waiting after a failed call: %v", p)
	}
}
