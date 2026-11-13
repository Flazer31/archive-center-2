package diagnostics

import (
	"strings"
	"testing"
)

// A DSN loses only its password when the password has no space (the error
// around it stays readable); one with spaces is masked whole; a known
// secret is masked also as it appears in an address.
func TestRedactKeepsErrorAroundCredentials(t *testing.T) {
	got := Redact("2026/11/08 12:00:00 worker: failed chat:A turn 2: Error 1045: denied for user user:private-password@tcp(127.0.0.1:3306)/db")
	if got != "2026/11/08 12:00:00 worker: failed chat:A turn 2: Error 1045: denied for user user:[가림]@tcp(127.0.0.1:3306)/db" {
		t.Fatalf("narrow DSN: %q", got)
	}
	if got := Redact("open user:private spaced password@tcp(127.0.0.1:3306)/db failed"); strings.Contains(got, "spaced password") || !strings.Contains(got, "failed") {
		t.Fatalf("spaced DSN: %q", got)
	}
	if got := Redact("GET http://h/x?t=opaque%2Ftoken%2Bspace done", "opaque/token+space"); strings.Contains(got, "opaque") || !strings.Contains(got, "done") {
		t.Fatalf("escaped secret: %q", got)
	}
}
