package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunApplyPendingNoPending(t *testing.T) {
	if code := run([]string{"apply-pending", "--root", t.TempDir()}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	if code := run([]string{"unknown", "--root", t.TempDir()}); code == 0 {
		t.Fatal("unknown command returned success")
	}
}

func TestDataPathCommandOnlyPrintsResolvedDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "not-created")
	t.Setenv("AC2_DATA_DIR", filepath.Join(root, "environment"))
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = original }()
	code := run([]string{"data-path", "--root", root, "--", "-managed", "-data", "not-created"})
	writer.Close()
	output, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || code != 0 || strings.TrimSpace(string(output)) != target {
		t.Fatalf("code=%d output=%q err=%v", code, output, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("data-path command touched package/data files", err)
	}
}
