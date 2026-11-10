package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// This subprocess replaces only the external install-db executable. It records
// every invocation and rejects any unexpected argument before touching files.
func init() {
	if os.Getenv("AC2_DBINIT_HELPER") != "1" {
		return
	}
	args := os.Args[1:]
	b, _ := json.Marshal(args)
	f, err := os.OpenFile(os.Getenv("AC2_DBINIT_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(90)
	}
	f.Write(append(b, '\n'))
	f.Close()
	root := os.Getenv("AC2_DBINIT_ROOT")
	var dir string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--datadir=") {
			dir = strings.TrimPrefix(arg, "--datadir=")
		}
	}
	want := []string{"--datadir=" + dir, "--silent"}
	if runtime.GOOS != "windows" {
		want = []string{"--no-defaults", "--datadir=" + dir, "--auth-root-authentication-method=normal", "--skip-test-db"}
	}
	if (dir != filepath.Join(root, "mariadb") && dir != filepath.Join(root, "mariadb.init")) || !reflect.DeepEqual(args, want) {
		fmt.Fprintln(os.Stderr, "unexpected install-db arguments")
		os.Exit(91)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale-marker")); !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "stale initialization was not removed")
		os.Exit(92)
	}
	if os.MkdirAll(filepath.Join(dir, "mysql"), 0o700) != nil || os.WriteFile(filepath.Join(dir, "mysql", "db.opt"), []byte("synthetic partial bootstrap"), 0o600) != nil || os.WriteFile(filepath.Join(dir, "ibdata1"), nil, 0o600) != nil {
		os.Exit(93)
	}
	if os.Getenv("AC2_DBINIT_FAIL") == "1" {
		os.Exit(23) // Deliberately no stdout/stderr, as in the reported failure.
	}
	os.Exit(0)
}

func initToolFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "calls.jsonl")
	t.Setenv("AC2_DBINIT_HELPER", "1")
	t.Setenv("AC2_DBINIT_ROOT", root)
	t.Setenv("AC2_DBINIT_CALLS", calls)
	t.Setenv("AC2_DBINIT_FAIL", "")
	return filepath.Join(root, "mariadb"), exe, calls
}

func assertInitCalls(t *testing.T, calls string, want int) {
	t.Helper()
	b, err := os.ReadFile(calls)
	if want == 0 && os.IsNotExist(err) {
		return
	}
	if err != nil || len(strings.Split(strings.TrimSpace(string(b)), "\n")) != want {
		t.Fatalf("install-db invocation count: want %d, error %v", want, err)
	}
}

func TestMariaDBInitFailureThenRetry(t *testing.T) {
	dir, tool, calls := initToolFixture(t)
	t.Setenv("AC2_DBINIT_FAIL", "1")
	err := initializeMariaDB(context.Background(), dir, tool, installConfig{})
	if err == nil || !strings.Contains(err.Error(), "exit status 23") {
		t.Errorf("silent install-db failure must retain exit status: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("failed bootstrap published final datadir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir+".init", "mysql", "db.opt")); err != nil {
		t.Errorf("partial bootstrap fixture was not created in staging: %v", err)
	}
	assertInitCalls(t, calls, 1)
	t.Setenv("AC2_DBINIT_FAIL", "")
	if err := initializeMariaDB(context.Background(), dir, tool, installConfig{}); err != nil {
		t.Fatal(err)
	}
	assertInitCalls(t, calls, 2)
	if _, err := os.Stat(filepath.Join(dir, "mysql", "db.opt")); err != nil {
		t.Fatal("retry did not publish initialized database", err)
	}
}

func TestMariaDBInitRemovesInterruptedStaging(t *testing.T) {
	dir, tool, calls := initToolFixture(t)
	if err := privateDirectory(dir + ".init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir+".init", "stale-marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := initializeMariaDB(context.Background(), dir, tool, installConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + ".init"); !os.IsNotExist(err) {
		t.Fatal("interrupted staging remains after successful retry", err)
	}
	assertInitCalls(t, calls, 1)
}

func TestMariaDBInitFreshAndAlreadyInitialized(t *testing.T) {
	dir, tool, calls := initToolFixture(t)
	if err := initializeMariaDB(context.Background(), dir, tool, installConfig{}); err != nil {
		t.Fatal(err)
	}
	assertInitCalls(t, calls, 1)
	before, err := os.ReadFile(filepath.Join(dir, "mysql", "db.opt"))
	if err != nil {
		t.Fatal(err)
	}
	// Success must move the files, and a second call must not invoke the tool.
	t.Setenv("AC2_DBINIT_FAIL", "1")
	if err := initializeMariaDB(context.Background(), dir, tool, installConfig{}); err != nil {
		t.Fatal(err)
	}
	assertInitCalls(t, calls, 1)
	after, err := os.ReadFile(filepath.Join(dir, "mysql", "db.opt"))
	if err != nil || string(after) != string(before) {
		t.Fatal("existing initialized database changed", err)
	}
}

// Earlier versions created the final folder before bootstrapping; a failure
// before install-db wrote anything left it empty.
func TestMariaDBInitReplacesEmptyFinalFolder(t *testing.T) {
	dir, tool, calls := initToolFixture(t)
	if err := privateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if err := initializeMariaDB(context.Background(), dir, tool, installConfig{}); err != nil {
		t.Fatal(err)
	}
	assertInitCalls(t, calls, 1)
	if _, err := os.Stat(filepath.Join(dir, "mysql", "db.opt")); err != nil {
		t.Fatal("empty final folder blocked publishing", err)
	}
}

// A non-empty final folder without mysql is never deleted automatically.
func TestMariaDBInitKeepsNonEmptyFinalFolder(t *testing.T) {
	dir, tool, _ := initToolFixture(t)
	if err := privateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "ibdata1")
	if err := os.WriteFile(file, []byte("earlier file"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := initializeMariaDB(context.Background(), dir, tool, installConfig{})
	if err == nil || !strings.Contains(err.Error(), "다른 이름으로 옮긴 뒤") {
		t.Fatalf("non-empty final folder must be reported, got %v", err)
	}
	if b, e := os.ReadFile(file); e != nil || string(b) != "earlier file" {
		t.Fatal("non-empty final folder was changed", e)
	}
}

func TestMariaDBInitExistingPartialFolderIsPreserved(t *testing.T) {
	dir, tool, calls := initToolFixture(t)
	if err := os.MkdirAll(filepath.Join(dir, "mysql"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "mysql", "db.opt")
	if err := os.WriteFile(file, []byte("existing user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := initializeMariaDB(context.Background(), dir, tool, installConfig{}); err != nil {
		t.Fatal(err)
	}
	assertInitCalls(t, calls, 0)
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "existing user data" {
		t.Fatal("existing partial folder was automatically changed", err)
	}
}
