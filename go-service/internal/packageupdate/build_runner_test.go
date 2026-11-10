package packageupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareRunnerPreservesCopyAndReusesPendingHealthOwner(t *testing.T) {
	rel := "bin/ac2-updater"
	if runtime.GOOS == "windows" {
		rel += ".exe"
	}
	root := newFixture(t, map[string]string{rel: "old-updater"}, map[string]string{rel: "new-updater"}, nil)
	if err := os.Chmod(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	runner, err := PrepareRunner(root)
	if err != nil {
		t.Fatal(err)
	}
	if !inside(runner, filepath.Join(root, ".updates", "runner")) {
		t.Fatalf("runner not preserved under updates: %q", runner)
	}
	assertFile(t, runner, "old-updater")
	if _, err := ApplyPendingFromRunner(root, runner); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(root, filepath.FromSlash(rel)), "new-updater")
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	reused, err := PrepareRunner(root)
	if err != nil || reused != runner {
		t.Fatalf("pending-health owner runner=%q, want %q: %v", reused, runner, err)
	}
	assertFile(t, reused, "old-updater")
	state := mustState(t, root)
	state.Status = "applying"
	writeJSON(t, filepath.Join(root, ".updates", "update-state.json"), state)
	reused, err = PrepareRunner(root)
	if err != nil || reused != runner {
		t.Fatalf("interrupted owner runner=%q, want %q: %v", reused, runner, err)
	}
}

func TestWritePackageManifestUsesExplicitFilesAndExistingIntegrityContract(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"bin/ac2": "backend", "archive-center-2.js": "plugin"}
	for rel, body := range files {
		mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), body)
	}
	mustWrite(t, filepath.Join(root, "data", "sentinel"), "private-data")
	mustWrite(t, filepath.Join(root, "bin", "runtime", "sentinel"), "runtime")
	if err := WritePackageManifest(root, "0.2.0", []string{"bin/ac2", "archive-center-2.js"}); err != nil {
		t.Fatal(err)
	}
	m, err := readPackageManifest(filepath.Join(root, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if m.PackageVersion != "0.2.0" || len(m.Files) != len(files) {
		t.Fatalf("manifest=%+v", m)
	}
	if _, err := verifyNewPackage(root, m, []string{"bin/ac2", "archive-center-2.js"}); err != nil {
		t.Fatalf("generated manifest rejected: %v", err)
	}
	mustWrite(t, filepath.Join(root, "bin", "ac2"), "changed-backend")
	if _, err := verifyNewPackage(root, m, nil); err == nil || !strings.Contains(err.Error(), "size_bytes mismatch") {
		t.Fatalf("modified generated input not detected: %v", err)
	}
	for _, rel := range []string{"data/sentinel", "bin/runtime/sentinel"} {
		if err := WritePackageManifest(root, "0.2.0", []string{rel}); err == nil || !strings.Contains(err.Error(), "protected path") {
			t.Fatalf("generated manifest accepted protected path %q: %v", rel, err)
		}
	}
}
