package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// 1.4 P3: an injection that fails with 500 leaves a log line naming the step,
// so the cause can be read later (11-19: 1 of 385 injections failed with no
// backend record). The answer stays the same 500.
func TestInjectFailureIsLogged(t *testing.T) {
	base := os.Getenv("AC2_TEST_MARIADB_DSN")
	if base == "" {
		t.Skip("AC2_TEST_MARIADB_DSN not set")
	}
	boot, _ := sql.Open("mysql", base)
	boot.Exec("DROP DATABASE IF EXISTS ac2_test_injectfail")
	if _, err := boot.Exec("CREATE DATABASE ac2_test_injectfail CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Exec("DROP DATABASE ac2_test_injectfail"); boot.Close() })
	db, err := store.Open(base + "ac2_test_injectfail?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	pipe := pipeline.New(db)
	srv := observe.NewServer(store.RawSink{DB: db})
	srv.After = pipe
	h := injectHandler(srv, pipe, db, func() *provider.Embedder { return nil }, nil)
	db.Close() // every read after this fails
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	body, _ := json.Marshal(map[string]any{"chat_id": "F", "input": "x", "payload": []any{}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/memory/inject", strings.NewReader(string(body))))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), "memory delivery: chat F failed at ") {
		t.Fatalf("no failure log: %q", buf.String())
	}
}
