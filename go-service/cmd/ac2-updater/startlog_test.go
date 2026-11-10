package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartupLogPreservesConsoleAndExcludesTokenAcrossChunks(t *testing.T) {
	name := filepath.Join(t.TempDir(), "start-test.log")
	var screen bytes.Buffer
	b, err := openStartupLog(name, &screen, &screen)
	if err != nil {
		t.Fatal(err)
	}
	const token = "synthetic-access-value-do-not-store"
	text := "[시작] 테스트 준비\n새 접속 토큰이 만들어졌습니다. 이 화면에서 한 번만 보여 줍니다.\n\n    " + token + "\n\n(파일: example/token.txt)\n[준비 완료] 연결 가능합니다.\n"
	// exec's stream copying may split any UTF-8 character or token line.
	for _, v := range []byte(text) {
		if _, err := b.err.Write([]byte{v}); err != nil {
			t.Fatal(err)
		}
	}
	b.heartbeat()
	b.close()
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if screen.String() != text {
		t.Fatal("console changed or heartbeat continued after readiness")
	}
	if strings.Contains(string(got), token) || !strings.Contains(string(got), "시작 로그에는 기록하지 않습니다") || !strings.Contains(string(got), "[준비 완료]") {
		t.Fatalf("unsafe or incomplete startup log: %s", got)
	}
}

func TestStartupLogHeartbeatDoesNotCancelAndRecordsOriginalError(t *testing.T) {
	name := filepath.Join(t.TempDir(), "start-test.log")
	t.Setenv("AC2_START_LOG", name)
	var screen bytes.Buffer
	b, err := openStartupLog(name, &screen, &screen)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(b.err, "[시작] MariaDB 내려받기")
	b.heartbeat()
	b.failure(fmt.Errorf("synthetic original failure"))
	fmt.Fprintln(b.err, "synthetic original failure")
	fmt.Fprint(b.err, "last line without newline")
	b.close()
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"아직 진행 중입니다: MariaDB 내려받기", "종료 코드: 1", "synthetic original failure", "인터넷 연결", name, "last line without newline"} {
		if !strings.Contains(string(got), text) || !strings.Contains(screen.String(), text) {
			t.Errorf("screen/log omitted %q", text)
		}
	}
}

func TestStartupLogRedactsCredentialDiagnostics(t *testing.T) {
	name := filepath.Join(t.TempDir(), "start-test.log")
	var screen bytes.Buffer
	b, err := openStartupLog(name, &screen, &screen)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(b.err, "password=synthetic-pass api_key: synthetic-key Authorization: Bearer synthetic-auth user:synthetic-dsn@tcp(localhost:1)/db")
	fmt.Fprintln(b.err, "access token: path with spaces/token.txt (ends with fake)")
	fmt.Fprintln(b.err, "access token: test/token.txt is empty")
	b.close()
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"synthetic-pass", "synthetic-key", "synthetic-auth", "synthetic-dsn"} {
		if strings.Contains(string(got), value) {
			t.Errorf("credential was logged: %s", value)
		}
		if !strings.Contains(screen.String(), value) {
			t.Error("console diagnostic changed")
		}
	}
	if strings.Contains(string(got), "ends with fake") {
		t.Error("token suffix was logged")
	}
	if !strings.Contains(string(got), "is empty") {
		t.Error("token file failure was hidden")
	}
}

func TestStartupLogRecordsRealManagedChildStreams(t *testing.T) {
	root := t.TempDir()
	name := "ac2"
	if strings.HasSuffix(os.Args[0], ".exe") {
		name += ".exe"
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", name), body, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC2_STARTUP_TEST_CHILD", "1")
	logName := filepath.Join(root, "start-child.log")
	var stdout, stderr bytes.Buffer
	b, err := openStartupLog(logName, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	cmd, done, err := startManaged(root, []string{"-test.run=TestStartupLogHelper", "--"}, b)
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil {
		t.Fatal("managed command never started")
	}
	if code := exitCode(<-done); code != 7 {
		t.Fatalf("lost child status: %d", code)
	}
	b.close()
	got, err := os.ReadFile(logName)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"child stdout", "child stderr"} {
		if strings.Count(string(got), text) != 1 {
			t.Errorf("child output missing or duplicated: %s", got)
		}
	}
	if !strings.Contains(stdout.String(), "child stdout") || !strings.Contains(stderr.String(), "child stderr") {
		t.Fatal("child streams not forwarded")
	}
}

func TestStartupLogHelper(t *testing.T) {
	if os.Getenv("AC2_STARTUP_TEST_CHILD") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "child stdout")
	fmt.Fprintln(os.Stderr, "child stderr")
	os.Exit(7)
}

func TestStartupLogProgressUsesTimerWithoutStoppingWork(t *testing.T) {
	name := filepath.Join(t.TempDir(), "start-timer.log")
	var screen bytes.Buffer
	b, err := openStartupLog(name, &screen, &screen)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	stop := b.progress()
	defer stop()
	deadline := time.After(20 * time.Second)
	for {
		b.mu.Lock()
		seen := strings.Contains(screen.String(), "아직 진행 중입니다")
		b.mu.Unlock()
		if seen {
			break
		}
		select {
		case <-deadline:
			t.Fatal("production progress timer never reported a slow stage")
		case <-time.After(20 * time.Millisecond):
		}
	}
	fmt.Fprintln(b.err, "work continued after progress")
	fmt.Fprintln(b.err, "[준비 완료]")
	b.heartbeat()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !strings.Contains(screen.String(), "work continued after progress") || strings.Count(screen.String(), "아직 진행 중입니다") != 1 {
		t.Fatal("progress interrupted work or continued after readiness")
	}
}
