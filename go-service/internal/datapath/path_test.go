package datapath

import (
	"path/filepath"
	"testing"
)

func TestPrecedenceAndPlatformDefaults(t *testing.T) {
	base := t.TempDir()
	env := map[string]string{"HOME": filepath.Join(base, "home"), "LOCALAPPDATA": filepath.Join(base, "local"), "XDG_DATA_HOME": filepath.Join(base, "xdg"), "AC2_DATA_DIR": "environment"}
	get := func(k string) string { return env[k] }
	check := func(explicit, platform, want string) {
		t.Helper()
		got, err := ResolveFor(explicit, base, platform, get)
		if err != nil || got != want || !filepath.IsAbs(got) {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	check("explicit", "linux", filepath.Join(base, "explicit"))
	check("", "linux", filepath.Join(base, "environment"))
	delete(env, "AC2_DATA_DIR")
	check("", "windows", filepath.Join(env["LOCALAPPDATA"], "ArchiveCenter2", "data"))
	check("", "darwin", filepath.Join(env["HOME"], "Library", "Application Support", "ArchiveCenter2", "data"))
	check("", "linux", filepath.Join(env["XDG_DATA_HOME"], "archive-center2", "data"))
	delete(env, "XDG_DATA_HOME")
	check("", "linux", filepath.Join(env["HOME"], ".local", "share", "archive-center2", "data"))
	check(filepath.Join(base, "absolute"), "linux", filepath.Join(base, "absolute"))
}

func TestMissingDefaultEnvironment(t *testing.T) {
	base := t.TempDir()
	env := map[string]string{"USERPROFILE": filepath.Join(base, "profile")}
	get := func(k string) string { return env[k] }
	got, err := ResolveFor("", base, "windows", get)
	if err != nil || got != filepath.Join(env["USERPROFILE"], "AppData", "Local", "ArchiveCenter2", "data") {
		t.Fatalf("profile fallback %q: %v", got, err)
	}
	delete(env, "USERPROFILE")
	for _, platform := range []string{"windows", "linux", "darwin"} {
		if _, err := ResolveFor("", base, platform, get); err == nil {
			t.Fatalf("%s silently selected package data", platform)
		}
	}
	env["XDG_DATA_HOME"] = filepath.Join(base, "xdg")
	if _, err := ResolveFor("", base, "linux", get); err != nil {
		t.Fatal(err)
	}
}
