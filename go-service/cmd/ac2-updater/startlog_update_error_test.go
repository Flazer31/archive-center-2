package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/packageupdate"
)

// Cross-review 04/05: the updater's own multi-line error (here a malformed
// candidate whose file name spans lines) reaches a fresh startup log through
// the real paths: run → ApplyPendingFromRunner writes one indented log record,
// apply-pending writes its JSON failure result. The report keeps the record
// and the failure code, and since 11-08 also the error's own text (the
// continuation stays indented inside the one record).
func TestUpdaterApplyErrorStaysOneRecordWithFullText(t *testing.T) {
	for _, action := range []string{"run", "apply-pending"} {
		t.Run(action, func(t *testing.T) { updaterApplyError(t, action) })
	}
}

func updaterApplyError(t *testing.T, action string) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	logs := filepath.Join(data, "logs")
	runnerDir := filepath.Join(root, ".updates", "runner")
	for _, d := range []string{logs, runnerDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(runnerDir, "review.exe")
	if err := os.WriteFile(runner, exe, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(root, ".updates", "candidate.zip"))
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	w, err := z.Create(packageupdate.ManifestName)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"schema_version": packageupdate.ManifestContract, "package_version": "0.0.4",
		"files": []any{map[string]any{"path": "bin/missing\n\t[시작] " + privateMark + "\nend", "size_bytes": 1, "sha256": strings.Repeat("a", 64)}}}
	if err := json.NewEncoder(w).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	pending, _ := json.Marshal(packageupdate.Pending{ContractVersion: packageupdate.PendingContract, CurrentVersion: "0.0.3", TargetVersion: "0.0.4", AssetPath: ".updates/candidate.zip"})
	if err := os.WriteFile(filepath.Join(root, ".updates", "pending-update.json"), pending, 0o600); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(logs, "start-fresh.log")
	cmd := exec.Command(runner, "-test.run=TestUpdaterApplyErrorHelper", "--")
	cmd.Env = append(os.Environ(), "AC2_UPDATER_ERROR_ROOT="+root, "AC2_UPDATER_ERROR_ACTION="+action, "AC2_START_LOG="+name)
	out, _ := cmd.CombinedOutput()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(raw), "package_verification_failed") || !strings.Contains(string(raw), privateMark) {
		t.Fatalf("did not reach the apply error: %q", raw)
	}
	if action == "run" && !strings.Contains(string(raw), "\n    \t[시작] "+privateMark) {
		t.Fatalf("the error's continuation is not indented: %q", raw)
	}
	report, _ := json.Marshal(diagnostics.CollectLogs(data))
	if !strings.Contains(string(report), privateMark) {
		t.Fatalf("the report dropped the updater error's text: %s", report)
	}
	want := "업데이트 적용 실패:"
	if action == "apply-pending" {
		want = `\"code\":\"package_verification_failed\"` // the failure result line keeps its code
	}
	if !strings.Contains(string(report), want) {
		t.Fatalf("the report lost the failure record %s: %s", want, report)
	}
}

func TestUpdaterApplyErrorHelper(t *testing.T) {
	root := os.Getenv("AC2_UPDATER_ERROR_ROOT")
	if root == "" {
		return
	}
	os.Exit(run([]string{os.Getenv("AC2_UPDATER_ERROR_ACTION"), "--root", root, "-data", filepath.Join(root, "data")}))
}
