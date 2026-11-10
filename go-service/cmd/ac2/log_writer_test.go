package main

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Cross-review 03: the backend's own start must install the indented log
// writer. A data path with a line break makes runBackend's first log record
// span lines; its continuation must arrive indented on the real stderr.
func TestRunBackendIndentsMultilineLogRecords(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestRunBackendLogHelper", "--")
	cmd.Env = append(os.Environ(), "AC2_LOG_WRITER_CHILD="+t.TempDir())
	out, _ := cmd.CombinedOutput()
	got := strings.ReplaceAll(string(out), "\r\n", "\n")
	if !strings.Contains(got, "\n    [시작] SYNTHETIC_PRIVATE") {
		t.Fatalf("the continuation line is not indented: %q", got)
	}
	if strings.Contains(got, "\n[시작] SYNTHETIC_PRIVATE") {
		t.Fatalf("a quoted line starts a record: %q", got)
	}
}

func TestRunBackendLogHelper(t *testing.T) {
	parent := os.Getenv("AC2_LOG_WRITER_CHILD")
	if parent == "" {
		return
	}
	// under the parent test's temporary folder, which the parent removes
	dir := parent + "\n[시작] SYNTHETIC_PRIVATE"
	os.Args = []string{os.Args[0], "-data", dir, "-port", "1", "-listen", "127.0.0.1"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Exit(runBackend())
}
