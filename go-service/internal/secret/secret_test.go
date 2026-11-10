package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A sealed key never shows the key, opens again with the same key file
// (also after a restart), and not with another key file or after a change.
func TestBoxSealsWithItsOwnKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	a, err := LoadBox(path)
	if err != nil {
		t.Fatal(err)
	}
	const key = "sk-test-0123456789abcdef"
	s1, err := a.Seal(key)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := a.Seal(key)
	if strings.Contains(s1, key) || strings.Contains(s1, "0123456789") || s1 == s2 {
		t.Fatalf("sealed value shows the key or repeats: %q %q", s1, s2)
	}
	b, err := LoadBox(path) // a restart reads the same key file
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Open(s1); err != nil || got != key {
		t.Fatalf("reopen after restart: %q %v", got, err)
	}
	other, err := LoadBox(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(s1); err == nil {
		t.Fatalf("another key file opened the key")
	}
	if _, err := b.Open(s1[:len(s1)-4] + "AAAA"); err == nil {
		t.Fatalf("a changed value opened")
	}
	if _, err := b.Open(key); err == nil {
		t.Fatalf("a plain value was taken as sealed")
	}
	raw, _ := os.ReadFile(path)
	if len(raw) != 32 {
		t.Fatalf("key file is %d bytes", len(raw))
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBox(path); err == nil {
		t.Fatalf("a damaged key file must not start the backend")
	}
}
