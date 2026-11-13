package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/store"
)

// The system server's version is read from "mariadbd --version" as the
// distributions print it (1.1: an old Ubuntu MariaDB is told before start).
func TestMariaDBVersionIn(t *testing.T) {
	for out, want := range map[string]string{
		"/usr/sbin/mariadbd  Ver 10.11.13-MariaDB-0ubuntu0.24.04.1 for debian-linux-gnu on x86_64 (Ubuntu 24.04)": "10.11.13",
		"mariadbd  Ver 11.8.3-MariaDB-ubu2404 for debian-linux-gnu on x86_64 (mariadb.org binary distribution)":   "11.8.3",
		"mariadbd Ver 12.0.2-MariaDB for Linux on aarch64 (Source distribution)":                                  "12.0.2",
		"no version here": "",
		// a version in the path is not the server's (review)
		"/opt/ac2-1.1.0/bin/mariadbd  Ver 11.8.3-MariaDB for Linux":        "11.8.3",
		"/opt/mariadb-13.0.2/bin/mariadbd  Ver 10.11.13-MariaDB for Linux": "10.11.13",
		"/opt/mariadb-13.0.2/bin/mariadbd: unknown option":                 "",
	} {
		if got := mariaDBVersionIn(out); got != want {
			t.Errorf("%q: %q, want %q", out, got, want)
		}
	}
	// what the start file decides on: a series older than its first release
	// with snapshot isolation is old (10.5, 10.6.17, 10.11.7); 10.6.18,
	// 10.11.8 and Ubuntu 24.04's 10.11.13 are not
	if store.CheckMariaDBVersion(mariaDBVersionIn("Ver 10.5.27-MariaDB")) == nil || store.CheckMariaDBVersion(mariaDBVersionIn("Ver 10.6.17-MariaDB")) == nil || store.CheckMariaDBVersion(mariaDBVersionIn("Ver 10.11.7-MariaDB")) == nil || store.CheckMariaDBVersion(mariaDBVersionIn("Ver 10.6.18-MariaDB")) != nil || store.CheckMariaDBVersion(mariaDBVersionIn("Ver 10.11.8-MariaDB")) != nil || store.CheckMariaDBVersion(mariaDBVersionIn("Ver 10.11.13-MariaDB")) != nil {
		t.Fatal("version decision")
	}
}

// An old server is named in the start window; other failures keep the one
// credential-free message (a connection error may hold credentials).
func TestDatabaseOpenHelp(t *testing.T) {
	old := databaseOpenHelp(fmt.Errorf("open: %w", store.VersionError{Version: "10.5.27-MariaDB"}))
	if len(old) != 2 || old[0] != observe.DatabaseHelp || !strings.Contains(old[1], "10.5.27-MariaDB") || !strings.Contains(old[1], store.MinimumMariaDB) {
		t.Fatalf("old server: %q", old)
	}
	other := databaseOpenHelp(errors.New("Error 1045: Access denied for user 'ac2'@'127.0.0.1' (using password: YES) secret-pass"))
	if len(other) != 1 || other[0] != observe.DatabaseHelp {
		t.Fatalf("other failure: %q", other)
	}
}
