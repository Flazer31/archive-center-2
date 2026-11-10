package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The package's program files are listed with size and SHA256, the data
// folder inside the package is not read, and on Windows a file that kept
// the downloaded mark is told with a hint (read only: the mark stays).
func TestPackageTrustListsFilesAndMarks(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	for name, body := range map[string]string{"bin/ac2.exe": "exe body", "bin/start.ps1": "script", "README.txt": "not a program", "data/logs/x.exe": "in data"} {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	marked := runtime.GOOS == "windows"
	if marked {
		if err := os.WriteFile(filepath.Join(root, "bin", "ac2.exe")+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256([]byte("exe body"))
	want := strings.ToUpper(hex.EncodeToString(sum[:]))
	r := PackageTrust(root, data)
	got := map[string]TrustFile{}
	for _, f := range r.Files {
		got[f.Path] = f
	}
	if len(got) != 2 || got["bin/ac2.exe"].Size != 8 || got["bin/ac2.exe"].SHA256 != want {
		t.Fatalf("files: %+v", r.Files)
	}
	if _, ok := got["data/logs/x.exe"]; ok {
		t.Fatal("the data folder was read")
	}
	if marked {
		if !got["bin/ac2.exe"].MarkOfTheWeb || got["bin/ac2.exe"].InternetZoneTag != "3" || r.MarkedFiles != 1 || !strings.Contains(r.Hint, "차단 해제") {
			t.Fatalf("mark: %+v %q", got["bin/ac2.exe"], r.Hint)
		}
		if got["bin/start.ps1"].MarkOfTheWeb {
			t.Fatal("unmarked file told as marked")
		}
		if _, err := os.Stat(filepath.Join(root, "bin", "ac2.exe") + ":Zone.Identifier"); err != nil {
			t.Fatal("the mark was removed: the report must not change files")
		}
	}
}
