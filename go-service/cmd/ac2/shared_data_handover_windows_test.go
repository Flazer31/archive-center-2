package main

// Opt-in, offline managed-process integration test. The historical executable
// and local engine are explicit inputs; this never opens an installed data dir.
import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
	"golang.org/x/sys/windows"
)

func TestOfflineOldDataHandoverManagedRestart(t *testing.T) {
	oldExe, newExe, engine := os.Getenv("AC2_HANDOVER_OLD_EXE"), os.Getenv("AC2_HANDOVER_NEW_EXE"), os.Getenv("AC2_HANDOVER_ENGINE")
	if oldExe == "" || newExe == "" || engine == "" {
		t.Skip("requires explicit synthetic old/new binaries and local MariaDB engine")
	}
	for _, p := range []string{oldExe, newExe, filepath.Join(engine, "bin", "mariadbd.exe")} {
		if _, e := os.Stat(p); e != nil {
			t.Fatal(e)
		}
	}
	version, err := exec.Command(filepath.Join(engine, "bin", "mariadbd.exe"), "--version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	t.Log(strings.TrimSpace(string(version)))
	root := t.TempDir()
	local := filepath.Join(root, "isolated-local")
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("AC2_DATA_DIR", "")
	t.Setenv("VOYAGE_API_KEY", "")
	// Cache-path alias only: the engine's real version is logged above. This is
	// not evidence for the production pinned MariaDB release or engine upgrades.
	cache := filepath.Join(local, "ArchiveCenter2", "runtime", "mariadb", "13.0.2")
	if err = os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
		t.Fatal(err)
	}
	if err = filepath.WalkDir(engine, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(engine, p)
		if e != nil {
			return e
		}
		dst := filepath.Join(cache, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		handoverCopyFile(t, p, dst)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	oldRoot := filepath.Join(root, "old 0.0.1 원본")
	a := filepath.Join(root, "new A")
	b := filepath.Join(root, "new B")
	for _, r := range []string{oldRoot, a, b} {
		if err = os.MkdirAll(filepath.Join(r, "bin"), 0700); err != nil {
			t.Fatal(err)
		}
		src := newExe
		if r == oldRoot {
			src = oldExe
		}
		handoverCopyFile(t, src, filepath.Join(r, "bin", "ac2.exe"))
		handoverCopyFile(t, filepath.Join("..", "..", "..", "bin", "stop.ps1"), filepath.Join(r, "bin", "stop.ps1"))
	}
	port, err := freePort(0)
	if err != nil {
		t.Fatal(err)
	}
	oldData := filepath.Join(oldRoot, "data")
	common := filepath.Join(local, "ArchiveCenter2", "data")
	stop := handoverStart(t, oldRoot, oldData, port)
	db, box := handoverDB(t, oldData)
	job, err := db.JobFor(memory.Source{Session: "synthetic-handover", Turn: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SaveGeneration(job, store.Generation{Model: "synthetic-no-provider", Output: memory.Output{Memories: []memory.Memory{{ID: "m1", Kind: "event", Body: "Synthetic Mira left a blue notebook at the station."}}}}); err != nil {
		t.Fatal(err)
	}
	before, err := db.ItemsOf([]int64{job.ID})
	if err != nil || len(before.Memories) != 1 {
		t.Fatal("fixture memory", err)
	}
	if err = db.SetMemoryBudgets(1357, 2468); err != nil {
		t.Fatal(err)
	}
	const syntheticKey = "synthetic-handover-key-not-a-provider-credential"
	handoverSettingsHTTP(t, oldData, port, `{"critic":{"provider":"openai","model":"gpt-5-mini","reasoning":"low","retries":4,"known_chars":12345,"key":"`+syntheticKey+`"}}`)
	saved, err := db.ModelSettings(box)
	if err != nil || saved.Key != syntheticKey {
		t.Fatal("settings seed", err)
	}
	sealed := handoverSealed(t, oldData)
	if !strings.HasPrefix(sealed, "v1:") || sealed == syntheticKey {
		t.Fatal("key not sealed")
	}
	db.Close()
	stop()
	if t.Failed() {
		t.FailNow()
	}
	original := handoverHashes(t, oldData)
	// Explicit offline whole-directory copy, after manager and both child PIDs
	// exited. No discovery, merge, overwrite or product migration is involved.
	if _, err = os.Stat(common); !os.IsNotExist(err) {
		t.Fatal("fixture destination exists")
	}
	if err = filepath.WalkDir(oldData, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(oldData, p)
		if e != nil {
			return e
		}
		dst := filepath.Join(common, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		handoverCopyFile(t, p, dst)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, handoverHashes(t, common)) {
		t.Fatal("whole data copy differs")
	}
	var job2 int64
	for _, r := range []string{a, b} {
		stop = handoverStart(t, r, "", port) // production default resolver, not -data
		db, box = handoverDB(t, common)
		after, e := db.ModelSettings(box)
		if e != nil || !reflect.DeepEqual(saved, after) {
			t.Fatal("model settings/decryption changed", e)
		}
		items, e := db.ItemsOf([]int64{job.ID})
		if e != nil || !reflect.DeepEqual(before, items) {
			t.Fatal("memory changed", e)
		}
		c, s, e := db.MemoryBudgets()
		if e != nil || c != 1357 || s != 2468 {
			t.Fatal("budgets changed", e)
		}
		if handoverSealed(t, common) != sealed {
			t.Fatal("sealed value changed")
		}
		handoverSettingsHTTP(t, common, port, "")
		if r == a {
			j, e := db.JobFor(memory.Source{Session: "synthetic-handover-added", Turn: 1})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = db.SaveGeneration(j, store.Generation{Model: "synthetic-no-provider", Output: memory.Output{Memories: []memory.Memory{{ID: "m2", Kind: "fact", Body: "Synthetic additional record after handover."}}}}); e != nil {
				t.Fatal(e)
			}
			job2 = j.ID
		} else {
			items, e = db.ItemsOf([]int64{job2})
			if e != nil || len(items.Memories) != 1 {
				t.Fatal("new record lost on restart", e)
			}
		}
		db.Close()
		stop()
		if t.Failed() {
			t.FailNow()
		}
		for _, name := range []string{"secret.key", "token.txt", "install.json"} {
			got, e := os.ReadFile(filepath.Join(common, name))
			if e != nil || sha256.Sum256(got) != original[name] {
				t.Fatalf("%s changed", name)
			}
		}
		if !reflect.DeepEqual(original, handoverHashes(t, oldData)) {
			t.Fatal("preserved original modified")
		}
		if _, e := os.Stat(filepath.Join(r, "data")); !os.IsNotExist(e) {
			t.Fatal("new package-local data created")
		}
	}
	t.Log("old 0.0.1 managed -> stopped whole-data handover -> default shared data from new roots A/B: settings, sealed key, token, install config, memory and additional write retained; original hashes unchanged")
}

func handoverCopyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, e := os.Open(src)
	if e != nil {
		t.Fatal(e)
	}
	defer in.Close()
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = io.Copy(out, in)
	ce := out.Close()
	if e != nil || ce != nil {
		t.Fatal("copy", e, ce)
	}
}
func handoverHashes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	v := map[string][32]byte{}
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		v[rel] = sha256.Sum256(b)
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func handoverDB(t *testing.T, data string) (*store.DB, *secret.Box) {
	t.Helper()
	dsn, e := os.ReadFile(filepath.Join(data, "mariadb.dsn"))
	if e != nil {
		t.Fatal(e)
	}
	db, e := store.Open(strings.TrimSpace(string(dsn)))
	if e != nil {
		t.Fatal("open synthetic store")
	}
	t.Cleanup(func() { db.Close() })
	box, e := secret.LoadBox(filepath.Join(data, "secret.key"))
	if e != nil {
		db.Close()
		t.Fatal(e)
	}
	return db, box
}
func handoverSealed(t *testing.T, data string) string {
	t.Helper()
	dsn, e := os.ReadFile(filepath.Join(data, "mariadb.dsn"))
	if e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("mysql", strings.TrimSpace(string(dsn)))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var v string
	if e = db.QueryRow("SELECT value FROM settings WHERE name='critic_key'").Scan(&v); e != nil {
		t.Fatal(e)
	}
	return v
}
func handoverSettingsHTTP(t *testing.T, data string, port int, body string) {
	t.Helper()
	token, e := os.ReadFile(filepath.Join(data, "token.txt"))
	if e != nil {
		t.Fatal(e)
	}
	method := "GET"
	if body != "" {
		method = "POST"
	}
	req, e := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d/settings/models", port), strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set(observe.TokenHeader, strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	c := http.Client{Timeout: 3 * time.Second}
	resp, e := c.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("settings HTTP status", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
}

func handoverStart(t *testing.T, root, data string, port int) func() {
	t.Helper()
	args := []string{"-managed", "-listen", "127.0.0.1", "-port", strconv.Itoa(port)}
	if data != "" {
		args = append(args, "-data", data)
	} else {
		data = filepath.Join(os.Getenv("LOCALAPPDATA"), "ArchiveCenter2", "data")
	}
	logFile, e := os.Create(filepath.Join(root, "managed-test.log"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if t.Failed() {
			raw, _ := os.ReadFile(filepath.Join(root, "managed-test.log"))
			token, _ := os.ReadFile(filepath.Join(data, "token.txt"))
			if len(bytes.TrimSpace(token)) > 0 {
				raw = bytes.ReplaceAll(raw, bytes.TrimSpace(token), []byte("[synthetic token redacted]"))
			}
			var c installConfig
			b, _ := os.ReadFile(filepath.Join(data, "install.json"))
			_ = json.Unmarshal(b, &c)
			t.Log(scrubInstallLog(string(raw), c))
		}
	})
	cmd := exec.Command(filepath.Join(root, "bin", "ac2.exe"), args...)
	cmd.Dir = root
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	var handles []windows.Handle
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		s := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(root, "bin", "stop.ps1"), "-Data", data)
		if out, e := s.CombinedOutput(); e != nil {
			t.Errorf("stop failed: %v %s", e, out)
		}
		select {
		case e := <-done:
			if e != nil {
				t.Errorf("managed exit: %v", e)
			}
		case <-time.After(25 * time.Second):
			cmd.Process.Kill()
			t.Error("managed shutdown timeout")
		}
		for _, h := range handles {
			state, e := windows.WaitForSingleObject(h, 0)
			if e != nil || state != windows.WAIT_OBJECT_0 {
				t.Error("managed child still running", e)
			}
			windows.CloseHandle(h)
		}
		if _, e := os.Stat(filepath.Join(data, "backend.pid")); !os.IsNotExist(e) {
			t.Error("backend.pid not removed")
		}
		logFile.Close()
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, e = os.Stat(filepath.Join(data, "backend.pid")); e == nil {
			c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
			if e == nil {
				c.Close()
				break
			}
		}
		select {
		case e := <-done:
			done <- e
			t.Fatal("managed exited before readiness; inspect isolated managed-test.log", e)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("managed readiness timeout")
		}
		time.Sleep(200 * time.Millisecond)
	}
	var conf installConfig
	b, e := os.ReadFile(filepath.Join(data, "install.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &conf); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("mysql", conf.connection("ac2", conf.AppPassword, "ac2"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var pidPath string
	if e = db.QueryRow("SELECT @@pid_file").Scan(&pidPath); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{filepath.Join(data, "backend.pid"), pidPath} {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		pid, e := strconv.Atoi(strings.TrimSpace(string(bytes.Trim(b, "\x00"))))
		if e != nil {
			t.Fatal(e)
		}
		h, e := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if e != nil {
			t.Fatal(e)
		}
		handles = append(handles, h)
	}
	return stop
}
