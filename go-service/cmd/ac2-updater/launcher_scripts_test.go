package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeLauncherFixture(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}

func copyLauncherScript(t *testing.T, packageDir, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "bin", name))
	if err != nil {
		t.Fatal(err)
	}
	writeLauncherFixture(t, filepath.Join(packageDir, "bin", name), string(body))
}

func TestStartCmdFailurePrintsPathsWithShellCharacters(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires cmd.exe")
	}
	root := filepath.Join(t.TempDir(), "AC2 (1) & test")
	bin := filepath.Join(root, "bin")
	log := filepath.Join(root, "startup (2) & log.txt")
	copyLauncherScript(t, root, "start.cmd")
	writeLauncherFixture(t, filepath.Join(bin, "start.ps1"), "exit 23\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/c", "chcp 65001 >nul && bin\\start.cmd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "AC2_START_CONSOLE=1", "AC2_SCRIPT_DIR="+bin+"\\", "AC2_START_LOG="+log)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("unexpected launcher exit %v\n%s", err, output)
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"종료 코드: 23", filepath.Join(bin, "start.ps1"), log, "창을 닫으려면"} {
		if !strings.Contains(string(output), want) || !strings.Contains(string(recorded), want) {
			t.Fatalf("missing %q in console or log\nconsole: %s\nlog: %s", want, output, recorded)
		}
	}
}

func TestStartShRecoversRunnerThroughPackageLink(t *testing.T) {
	sh := "/bin/sh"
	if runtime.GOOS == "windows" {
		sh = os.Getenv("AC2_TEST_BASH")
		if sh == "" {
			sh = filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe")
		}
		if _, err := os.Stat(sh); os.IsNotExist(err) {
			t.Skip("requires Git Bash")
		}
	}
	root := t.TempDir()
	pkg := filepath.Join(root, "package")
	copyLauncherScript(t, pkg, "start.sh")
	writeLauncherFixture(t, filepath.Join(pkg, "bin", "ac2"), "#!/bin/sh\nexit 97\n")
	writeLauncherFixture(t, filepath.Join(root, "fake", "mariadbd"), "#!/bin/sh\nexit 98\n")
	writeLauncherFixture(t, filepath.Join(root, "fake", "pkg"), "#!/bin/sh\nexit 99\n")
	writeLauncherFixture(t, filepath.Join(pkg, ".updates", "update-state.json"), `{
  "status": "applied_pending_health",
  "runner_path": ".updates/runner/recovery/updater"
}
`)
	writeLauncherFixture(t, filepath.Join(pkg, ".updates", "runner", "recovery", "updater"), `#!/bin/sh
set -eu
case "$1" in
    prepare-runner) [ "$#" = 3 ] && [ "$2" = --root ] && [ "$3" = "$(pwd -P)" ] || exit 91
        printf '%s\n' "$0" ;;
    run) [ "$#" = 5 ] && [ "$2" = --root ] && [ "$3" = "$(pwd -P)" ] && [ "$4" = -- ] && [ "$5" = -managed ] || exit 92
        printf 'recovered\n' > recovered ;;
    *) exit 93 ;;
esac
`)
	if runtime.GOOS == "windows" {
		link := exec.Command("cmd.exe", "/d", "/c", "mklink /j alias package")
		link.Dir = root
		if output, err := link.CombinedOutput(); err != nil {
			t.Fatalf("make package junction: %v\n%s", err, output)
		}
	} else if err := os.Symlink(pkg, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-c", `export PATH="$PWD/fake:$PATH" PREFIX="$PWD/fake" AC2_DATA_DIR="$PWD/data"; exec sh alias/bin/start.sh`)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("start through link: %v\n%s", err, output)
	}
	if recorded, err := os.ReadFile(filepath.Join(pkg, "recovered")); err != nil || string(recorded) != "recovered\n" {
		t.Fatalf("preserved runner did not run: %q, %v", recorded, err)
	}
}

func TestPluginHeaderCheckerExitStatus(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("requires Node.js")
	}
	root := t.TempDir()
	good, bad := filepath.Join(root, "valid.js"), filepath.Join(root, "invalid.js")
	writeLauncherFixture(t, good, "//@name fixture\n//@api 3.0\n")
	writeLauncherFixture(t, bad, "//@name fixture\n//@api 2.0\n")
	script := filepath.Join("..", "..", "..", "tools", "check-plugin-header.cjs")
	if output, err := exec.Command(node, script, good).CombinedOutput(); err != nil || !strings.Contains(string(output), good+" OK") {
		t.Fatalf("valid header: %v\n%s", err, output)
	}
	output, err := exec.Command(node, script, bad, good).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 ||
		!strings.Contains(string(output), bad+" FAIL") || !strings.Contains(string(output), good+" OK") {
		t.Fatalf("mixed header results: %v\n%s", err, output)
	}
}
