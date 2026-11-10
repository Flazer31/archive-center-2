//go:build windows

package observe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func fileDACL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// The token file, made new, rotated, or already there with inherited access,
// lists exactly the backend's account and SYSTEM, and inherits nothing.
func TestTokenFileOwnerOnlyOnWindows(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// "D:P" (protected: nothing inherited from the folder), then flags
	// Windows adds itself (AI = auto-inherited bookkeeping), then the entries
	want := "(A;;FA;;;" + u.User.Sid.String() + ")(A;;FA;;;SY)"
	check := func(stage, path string) {
		got := fileDACL(t, path)
		at := strings.Index(got, "(")
		if !strings.HasPrefix(got, "D:P") || at < 0 || got[at:] != want {
			t.Fatalf("%s: access %s, want D:P…%s", stage, got, want)
		}
	}
	path := filepath.Join(t.TempDir(), "token.txt")
	tok, _, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	check("made", path)
	if _, _, err := tok.Rotate(); err != nil {
		t.Fatal(err)
	}
	check("rotated", path)

	// a file written by other means inherits the folder's access; loading it
	// replaces that
	old := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(old, []byte("ac2-copied-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fileDACL(t, old); !strings.Contains(got, "ID") {
		t.Skipf("the temp folder passes no inherited access (%s); nothing to replace", got)
	}
	if _, _, err := LoadToken(old); err != nil {
		t.Fatal(err)
	}
	check("loaded", old)
}
