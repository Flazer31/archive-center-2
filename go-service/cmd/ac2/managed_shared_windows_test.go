package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsMariaDBSharedReuse(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(mariaWindowsURL), "mariadb-"), "-winx64.zip")
	dest := filepath.Join(cache, "ArchiveCenter2", "runtime", "mariadb", version)
	engine := filepath.Join(dest, "bin", "mariadbd.exe")
	if err := os.MkdirAll(filepath.Dir(engine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine, []byte("synthetic shared engine"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	downloads := 0
	http.DefaultTransport = installDownloadTransport(func(r *http.Request) (*http.Response, error) {
		downloads++
		return nil, fmt.Errorf("unexpected network request: %s", r.URL)
	})
	for range []string{"first package", "second package"} {
		root := t.TempDir()
		got, err := windowsMariaDB(context.Background())
		if err != nil || got != dest {
			t.Fatalf("shared engine not reused: got=%q err=%v", got, err)
		}
		if _, err := os.Stat(filepath.Join(root, "runtime")); !os.IsNotExist(err) {
			t.Fatal("package-local runtime created")
		}
	}
	if downloads != 0 {
		t.Fatalf("shared engine downloaded again: %d", downloads)
	}
}

func TestWindowsMariaDBSharedInstallAndRenameRace(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			t.Setenv("LOCALAPPDATA", t.TempDir())
			cache, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			version := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(mariaWindowsURL), "mariadb-"), "-winx64.zip")
			dest := filepath.Join(cache, "ArchiveCenter2", "runtime", "mariadb", version)
			root := t.TempDir()
			// This old package-local engine must not participate in shared installation.
			legacy := filepath.Join(root, "runtime", "mariadb", "bin", "mariadbd.exe")
			if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			z := zip.NewWriter(&archive)
			body := []byte("synthetic downloaded engine")
			w, err := z.Create("release/bin/mariadbd.exe")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(archive.Bytes())
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			downloads := 0
			winner := []byte("other installation won rename")
			http.DefaultTransport = installDownloadTransport(func(r *http.Request) (*http.Response, error) {
				downloads++
				if r.Method != http.MethodGet || r.URL.String() != mariaWindowsURL {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				entries, err := os.ReadDir(filepath.Dir(dest))
				if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "mariadb-download-") {
					t.Fatal("download not staged under shared runtime", err)
				}
				if race {
					// Install the competing winner after the initial existence check.
					engine := filepath.Join(dest, "bin", "mariadbd.exe")
					if err := os.MkdirAll(filepath.Dir(engine), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(engine, winner, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(archive.Bytes())), ContentLength: int64(archive.Len())}, nil
			})
			got, err := prepareWindowsMariaDB(context.Background(), dest, mariaWindowsURL, hex.EncodeToString(digest[:]))
			if err != nil || got != dest {
				t.Fatalf("shared installation failed: got=%q err=%v", got, err)
			}
			if race {
				body = winner
			}
			actual, err := os.ReadFile(filepath.Join(dest, "bin", "mariadbd.exe"))
			if err != nil || !bytes.Equal(actual, body) {
				t.Fatal("wrong installed engine", err)
			}
			if downloads != 1 {
				t.Fatalf("download count %d", downloads)
			}
			entries, err := os.ReadDir(filepath.Dir(dest))
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(dest) {
				t.Fatal("shared install left staging files", err)
			}
			// The production wrapper must reuse this install in the next package.
			if got, err := windowsMariaDB(context.Background()); err != nil || got != dest {
				t.Fatal("wrapper uses a different runtime", got, err)
			}
			if downloads != 1 {
				t.Fatal("shared wrapper downloaded again")
			}
			actual, err = os.ReadFile(legacy)
			if err != nil || string(actual) != "legacy" {
				t.Fatal("legacy runtime modified", err)
			}
		})
	}
}

func TestWindowsMariaDBUserProfileFallback(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	profile := t.TempDir()
	t.Setenv("USERPROFILE", profile)
	version := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(mariaWindowsURL), "mariadb-"), "-winx64.zip")
	dest := filepath.Join(profile, "AppData", "Local", "ArchiveCenter2", "runtime", "mariadb", version)
	engine := filepath.Join(dest, "bin", "mariadbd.exe")
	if err := os.MkdirAll(filepath.Dir(engine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine, []byte("synthetic fallback engine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := windowsMariaDB(context.Background()); err != nil || got != dest {
		t.Fatal("USERPROFILE fallback failed", got, err)
	}
}
