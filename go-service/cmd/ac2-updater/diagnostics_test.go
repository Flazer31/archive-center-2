package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/diagnostics"
)

// tokenShaped has the access-token form so masking is tested; built from
// two parts so the source never holds a literal token-shaped value.
var tokenShaped = "ac2-" + "abcdef-ghijkl-mnopqr-stuvwx"

func runDiagnosticCommand(t *testing.T, args ...string) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = original }()
	code := run(append([]string{"diagnostics"}, args...))
	writer.Close()
	output, err := io.ReadAll(reader)
	reader.Close()
	if code != 0 || err != nil {
		t.Fatalf("diagnostics code=%d error=%v output=%q", code, err, output)
	}
	return string(output)
}

func readOfflineReport(t *testing.T, data string) (offlineDiagnosticReport, string, string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(data, "diagnostics", "ac2-diagnostics-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("report files=%v error=%v", files, err)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var report offlineDiagnosticReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	return report, string(body), files[0]
}

func writeDiagnosticFixture(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The offline report holds the newest three start logs plus mariadb.log, each
// bounded to its last diagnostics.TailBytes; only credential values are
// masked, and other files of the data directory are never read.
func TestOfflineDiagnosticsProductionCommandPrivacyAndBoundedTails(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "isolated-shared-data")
	t.Setenv("AC2_DATA_DIR", data)
	const rawChat = "SYNTHETIC_PRIVATE_CHAT_DO_NOT_EXPORT"
	const rawMemory = "SYNTHETIC_PRIVATE_MEMORY_DO_NOT_EXPORT"
	const fileToken = "FILE_TOKEN_DO_NOT_READ"
	const encryptionKey = "FILE_ENCRYPTION_KEY_DO_NOT_READ"
	const fileDSN = "FILE_DSN_DO_NOT_READ"
	for name, body := range map[string]string{
		"token.txt": fileToken, "secret.key": encryptionKey, "mariadb.dsn": fileDSN,
		"chat.json": rawChat, "memory.json": rawMemory,
		"install.json":          `{"backend_port":35123,"database_port":35124,"root_password":"INSTALL_ROOT_PRIVATE","app_password":"INSTALL_APP_PRIVATE","chat":"` + rawChat + `","memory":"` + rawMemory + `"}`,
		"logs/start-oldest.log": "FOURTH_STARTUP_NOT_SELECTED\n",
		"logs/start-old2.log":   "OLDER_STARTUP_SELECTED_TWO\n",
		"logs/start-old.log":    "OLDER_STARTUP_SELECTED_ONE\n",
		"logs/other.log":        rawMemory,
		"logs/start-new.log":    strings.Repeat("discarded prefix\n", diagnostics.TailBytes/8) + "2026/11/04 12:00:00 new startup " + tokenShaped + " api_key=sk-synthetic-private-key user:private-dsn@tcp(localhost:1)/db\n",
		"mariadb.log":           strings.Repeat("discarded prefix\n", diagnostics.TailBytes/8) + "2026-11-04 12:00:00 database diagnostic password=private-password Authorization: Bearer private-auth\n",
	} {
		writeDiagnosticFixture(t, filepath.Join(data, filepath.FromSlash(name)), body)
	}
	for name, age := range map[string]time.Duration{"start-oldest.log": 3 * time.Hour, "start-old2.log": 2 * time.Hour, "start-old.log": time.Hour} {
		at := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(data, "logs", name), at, at); err != nil {
			t.Fatal(err)
		}
	}
	// The offline command must neither append to nor create a startup log.
	untouchedLog := filepath.Join(root, "unused-start.log")
	t.Setenv("AC2_START_LOG", untouchedLog)
	output := runDiagnosticCommand(t, "--root", root)
	report, body, name := readOfflineReport(t, data)
	if !strings.Contains(output, name) {
		t.Fatalf("saved path not displayed: %q", output)
	}
	if _, err := os.Stat(untouchedLog); !os.IsNotExist(err) {
		t.Fatal("offline export touched AC2_START_LOG", err)
	}
	for _, private := range []string{rawChat, rawMemory, fileToken, encryptionKey, fileDSN,
		"INSTALL_ROOT_PRIVATE", "INSTALL_APP_PRIVATE", "FOURTH_STARTUP_NOT_SELECTED",
		tokenShaped, "sk-synthetic-private-key", "private-dsn", "private-password", "private-auth"} {
		if strings.Contains(body, private) {
			t.Errorf("private content leaked: %s", private)
		}
	}
	if report.Contract != "ac2.diagnostics.v1" || report.Collector != "updater_offline" || report.OS == "" || report.Arch == "" || report.Version == "" {
		t.Fatalf("missing runtime metadata: %+v", report)
	}
	if report.StartedAt != nil || report.Install != (diagnosticInstall{Status: "available", BackendPort: 35123, DatabasePort: 35124}) {
		t.Fatalf("offline/install metadata=%+v", report)
	}
	// three newest start logs (newest first) + mariadb.log
	if len(report.Files) != 4 {
		t.Fatalf("files=%d", len(report.Files))
	}
	for i, file := range report.Files {
		big := file.Name == "logs/start-new.log" || file.Name == "mariadb.log"
		if file.Truncated != big || len(file.Text) > diagnostics.TailBytes || file.Error != "" {
			t.Errorf("file %d unbounded/unreadable log: name=%s truncated=%v len=%d err=%q", i, file.Name, file.Truncated, len(file.Text), file.Error)
		}
	}
	if report.Files[0].Name != "logs/start-new.log" || report.Files[1].Name != "logs/start-old.log" || report.Files[2].Name != "logs/start-old2.log" || report.Files[3].Name != "mariadb.log" {
		t.Fatalf("wrong logs selected: %v", []string{report.Files[0].Name, report.Files[1].Name, report.Files[2].Name, report.Files[3].Name})
	}
	if !strings.Contains(report.Files[0].Text, "new startup [접속 정보 가림] api_key=[가림]") || !strings.Contains(report.Files[1].Text, "OLDER_STARTUP_SELECTED_ONE") || !strings.Contains(report.Files[2].Text, "OLDER_STARTUP_SELECTED_TWO") || !strings.Contains(report.Files[3].Text, "database diagnostic password=[가림] Authorization: [가림]") {
		t.Fatal("log text missing or credentials not masked in place")
	}
}

func TestOfflineDiagnosticsMissingDataCreatesReportWithoutStartingBackend(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "missing-data")
	runDiagnosticCommand(t, "--root", root, "--data", "missing-data")
	report, _, _ := readOfflineReport(t, data)
	if report.Install.Status != "missing" || len(report.Files) != 2 {
		t.Fatalf("missing data report=%+v", report)
	}
	for _, file := range report.Files {
		if file.Error == "" || file.Text != "" {
			t.Errorf("missing log did not produce error: %+v", file)
		}
	}
	entries, err := os.ReadDir(data)
	if err != nil || len(entries) != 1 || entries[0].Name() != "diagnostics" {
		t.Fatalf("offline command created backend files: %v %v", entries, err)
	}
}

func TestOfflineDiagnosticsInstallAllowlistFailureCases(t *testing.T) {
	for _, test := range []struct{ name, input, status string }{
		{"malformed", `{"root_password":"INSTALL_PRIVATE",`, "invalid"},
		{"oversized", strings.Repeat("INSTALL_PRIVATE", 6000), "too_large"},
		{"wrong_type", `{"backend_port":"INSTALL_PRIVATE"}`, "invalid"},
		{"unknown_only", `{"password":"INSTALL_PRIVATE","prompt":"PRIVATE_PROMPT"}`, "available"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeDiagnosticFixture(t, filepath.Join(root, "data", "install.json"), test.input)
			runDiagnosticCommand(t, "--root", root, "--data", "data")
			report, body, _ := readOfflineReport(t, filepath.Join(root, "data"))
			if report.Install.Status != test.status || strings.Contains(body, "INSTALL_PRIVATE") || strings.Contains(body, "PRIVATE_PROMPT") {
				t.Fatalf("status=%s private content exported=%t", report.Install.Status, strings.Contains(body, "PRIVATE"))
			}
		})
	}
}
