package updateapi

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"archivecenter2/goservice/internal/packageupdate"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUnconfiguredRoutesNeverCallNetwork(t *testing.T) {
	var calls atomic.Int32
	s := New(Config{CurrentVersion: "1.0.0", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected network request")
	})}})
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/update/check"}, {http.MethodGet, "/update/status"},
		{http.MethodPost, "/update/download"}, {http.MethodPost, "/update/apply"},
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(route.method, route.path, strings.NewReader("{}")))
		body := decodeResponse(t, rec)
		if rec.Code != http.StatusOK || body["status"] != "not_configured" || body["message"] != "업데이트 위치가 설정되지 않았어요" || body["apply_supported"] != false {
			t.Fatalf("%s: status=%d body=%v", route.path, rec.Code, body)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unconfigured network requests = %d", calls.Load())
	}
}

func TestCheckDownloadsRealZIPAndDoesNotCreatePendingState(t *testing.T) {
	root := installTestPackage(t, "1.0.0")
	asset := testZIP(t, "1.1.0", "")
	var downloads atomic.Int32
	releaseServer := releaseServer(t, func() []byte { downloads.Add(1); return asset })
	s := New(Config{Root: root, Repository: "fixture/project", APIBase: releaseServer.URL, CurrentVersion: "1.0.0", RequestShutdown: func(int) { t.Fatal("check requested shutdown") }})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/update/check", nil))
	body := decodeResponse(t, rec)
	if rec.Code != http.StatusOK || body["update_available"] != true || body["compatibility_status"] != "compatible" || body["latest_version"] != "1.1.0" {
		t.Fatalf("check status=%d body=%v", rec.Code, body)
	}
	selected, _ := body["selected_asset"].(map[string]any)
	if selected["name"] != testAssetName() || body["platform"] != runtime.GOOS+"-"+runtime.GOARCH {
		t.Fatalf("incorrect platform asset: %v", body)
	}
	if downloads.Load() != 1 {
		t.Fatalf("downloads=%d", downloads.Load())
	}
	if _, err := os.Stat(filepath.Join(root, ".updates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check wrote update state: %v", err)
	}
}

func TestApplyVerifiesAndFlushesAcknowledgementBeforeShutdown(t *testing.T) {
	root := installTestPackage(t, "1.0.0")
	asset := testZIP(t, "1.1.0", "")
	releaseServer := releaseServer(t, func() []byte { return asset })
	rec := httptest.NewRecorder()
	shutdown := false
	s := New(Config{Root: root, Repository: "fixture/project", APIBase: releaseServer.URL, CurrentVersion: "1.0.0", RequestShutdown: func(code int) {
		shutdown = true
		if code != ApplyExitCode || !rec.Flushed {
			t.Fatalf("shutdown before acknowledgement flush: code=%d flushed=%t", code, rec.Flushed)
		}
		body := decodeResponse(t, rec)
		if body["status"] != "accepted" || body["shutdown_requested"] != true || body["exit_code"] != float64(ApplyExitCode) {
			t.Fatalf("shutdown without valid acknowledgement: %v", body)
		}
		data, err := os.ReadFile(filepath.Join(root, ".updates", "pending-update.json"))
		if err != nil {
			t.Fatal(err)
		}
		var pending packageupdate.Pending
		if err := json.Unmarshal(data, &pending); err != nil {
			t.Fatal(err)
		}
		if pending.ContractVersion != packageupdate.PendingContract || pending.TargetVersion != "1.1.0" {
			t.Fatalf("pending=%+v", pending)
		}
		if err := packageupdate.PreflightCandidate(root, packageupdate.Candidate{CurrentVersion: pending.CurrentVersion, TargetVersion: pending.TargetVersion, AssetPath: pending.AssetPath, RequiredFiles: pending.RequiredFiles}); err != nil {
			t.Fatalf("pending not applicable: %v", err)
		}
	}})
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/update/apply", strings.NewReader("{}")))
	if rec.Code != http.StatusOK || !shutdown {
		t.Fatalf("apply status=%d shutdown=%t body=%s", rec.Code, shutdown, rec.Body.String())
	}
	status := httptest.NewRecorder()
	s.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/update/status", nil))
	if body := decodeResponse(t, status); body["status"] != "pending_next_start" || body["target_version"] != "1.1.0" {
		t.Fatalf("status=%v", body)
	}
}

func TestCheckRejectsCorruptZIPAndManifestIntegrityMismatch(t *testing.T) {
	for _, damage := range []string{"corrupt", "hash", "size"} {
		t.Run(damage, func(t *testing.T) {
			root := installTestPackage(t, "1.0.0")
			asset := testZIP(t, "1.1.0", damage)
			releaseServer := releaseServer(t, func() []byte { return asset })
			s := New(Config{Root: root, Repository: "fixture/project", APIBase: releaseServer.URL, CurrentVersion: "1.0.0", RequestShutdown: func(int) { t.Fatal("invalid package requested shutdown") }})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/update/check", nil))
			body := decodeResponse(t, rec)
			if rec.Code != http.StatusOK || body["update_available"] != false || !strings.HasPrefix(fmt.Sprint(body["compatibility_status"]), "preflight_") {
				t.Fatalf("invalid package accepted: %v", body)
			}
			if _, err := os.Stat(filepath.Join(root, ".updates", "pending-update.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid package staged: %v", err)
			}
		})
	}
}

func TestChangedDownloadCannotBecomePending(t *testing.T) {
	root := installTestPackage(t, "1.0.0")
	good, bad := testZIP(t, "1.1.0", ""), testZIP(t, "1.1.0", "hash")
	var downloads atomic.Int32
	releaseServer := releaseServer(t, func() []byte {
		if downloads.Add(1) == 1 {
			return good
		}
		return bad
	})
	s := New(Config{Root: root, Repository: "fixture/project", APIBase: releaseServer.URL, CurrentVersion: "1.0.0", RequestShutdown: func(int) { t.Fatal("changed package requested shutdown") }})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/update/apply", strings.NewReader("{}")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("changed package accepted: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".updates", "pending-update.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed package became pending: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".updates", "1.1.0", testAssetName())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified staged ZIP remained: %v", err)
	}
}

func TestCurrentVersionAndPlatformMismatchBeforeNetwork(t *testing.T) {
	root := installTestPackage(t, "1.0.0")
	var calls atomic.Int32
	s := New(Config{Root: root, Repository: "fixture/project", CurrentVersion: "1.0.0", RequestShutdown: func(int) { t.Fatal("mismatch shutdown") }, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected network")
	})}})
	otherPlatform := "linux-arm64"
	if detectUpdatePlatform(runtime.GOOS, runtime.GOARCH) == otherPlatform {
		otherPlatform = "windows-amd64"
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/update/check?current_version=9.9.9", ""},
		{http.MethodGet, "/update/check?platform=" + otherPlatform, ""},
		{http.MethodPost, "/update/apply", `{"current_version":"9.9.9"}`},
		{http.MethodPost, "/update/apply", `{"platform":"` + otherPlatform + `"}`},
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(route.method, route.path, strings.NewReader(route.body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("mismatch accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("mismatch network calls=%d", calls.Load())
	}
}

func TestDownloadStagesVerifiedPackageWithoutShutdown(t *testing.T) {
	root := installTestPackage(t, "1.0.0")
	asset := testZIP(t, "1.1.0", "")
	releaseServer := releaseServer(t, func() []byte { return asset })
	s := New(Config{Root: root, Repository: "fixture/project", APIBase: releaseServer.URL, CurrentVersion: "1.0.0", RequestShutdown: func(int) { t.Fatal("download requested shutdown") }})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/update/download", strings.NewReader("{}")))
	body := decodeResponse(t, rec)
	if rec.Code != http.StatusOK || body["apply_timing"] != "next_start" {
		t.Fatalf("download status=%d body=%v", rec.Code, body)
	}
	staged, err := os.ReadFile(fmt.Sprint(body["staged_path"]))
	if err != nil || !bytes.Equal(staged, asset) {
		t.Fatalf("staged bytes differ: %v", err)
	}
}

func TestStatusPrioritizesActiveStateAndDoesNotContactReleaseServer(t *testing.T) {
	root := installTestPackage(t, "1.0.0")
	staging := filepath.Join(root, ".updates")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "pending-update.json"), []byte(`{"target_version":"1.1.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "update-state.json"), []byte(`{"status":"applied_pending_health","current_version":"1.0.0","target_version":"1.2.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Root: root, Repository: "fixture/project", CurrentVersion: "1.0.0", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("status contacted network"); return nil, nil })}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/update/status", nil))
	if body := decodeResponse(t, rec); body["status"] != "applied_pending_health" || body["target_version"] != "1.2.0" {
		t.Fatalf("active state not preferred: %v", body)
	}
}

func TestAC2PlatformAssetsAndRequiredFiles(t *testing.T) {
	platforms := []string{"windows-amd64", "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "android-arm64"}
	for _, platform := range platforms {
		for _, assetPlatform := range platforms {
			if got := assetMatchesPlatform("ac2-1.1.0-"+assetPlatform+".zip", platform); got != (platform == assetPlatform) {
				t.Fatalf("asset=%s platform=%s matched=%t", assetPlatform, platform, got)
			}
		}
		goos, _, _ := strings.Cut(platform, "-")
		for _, rel := range RequiredPackageFiles(goos) {
			if strings.HasPrefix(rel, "data/") || strings.HasPrefix(rel, "bin/runtime/") || strings.Contains(rel, "mariadb-schema") || strings.Contains(rel, "archive-center-updater") {
				t.Fatalf("AC1 or preserved file in required list: %s", rel)
			}
		}
	}
}

func TestURLPolicyAllowsHTTPSAndLoopbackOnly(t *testing.T) {
	for _, raw := range []string{"https://example.test/package.zip", "http://127.0.0.1:1234/package.zip", "http://[::1]:1234/package.zip"} {
		if err := validateUpdateDownloadURL(raw); err != nil {
			t.Fatalf("valid URL %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://example.test/package.zip", "http://127.0.0.1.example.test/package.zip", "file:///tmp/package.zip", "https:///package.zip"} {
		if err := validateUpdateDownloadURL(raw); err == nil {
			t.Fatalf("invalid URL accepted: %q", raw)
		}
	}
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response JSON: %v body=%s", err, rec.Body.String())
	}
	return body
}

func installTestPackage(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	paths := RequiredPackageFiles(runtime.GOOS)
	for _, rel := range paths {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("installed:"+rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := packageupdate.WritePackageManifest(root, version, paths); err != nil {
		t.Fatal(err)
	}
	return root
}

func testAssetName() string { return "ac2-1.1.0-" + runtime.GOOS + "-" + runtime.GOARCH + ".zip" }

func releaseServer(t *testing.T, asset func() []byte) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/fixture/project/releases/latest":
			if r.Header.Get("Accept") != "application/vnd.github+json" {
				t.Error("GitHub Accept header missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.1.0", "name": "synthetic candidate", "assets": []map[string]any{
				{"name": "ac2-1.1.0-unsupported.zip", "browser_download_url": srv.URL + "/wrong-asset"},
				{"name": testAssetName(), "browser_download_url": srv.URL + "/package.zip"},
			}})
		case "/package.zip":
			_, _ = w.Write(asset())
		default:
			t.Errorf("unexpected release request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testZIP(t *testing.T, version, damage string) []byte {
	t.Helper()
	if damage == "corrupt" {
		return []byte("invalid ZIP")
	}
	files := map[string][]byte{}
	for _, rel := range RequiredPackageFiles(runtime.GOOS) {
		files[rel] = []byte("candidate:" + rel)
	}
	readiness, err := json.Marshal(map[string]any{"contract_version": packageupdate.PackageReleaseStatusContract, "target_version": version, "release_ready": true, "automatic_update_apply": true})
	if err != nil {
		t.Fatal(err)
	}
	files[packageupdate.PackageReleaseStatusName] = readiness
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		body := files[path]
		hash := sha256.Sum256(body)
		digest := fmt.Sprintf("%x", hash)
		size := len(body)
		if path == "archive-center-2.js" {
			if damage == "hash" {
				digest = strings.Repeat("0", len(digest))
			}
			if damage == "size" {
				size++
			}
		}
		entries = append(entries, map[string]any{"path": path, "sha256": digest, "size_bytes": size})
	}
	manifest, err := json.Marshal(map[string]any{"schema_version": packageupdate.ManifestContract, "package_version": version, "files": entries})
	if err != nil {
		t.Fatal(err)
	}
	files[packageupdate.ManifestName] = manifest
	paths = append(paths, packageupdate.ManifestName)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, path := range paths {
		w, err := zw.Create("candidate/" + path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(w, bytes.NewReader(files[path])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
