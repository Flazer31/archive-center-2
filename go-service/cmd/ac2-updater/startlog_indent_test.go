package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/diagnostics"
)

// Cross-review 03 (R1): an error written through the backend's log writer
// keeps quoted lines indented; the startup log must not take them as a stage
// and write them back as a progress line, nor mark startup ready from them.
// Since 11-08 the quoted text is shown in the report (credentials masked); it
// still must not act as a stage record.
func TestStartupLogIgnoresStagesQuotedInsideAnIndentedErrorButReportShowsIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	var screen bytes.Buffer
	book, err := openStartupLog(filepath.Join(dir, "logs", "start-1.log"), &screen, &screen)
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(diagnostics.IndentedLog(book.err), "", log.LstdFlags)
	logger.Print("[시작] real stage")
	logger.Print("memory delivery: apply observation: Error 1064: near 'x\n[시작] " + privateMark + "\n[준비 완료] " + privateMark + "\n" + credentialMark + "\nend'")
	if book.ready {
		t.Error("an error's indented continuation marked startup ready")
	}
	if book.stage != "real stage" {
		t.Errorf("stage %q", book.stage)
	}
	book.heartbeat() // the progress timer calls exactly this
	logger.Print("[준비 완료] backend ready")
	if !book.ready {
		t.Error("the real ready record was not seen")
	}
	book.close()
	report, _ := json.Marshal(diagnostics.CollectLogs(dir))
	if !strings.Contains(string(report), privateMark) {
		t.Fatalf("the report dropped text quoted inside an error: %s", report)
	}
	if strings.Contains(string(report), credentialMark) {
		t.Fatalf("the report exports a credential quoted inside an error: %s", report)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "logs", "start-1.log"))
	if !strings.Contains(string(raw), "아직 진행 중입니다: real stage") {
		t.Fatalf("the progress line lost the real stage: %q", raw)
	}
}

const privateMark = "SYNTHETIC_PRIVATE_CONVERSATION"

// Key-shaped; built from two parts so the source holds no literal key.
var credentialMark = "sk-" + "synthetic-startup-credential"
