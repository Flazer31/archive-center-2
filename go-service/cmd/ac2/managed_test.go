package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadProgressText(t *testing.T) {
	for _, c := range []struct {
		name            string
		received, total int64
		want            string
	}{
		{"empty", 0, 10_000_000, "0.0 / 10.0 MB (0.0%)"},
		{"partial", 2_500_000, 10_000_000, "2.5 / 10.0 MB (25.0%)"},
		{"complete", 10_000_000, 10_000_000, "10.0 / 10.0 MB (100.0%)"},
		{"fraction", 1_234_567, 3_000_000, "1.2 / 3.0 MB (41.2%)"},
		{"unknown", 2_500_000, -1, "2.5 MB 받음"},
		{"zero total", 2_500_000, 0, "2.5 MB 받음"},
		{"large", 3_000_000_000, 6_000_000_000, "3000.0 / 6000.0 MB (50.0%)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := downloadProgressText(c.received, c.total); got != "[1/5] 내려받기: "+c.want {
				t.Fatalf("received=%d total=%d: %q", c.received, c.total, got)
			}
		})
	}
}

func TestDownloadProgressWritesAndInterval(t *testing.T) {
	var output bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	t.Cleanup(func() { log.SetOutput(writer); log.SetFlags(flags) })
	log.SetOutput(&output)
	log.SetFlags(0)
	body := bytes.Repeat([]byte("progress"), 128)
	p := &downloadProgress{total: int64(len(body))}
	p.print()
	output.Reset()
	if n, err := p.Write(body[:len(body)/2]); err != nil || n != len(body)/2 {
		t.Fatalf("write: %d %v", n, err)
	}
	if output.Len() != 0 {
		t.Fatal("reported again before the interval")
	}
	p.last = time.Now().Add(-2 * time.Second)
	if _, err := io.Copy(p, bytes.NewReader(body[len(body)/2:])); err != nil {
		t.Fatal(err)
	}
	if p.received != int64(len(body)) || strings.TrimSpace(output.String()) != downloadProgressText(int64(len(body)), p.total) {
		t.Fatal("periodic report did not count bytes written", output.String())
	}
	output.Reset()
	p.print()
	if !strings.Contains(output.String(), "100.0%") {
		t.Fatal("final report missing", output.String())
	}
}

type installDownloadTransport func(*http.Request) (*http.Response, error)

func (f installDownloadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestMariaDownloadErrorsKeepCleanupAndKoreanMessage(t *testing.T) {
	transport, writer, flags := http.DefaultTransport, log.Writer(), log.Flags()
	t.Cleanup(func() { http.DefaultTransport = transport; log.SetOutput(writer); log.SetFlags(flags) })
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var output bytes.Buffer
			log.SetOutput(&output)
			log.SetFlags(0)
			http.DefaultTransport = installDownloadTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.String() != mariaWindowsURL {
					t.Fatalf("unexpected download request: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("synthetic corrupt archive")), ContentLength: -1}, nil
			})
			root := t.TempDir()
			_, err := prepareWindowsMariaDB(context.Background(), filepath.Join(root, "runtime", "mariadb"), mariaWindowsURL, mariaWindowsSHA)
			if err == nil {
				t.Fatal("download failure accepted")
			}
			if status == http.StatusOK {
				if !strings.Contains(err.Error(), "해시가 공식 배포본과 다릅니다") || !strings.Contains(output.String(), "MB 받음") {
					t.Fatal("hash mismatch or unknown-size progress missing", err, output.String())
				}
			} else if !strings.Contains(err.Error(), "내려받기 실패") {
				t.Fatal("download error is not actionable Korean", err)
			}
			if strings.Contains(output.String(), "[3/5]") {
				t.Fatal("extract started after failed download/hash")
			}
			entries, err := os.ReadDir(filepath.Join(root, "runtime"))
			if err != nil || len(entries) != 0 {
				t.Fatal("failed download left temporary files", err)
			}
		})
	}
}

func TestInstallSettingsPreserveCredentialsAndDatabasePort(t *testing.T) {
	dir := t.TempDir()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	c, err := installSettings(dir, port)
	if err != nil {
		t.Fatal(err)
	}
	if c.BackendPort != port || c.DatabasePort == 0 || c.RootPassword == "" || c.RootPassword == c.AppPassword {
		t.Fatal("independent installation resources were not made")
	}
	next, err := installSettings(dir, port)
	if err != nil || next != c {
		t.Fatal("restart replaced persisted configuration", err)
	}
	if !strings.Contains(c.connection("ac2", c.AppPassword, "ac2"), "/ac2?") {
		t.Fatal("database name missing")
	}
	if text := scrubInstallLog(c.RootPassword+"/"+c.AppPassword, c); strings.Contains(text, c.RootPassword) || strings.Contains(text, c.AppPassword) {
		t.Fatal("credential in diagnostic")
	}
}

func TestInstallSettingsUsesRequestedPortDespiteSavedPort(t *testing.T) {
	dir := t.TempDir()
	old, err := installSettings(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	available, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := available.Addr().(*net.TCPAddr).Port
	available.Close()
	want := old
	want.BackendPort = port
	got, err := installSettings(dir, port)
	if err != nil || got != want {
		t.Fatal("requested backend port or existing DB credentials were changed", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "install.json"))
	var persisted installConfig
	if err != nil || json.Unmarshal(b, &persisted) != nil || persisted != want {
		t.Fatal("stored backend port was not brought in line with the running port")
	}
}

func TestManagedBusyPortStopsBeforeDatabasePreparation(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	data := filepath.Join(t.TempDir(), "not-created")
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	downloads := 0
	http.DefaultTransport = installDownloadTransport(func(r *http.Request) (*http.Response, error) {
		downloads++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unused"))}, nil
	})
	err = runManaged(data, port, "all", "", "", "", "", false)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(port)+" 포트") || !strings.Contains(err.Error(), "다른 시작 창을 닫고 다시 실행해 주세요") {
		t.Fatalf("occupied backend port did not produce an actionable message: %v", err)
	}
	if downloads != 0 {
		t.Fatal("database download started despite an occupied backend port")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("database/install preparation started before port conflict was reported")
	}
}

func TestInstallLockReleasesAndZipStaysInDirectory(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockInstall(filepath.Join(dir, "run.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if other, e := lockInstall(filepath.Join(dir, "run.lock")); e == nil {
		other()
		t.Fatal("two installers own the same data")
	}
	unlock()
	again, err := lockInstall(filepath.Join(dir, "run.lock"))
	if err != nil {
		t.Fatal(err)
	}
	again()
	zpath := filepath.Join(dir, "engine.zip")
	f, err := os.Create(zpath)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	w, err := z.Create("release/../outside")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("synthetic archive"))
	z.Close()
	f.Close()
	if extractMariaZip(zpath, filepath.Join(dir, "runtime")) == nil {
		t.Fatal("archive escaped extraction root")
	}
	if _, err := os.Stat(filepath.Join(dir, "outside")); !os.IsNotExist(err) {
		t.Fatal("out-of-root file written")
	}
}
