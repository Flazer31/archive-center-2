package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/packageupdate"
	"archivecenter2/goservice/internal/serve"
)

func TestStopManagedReapsStuckChildBeforeRollback(t *testing.T) {
	if os.Getenv("AC2_TEST_STUCK_MANAGER") == "1" {
		signal.Ignore(os.Interrupt)
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return
	}
	root := t.TempDir()
	data := t.TempDir() // No backend.pid: the real Windows stop script has nothing to stop.
	if runtime.GOOS == "windows" {
		stop, err := os.ReadFile("../../../bin/stop.ps1")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "bin", "stop.ps1"), stop, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	updates := filepath.Join(root, ".updates")
	if err := os.Mkdir(updates, 0o700); err != nil {
		t.Fatal(err)
	}
	const journal = `{"contract_version":"ac2.update-state.v1","status":"applied_pending_health","current_version":"1","target_version":"2","journal":[{"path":"candidate-only.bin"}]}`
	if err := os.WriteFile(filepath.Join(updates, "update-state.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(root, "candidate-only.bin")
	if err := os.WriteFile(candidate, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(guard, os.Args[0], "-test.run=^TestStopManagedReapsStuckChildBeforeRollback$")
	cmd.Env = append(os.Environ(), "AC2_TEST_STUCK_MANAGER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		cancel()
		cmd.Wait()
		t.Fatalf("child readiness %q, %v", line, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	oldGrace := managedStopGrace
	managedStopGrace = time.Second
	defer func() { managedStopGrace = oldGrace }()
	start := time.Now()
	stopManaged(root, data, cmd, done, nil)
	if guard.Err() != nil || time.Since(start) < managedStopGrace {
		t.Fatalf("stop did not reap child after grace: elapsed %v, guard %v", time.Since(start), guard.Err())
	}
	if err := <-done; err == nil || cmd.ProcessState == nil {
		t.Fatalf("child completion %v, state %v", err, cmd.ProcessState)
	}
	result, err := packageupdate.Rollback(root)
	if err != nil || result.Status != "rolled_back" {
		t.Fatalf("rollback %+v, %v", result, err)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("candidate survived rollback: %v", err)
	}
}

func TestLauncherArgumentsMatchBackendFlagOverrides(t *testing.T) {
	for _, args := range [][]string{
		{"-data", "package-data", "-data", "chosen-data"},
		{"-data=package-data", "--data=chosen-data"},
		nil,
	} {
		flags := flag.NewFlagSet("backend", flag.ContinueOnError)
		data := flags.String("data", "default-data", "")
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		if got := argumentValue(args, "data", "default-data"); got != *data {
			t.Fatalf("launcher data %q differs from backend data %q for %v", got, *data, args)
		}
	}
}

func TestProbeUsesTokenReadyAndMachineVersion(t *testing.T) {
	data := t.TempDir()
	const token = "synthetic-runner-token"
	if err := os.WriteFile(filepath.Join(data, "token.txt"), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	ready, version, calls := "ready", "2.3.4", []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(observe.TokenHeader) != token {
			t.Error("probe omitted existing access token")
			w.WriteHeader(401)
			return
		}
		calls = append(calls, r.URL.Path)
		if r.URL.Path == "/ready" {
			json.NewEncoder(w).Encode(map[string]any{"status": ready})
			return
		}
		if r.URL.Path == "/version" {
			json.NewEncoder(w).Encode(map[string]any{"version": "display name", "machine_version": version})
			return
		}
		t.Error("unexpected probe route")
		w.WriteHeader(404)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err := os.WriteFile(filepath.Join(data, "install.json"), []byte(`{"backend_port":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-port", port, "-listen", "all"}
	probe := func() error { return probeReady(context.Background(), server.Client(), data, args, version) }
	if err := probe(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "/ready,/version" {
		t.Fatalf("wrong readiness probes %v", calls)
	}
	if err := probeReady(context.Background(), server.Client(), data, args, "different-machine-version"); err == nil {
		t.Fatal("accepted wrong backend version")
	}
	ready = "not_ready"
	if err := probe(); err == nil {
		t.Fatal("committed a backend that is not ready")
	}
	if err := os.Remove(filepath.Join(data, "token.txt")); err != nil {
		t.Fatal(err)
	}
	before := len(calls)
	if err := probe(); err == nil || len(calls) != before {
		t.Fatal("probe invented a token or sent an unauthenticated request")
	}
}

type probeTransport func(*http.Request) (*http.Response, error)

func (f probeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProbeDefaultsToFixedPortWithoutInstallSettings(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "token.txt"), []byte("synthetic-probe-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: probeTransport(func(r *http.Request) (*http.Response, error) {
		_, port, err := net.SplitHostPort(r.URL.Host)
		if err != nil || port != fmt.Sprint(serve.DefaultPort) || r.Header.Get(observe.TokenHeader) != "synthetic-probe-token" {
			t.Fatal("probe ignored the fixed default port or existing token")
		}
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"ready","machine_version":"synthetic-version"}`))}, nil
	})}
	if err := probeReady(context.Background(), client, data, nil, "synthetic-version"); err != nil || calls != 2 {
		t.Fatalf("fixed-port probe failed: %v (%d calls)", err, calls)
	}
}

func TestWaitReadyObservesExitedManager(t *testing.T) {
	done := make(chan error, 1)
	done <- nil
	if err := waitReady(context.Background(), t.TempDir(), nil, "expected", done); err == nil {
		t.Fatal("accepted exited manager")
	}
	if _, ok := <-done; !ok {
		t.Fatal("lost completion for shutdown owner")
	}
}

func TestResolvedDataArgumentsMatchBackend(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AC2_DATA_DIR", filepath.Join(root, "shared"))
	for _, args := range [][]string{
		nil,
		{"-managed", "-data", "first", "--data=last", "-port", "3123"},
		{"--", "-data", "ignored"},
		{"positional", "-data", "ignored"},
		{"-tls-cert", "-data=certificate-name", "-managed=false"},
	} {
		data, normalized, err := resolveBackendData(root, args)
		if err != nil {
			t.Fatal(err)
		}
		flags := flag.NewFlagSet("backend", flag.ContinueOnError)
		parsed := flags.String("data", "", "")
		flags.Bool("managed", false, "")
		flags.Int("port", 0, "")
		flags.String("tls-cert", "", "")
		if err := flags.Parse(normalized); err != nil {
			t.Fatal(err)
		}
		if *parsed != data {
			t.Fatalf("child data %q differs from probe %q", *parsed, data)
		}
		want := filepath.Join(root, "shared")
		if len(args) > 0 && args[0] == "-managed" {
			want = filepath.Join(root, "last")
		}
		if data != want {
			t.Fatalf("got %q want %q", data, want)
		}
	}
}

func TestSharedPathAcrossPackageRootsDoesNotTouchFiles(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("AC2_DATA_DIR", shared)
	contents := map[string]string{"token.txt": "synthetic-token", "secret.key": "synthetic-key", "install.json": `{"backend_port":3123,"app_password":"synthetic"}`}
	for name, value := range contents {
		if err := os.WriteFile(filepath.Join(shared, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		root := t.TempDir()
		data, _, err := resolveBackendData(root, nil)
		if err != nil || data != shared {
			t.Fatalf("data %q: %v", data, err)
		}
		for name, want := range contents {
			got, err := os.ReadFile(filepath.Join(data, name))
			if err != nil || string(got) != want {
				t.Fatalf("%s changed: %v", name, err)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(err) {
			t.Fatal("resolver created package data")
		}
	}
}

func TestNormalizedArgumentsPreserveMalformedDataFlag(t *testing.T) {
	t.Setenv("AC2_DATA_DIR", t.TempDir())
	_, args, err := resolveBackendData(t.TempDir(), []string{"-managed", "-data"})
	if err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("backend", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.String("data", "", "")
	flags.Bool("managed", false, "")
	if err := flags.Parse(args); err == nil {
		t.Fatal("normalized a missing data value into valid arguments")
	}
}
