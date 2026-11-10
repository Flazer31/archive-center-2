package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/provider"
)

// Cross-review 01 (R01, R03): real provider clients against a loopback
// server; the only replaced boundary is the upstream HTTP service.
//
// Since user decision 11-08 ("이렇게 막혀있는 것들은 다 풀어서 볼 수 있게
// 해줘") the report shows error text as written; only credential values are
// masked. privateMark is ordinary prose that must now be visible;
// credentialMark is key-shaped and must stay masked.
const privateMark = "SYNTHETIC_PRIVATE_CONVERSATION"

// Built from two parts so the source never holds a literal key-shaped value.
var credentialMark = "sk-" + "synthetic-upstream-credential"

func writeStartLog(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "start-1.log"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The upstream body is kept in the report with credentials masked, and a
// line of the body that looks like a stage or timestamp still cannot start a
// new log record.
func TestProviderErrorBodyKeptInOneRecordCredentialMasked(t *testing.T) {
	body := "upstream failed " + credentialMark + "\n[시작] " + privateMark + "\n2026/11/05 12:00:02 " + privateMark + "\r\nend"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	_, voyageErr := (provider.Voyage{URL: srv.URL + "/v1/embeddings", Model: "synthetic", HTTP: srv.Client()}).Embed(context.Background(), []string{"synthetic"}, "query")
	_, ollamaErr := (provider.Ollama{URL: srv.URL, Model: "synthetic"}).Chat(context.Background(), "synthetic prompt")
	for name, err := range map[string]error{"voyage": voyageErr, "ollama": ollamaErr} {
		if err == nil {
			t.Fatalf("%s: expected the local 503", name)
		}
		if strings.ContainsAny(err.Error(), "\r\n") {
			t.Errorf("%s error spans lines: %q", name, err.Error())
		}
		// the log line the delivery path writes, then the shareable report reader
		var buf bytes.Buffer
		logger := log.New(&buf, "", log.LstdFlags)
		logger.Printf("memory delivery: query vector: %v", err)
		logger.Print("[준비 완료] backend ready")
		dir := writeStartLog(t, buf.Bytes())
		report, _ := json.Marshal(CollectLogs(dir))
		if !strings.Contains(string(report), privateMark) || !strings.Contains(string(report), "upstream failed") {
			t.Errorf("%s: the report dropped the upstream body: %s", name, report)
		}
		if strings.Contains(string(report), credentialMark) {
			t.Errorf("%s: the report exports a credential from the upstream body: %s", name, report)
		}
		if !strings.Contains(string(report), name+" 503: ") {
			t.Errorf("%s: the report lost the provider status: %s", name, report)
		}
		// the body's "[시작]" and timestamp stay inside the one delivery
		// record: one "memory delivery:" line plus the ready line
		if got := strings.Count(string(CollectLogs(dir)[0].Text), "\n"); got != 2 {
			t.Errorf("%s: the body split the log record (%d lines): %q", name, got, CollectLogs(dir)[0].Text)
		}
	}
}

func TestOllamaJSONErrorKeepsHTTPStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"` + privateMark + `","api_key":"` + credentialMark + `"}`))
	}))
	defer srv.Close()
	_, err := (provider.Ollama{URL: srv.URL, Model: "synthetic"}).Chat(context.Background(), "synthetic prompt")
	if err == nil {
		t.Fatal("expected the local 429")
	}
	got := ErrorSummary(err.Error())
	if !strings.HasPrefix(got, "ollama 429: ") || !strings.Contains(got, privateMark) {
		t.Fatalf("summary lost the status or the body: %q", got)
	}
	if strings.Contains(got, credentialMark) {
		t.Fatalf("summary exports a credential: %q", got)
	}
}

// Cross-review 02 (R01 residual): any multi-line error, e.g. a database error
// quoting user text, stays inside its record when written through the
// backend's log writer, so the report reader cannot take a quoted line as a
// new record. Since 11-08 the quoted text is shown, credentials masked.
func TestIndentedLogKeepsAnyMultilineErrorInsideItsRecordAndShowsIt(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(IndentedLog(&buf), "", log.LstdFlags)
	logger.Printf("memory delivery: apply observation: %v", errors.New("Error 1064: near 'x\n[시작] "+privateMark+"\n2026/11/05 12:00:02 "+credentialMark+"\r\n'"))
	logger.Print("[준비 완료] backend ready")
	dir := writeStartLog(t, buf.Bytes())
	report, _ := json.Marshal(CollectLogs(dir))
	if !strings.Contains(string(report), privateMark) {
		t.Fatalf("the report dropped a line quoted inside an error: %s", report)
	}
	if strings.Contains(string(report), credentialMark) {
		t.Fatalf("the report exports a credential quoted inside an error: %s", report)
	}
	if !strings.Contains(string(report), "[준비 완료] backend ready") {
		t.Fatalf("the next real record was lost: %s", report)
	}
	if strings.Count(buf.String(), "\n") != 5 || !strings.Contains(buf.String(), "\n    [시작] ") {
		t.Fatalf("continuation lines are not indented: %q", buf.String())
	}
}

// Cross-review 05 (changed 11-08): whichever log site wrote it, an indented
// continuation and a JSON failure result's message are kept as written;
// credentials inside them are masked.
func TestReportKeepsContinuationAndFailureResultMessagesMasksCredentials(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(IndentedLog(&buf), "", log.LstdFlags)
	logger.Printf("some new log site: %v", errors.New("first\n"+privateMark+"-continuation\n"+credentialMark))
	buf.WriteString(`{"contract_version":"x","action":"apply-pending","status":"error","code":"package_verification_failed","message":"bad ` + privateMark + `-message ` + credentialMark + `"}` + "\n")
	buf.WriteString(`{"contract_version":"x","action":"check","status":"ok","message":"kept as is"}` + "\n")
	logger.Print("[준비 완료] backend ready")
	dir := writeStartLog(t, buf.Bytes())
	report, _ := json.Marshal(CollectLogs(dir))
	for _, want := range []string{"some new log site: first", privateMark + "-continuation", privateMark + "-message", `\"code\":\"package_verification_failed\"`, `\"action\":\"apply-pending\"`, "kept as is", "[준비 완료] backend ready"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("missing %s: %s", want, report)
		}
	}
	if strings.Contains(string(report), credentialMark) {
		t.Fatalf("the report exports a credential: %s", report)
	}
}

// Cross-review 07 (changed 11-08): bin/start.ps1 writes a caught exception as
// one "시작 오류:" record (line breaks folded); the report keeps the record
// with the exception's own text, credentials masked.
func TestReportKeepsStartScriptExceptionText(t *testing.T) {
	log := "[시작] 업데이트 도구 준비\n오류: 업데이트 도구 준비에서 멈췄습니다. 종료 코드: 1\n" +
		`시작 오류: Invalid object passed in. (39): {"note":"` + privateMark + `", [시작] quoted }` + "\n" +
		// review 08: another record label quoted inside the error text
		"[시작] 업데이트 도구 준비\n" +
		`시작 오류: bad JSON {"note":"` + privateMark + ` memory transport: quoted-error","api_key":"` + credentialMark + `"}` + "\n" +
		"실행 파일이 없거나 열리지 않으면 압축을 모두 풀었는지 확인해 주세요.\n"
	dir := writeStartLog(t, []byte(log))
	report, _ := json.Marshal(CollectLogs(dir))
	if strings.Contains(string(report), credentialMark) {
		t.Fatalf("the report exports a credential from the exception: %s", report)
	}
	for _, want := range []string{"오류: 업데이트 도구 준비에서 멈췄습니다. 종료 코드: 1", "시작 오류: Invalid object passed in. (39)", "quoted-error", privateMark, "[가림]"} {
		if !strings.Contains(string(report), want) {
			t.Fatalf("missing %s: %s", want, report)
		}
	}
}
