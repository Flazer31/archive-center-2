package main

import (
	"bytes"
	"database/sql"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

func checkStoreFailureLog(t *testing.T, h http.HandlerFunc, method, path, body, cause string) {
	t.Helper()
	var output bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(saved)
	got := callMem(t, h, method, path, body)
	if got.Code != http.StatusInternalServerError || got.V["code"] != "store_failed" || strings.Contains(got.Raw, cause) {
		t.Fatalf("response %d %s", got.Code, got.Raw)
	}
	if !strings.Contains(output.String(), "store_failed: ") || !strings.Contains(output.String(), cause) {
		t.Fatalf("log %q, want store_failed with %q", output.String(), cause)
	}
}

func TestChatOperationFailureLogs(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	// A cached chat enters the operation callback. An unseen chat needs a
	// session read first, which exercises the outer error on GET.
	u2LoadHTTP(t, pipe, "known", 1)
	db.Close()
	for _, tc := range []struct {
		name, method, path, body string
		h                        http.HandlerFunc
	}{
		{"attach session", "GET", "/chats/attach?chat_id=new", "", attachHandler(pipe, db, nil)},
		{"attach operation", "POST", "/chats/attach", `{"chat_id":"known","detach":true}`, attachHandler(pipe, db, nil)},
		{"copy operation", "POST", "/chats/copy", `{"from":"known","to":"other"}`, chatOpsHandler(pipe, db, nil, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkStoreFailureLog(t, tc.h, tc.method, tc.path, tc.body, "database is closed")
		})
	}
}

func TestHypaOperationFailureLogs(t *testing.T) {
	for _, table := range []string{"gen_grants", "session_past"} {
		t.Run(table, func(t *testing.T) {
			db := modelsDB(t)
			conn, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			cause := "test write failed in " + table
			if _, err := conn.Exec("CREATE TRIGGER fail_write BEFORE INSERT ON " + table + " FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='" + cause + "'"); err != nil {
				t.Fatal(err)
			}
			pipe := pipeline.New(db)
			srv := observe.NewServer(store.RawSink{DB: db})
			srv.After = pipe
			h := hypaHandler(pipe, generate.NewWorker(db, nil, "", 0), db, srv)
			checkStoreFailureLog(t, h, "POST", "/memory/hypa", `{"chat_id":"X","summaries":[{"text":"one summary"}]}`, cause)
		})
	}
}
