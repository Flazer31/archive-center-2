package osfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A folder with a file another handle holds open cannot be renamed on
// Windows (the 11-06 first-start failure); Rename succeeds once it is let go.
func TestRenameWaitsForAHeldFolderOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the hold is a Windows behavior")
	}
	dir := t.TempDir()
	from, to := filepath.Join(dir, "mariadb.init"), filepath.Join(dir, "mariadb")
	if err := os.Mkdir(from, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(from, "ibdata1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err == nil || !held(err) {
		f.Close()
		t.Fatalf("the open file did not hold the folder (os.Rename: %v)", err)
	}
	go func() { time.Sleep(400 * time.Millisecond); f.Close() }()
	if err := Rename(from, to); err != nil {
		t.Fatalf("Rename after the hold ended: %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, "ibdata1")); err != nil {
		t.Fatal("the folder did not move:", err)
	}
}

// A hold that outlasts the retry returns the same error os.Rename gives.
func TestRenameGivesUpAfterItsBound(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the hold is a Windows behavior")
	}
	defer func(d time.Duration) { retryFor = d }(retryFor)
	retryFor = 300 * time.Millisecond
	dir := t.TempDir()
	from := filepath.Join(dir, "held")
	if err := os.Mkdir(from, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(from, "x"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	if err := Rename(from, filepath.Join(dir, "to")); err == nil || !held(err) {
		t.Fatalf("expected the held error, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("did not stop at its bound")
	}
}

// Errors other than a hold are returned at once (a missing source).
func TestRenameReturnsOtherErrorsAtOnce(t *testing.T) {
	start := time.Now()
	if err := Rename(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "to")); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("retried an error that is not a hold")
	}
}
