package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// cmd.exe reads a batch file by byte offsets: one with letters outside
// ASCII (Korean messages after "chcp 65001") and bare LF line ends is cut
// mid-line, and the pieces run as commands. Every .cmd the Windows package
// ships ends its lines with CRLF.
func TestPackageCmdFilesEndLinesWithCRLF(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var files []string
	for _, pattern := range []string{"*.cmd", filepath.Join("bin", "*.cmd")} {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 4 {
		t.Fatalf("package .cmd files not found: %v", files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		ascii := true
		for _, c := range b {
			if c > 0x7f {
				ascii = false
				break
			}
		}
		if ascii {
			continue
		}
		if bytes.Count(b, []byte("\n")) != bytes.Count(b, []byte("\r\n")) {
			t.Errorf("%s: a line ends with LF only", filepath.Base(f))
		}
	}
}
