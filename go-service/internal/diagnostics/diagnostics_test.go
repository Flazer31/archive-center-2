package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// tokenShaped has the access-token form so masking is tested; built from
// two parts so the source never holds a literal token-shaped value.
var tokenShaped = "ac2-" + "abcdef-ghijkl-mnopqr-stuvwx"

func ownedTemp(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "_diag02-owned", "backend-tests")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestRedactSharedCredentialForms(t *testing.T) {
	secrets := []string{"opaque-known-value", tokenShaped, "sk-private-secret-key", "private-api", "private-dsn", "private-password", "private-bearer", "private-sql", "private-url"}
	input := `opaque-known-value ` + tokenShaped + ` sk-private-secret-key api_key=private-api user:private-dsn@tcp(localhost:1)/db password="private-password" Authorization: Bearer private-bearer IDENTIFIED BY 'private-sql' https://user:private-url@example.test/path`
	got := Redact(input, secrets[0])
	for _, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Fatalf("unredacted synthetic credential %q", secret)
		}
	}
	// Version and package path names are not tokens.
	path := `_ac2-004-wt/bin/ac2-updater.exe`
	if Redact(path) != path {
		t.Fatal("package path was mistaken for an access token")
	}
}

func TestRedactQuotedCredentialsAndSpacedDSN(t *testing.T) {
	text := `password="private password with spaces" token='private token with spaces' api_key="private \"quoted\" key" user:private dsn password@tcp(localhost:1)/db`
	got := Redact(text)
	for _, marker := range []string{"private", "with spaces", "quoted", "dsn password"} {
		if strings.Contains(got, marker) {
			t.Fatalf("quoted credential suffix leaked: %q", got)
		}
	}
}

func TestRedactDSNPreservesTimestampAndStage(t *testing.T) {
	for _, dsn := range []string{"user:private dsn password@tcp(localhost:1)/db", "123:private@dsn:password@tcp(localhost:1)/db", "root:private@dsn@unix(/tmp/socket)/db"} {
		got := Redact("2026/11/04 12:00:00 new startup " + dsn + "\n")
		if !strings.Contains(got, "2026/11/04 12:00:00 new startup") || strings.Contains(got, "private") {
			t.Fatalf("timestamp/stage lost or DSN leaked: %q", got)
		}
	}
}

// The report holds the newest LogFiles (3) start logs plus mariadb.log, each
// bounded to its last TailBytes (4 MiB); other files in the data directory
// are never read, and credentials are masked.
func TestNewReportKnownFilesOnlyAndBoundedTails(t *testing.T) {
	data := ownedTemp(t)
	if err := os.MkdirAll(filepath.Join(data, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"logs/start-oldest.log": "fourth newest start must not be read\n",
		"logs/start-old2.log":   "2026/11/04 11:00:00 older start two\n",
		"logs/start-old1.log":   "2026/11/04 11:30:00 older start one\n",
		"logs/start-new.log":    strings.Repeat("prefix discarded\n", TailBytes/8) + "2026/11/04 12:00:00 new startup " + tokenShaped + "\n",
		"mariadb.log":           strings.Repeat("행\n", TailBytes/2) + "2026-11-04 12:00:00 password=private-maria\n",
		"token.txt":             "credential-file-marker", "mariadb.dsn": "dsn-file-marker", "secret.key": "key-file-marker",
		"memories.json": "memory-original-marker", "observations.jsonl": "conversation-original-marker", "logs/other.log": "prompt-original-marker",
	} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, age := range map[string]time.Duration{"logs/start-oldest.log": 3 * time.Hour, "logs/start-old2.log": 2 * time.Hour, "logs/start-old1.log": time.Hour} {
		at := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(data, name), at, at); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReport(data, "Beta test", time.Now().Add(-time.Minute))
	// three newest start logs (newest first) + mariadb.log
	if len(r.Files) != LogFiles+1 || LogFiles != 3 || r.UptimeSeconds < 59 {
		t.Fatalf("runtime/log selection: %+v", r)
	}
	for i, want := range []string{"logs/start-new.log", "logs/start-old1.log", "logs/start-old2.log", "mariadb.log"} {
		if r.Files[i].Name != want {
			t.Fatalf("file %d is %q, want %q", i, r.Files[i].Name, want)
		}
	}
	for _, f := range r.Files {
		big := f.Name == "logs/start-new.log" || f.Name == "mariadb.log"
		if f.Truncated != big || len(f.Text) > TailBytes || !utf8.ValidString(f.Text) {
			t.Fatalf("tail bounds: %s truncated=%v len=%d", f.Name, f.Truncated, len(f.Text))
		}
	}
	encoded, _ := json.Marshal(r)
	for _, marker := range []string{"fourth newest start", "credential-file-marker", "dsn-file-marker", "key-file-marker", "memory-original-marker", "conversation-original-marker", "prompt-original-marker", tokenShaped, "private-maria"} {
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("report leaked forbidden synthetic input %q", marker)
		}
	}
	if !strings.Contains(r.Files[0].Text, "new startup [접속 정보 가림]") || !strings.Contains(r.Files[1].Text, "older start one") || !strings.Contains(r.Files[2].Text, "older start two") {
		t.Fatal("useful newest tails missing or the token was not masked in place")
	}
	if !strings.Contains(r.Files[3].Text, "password=[가림]") {
		t.Fatal("mariadb.log tail lost its masked password record")
	}
}

func TestReportMissingAndOversizeSingleLine(t *testing.T) {
	data := ownedTemp(t)
	r := NewReport(filepath.Join(data, "missing"), "test", time.Time{})
	if len(r.Files) != 2 || r.Files[0].Error == "" || r.Files[1].Error == "" {
		t.Fatal("missing logs not reported")
	}
	if err := os.WriteFile(filepath.Join(data, "mariadb.log"), []byte(strings.Repeat("x", TailBytes*2)), 0600); err != nil {
		t.Fatal(err)
	}
	logs := CollectLogs(data)
	if !logs[1].Truncated || logs[1].Text != "" {
		t.Fatal("partial long line could expose a cut credential")
	}
}

// Since user decision 11-08 the error text is shown whole; only credential
// values are masked and very long text is cut at ErrorLimit.
func TestErrorSummaryKeepsResponseBodiesMasksCredentials(t *testing.T) {
	secret := "sk-" + "synthetic-credential-value"
	got := ErrorSummary(`embedding: provider 429: {"prompt":"private-conversation-marker","api_key":"` + secret + `"} Authorization: Bearer bearer-secret-value`)
	for _, want := range []string{"provider 429", "private-conversation-marker", "[가림]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary lost %q: %q", want, got)
		}
	}
	for _, leaked := range []string{secret, "bearer-secret-value"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("summary exports credential %q: %q", leaked, got)
		}
	}
	if !strings.Contains(ErrorSummary("SQL failed around private-memory-marker"), "private-memory-marker") {
		t.Fatal("SQL excerpt hidden")
	}
	long := ErrorSummary(strings.Repeat("가", ErrorLimit+500))
	if r := []rune(long); len(r) != ErrorLimit+1 || r[ErrorLimit] != '…' {
		t.Fatalf("long error not cut at ErrorLimit: %d runes", len(r))
	}
}

// Since 11-08 the report shows runtime error payloads, SQL excerpts, chat
// labels and continuation lines as logged; only credential values are
// masked. Bounded tails still start at a complete line.
func TestCollectLogsKeepsRuntimeErrorPayloadsMasksCredentials(t *testing.T) {
	data := ownedTemp(t)
	if err := os.MkdirAll(filepath.Join(data, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	secret := "sk-" + "synthetic-log-credential"
	text := "2026/11/04 12:00:00 [시작] 백엔드 연결 준비\n" +
		"2026/11/04 12:00:01 memory delivery: query vector: provider 429: {\"prompt\":\"private-conversation\",\n\"memory\":\"private-memory\"}\n" +
		"2026/11/04 12:00:02 memory delivery: keep trace: SQL failed: private-sql-memory api_key=" + secret + "\n" +
		"2026/11/04 12:00:03 chat label private-chat-id: private-label-echo\n" +
		"2026/11/04 12:00:04 [준비 완료] 백엔드가 실행 중입니다.\n"
	if err := os.WriteFile(filepath.Join(data, "logs/start-test.log"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "mariadb.log"), []byte("2026-11-04 12:00:00 Error: Query: INSERT INTO memories VALUES ('private-db-memory')\nSQL continuation private-db-more\n2026-11-04 12:00:01 ready for connections\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := CollectLogs(data)
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("credential in a runtime log leaked")
	}
	for _, marker := range []string{"private-conversation", "private-memory", "private-sql-memory", "private-chat-id", "private-label-echo", "private-db-memory", "private-db-more", "api_key=[가림]"} {
		if !strings.Contains(string(encoded), marker) {
			t.Fatalf("runtime log text %q was dropped", marker)
		}
	}
	if !strings.Contains(r[0].Text, "provider 429") || !strings.Contains(r[0].Text, "[시작]") || !strings.Contains(r[0].Text, "[준비 완료]") || !strings.Contains(r[1].Text, "ready for connections") {
		t.Fatal("useful stage/status evidence lost")
	}
	// A tail cut inside a line drops that partial first line only; every
	// later complete line (even without a timestamp) is kept.
	text = strings.Repeat("x", TailBytes) + "\nleading-multiline-line\n2026/11/04 12:00:05 memory delivery: recent turn similarity: similarity-echo\n2026/11/04 12:00:06 scene contrast skipped: scene-echo\n2026/11/04 12:00:07 [준비 완료] 실행 중\n"
	if err := os.WriteFile(filepath.Join(data, "logs/start-test.log"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	r = CollectLogs(data)
	if !r[0].Truncated || strings.Contains(r[0].Text, "xxxx") {
		t.Fatalf("partial first line was kept: truncated=%v", r[0].Truncated)
	}
	for _, marker := range []string{"leading-multiline-line", "similarity-echo", "scene-echo", "[준비 완료]"} {
		if !strings.Contains(r[0].Text, marker) {
			t.Fatalf("complete line after the cut lost: %q", marker)
		}
	}
}
