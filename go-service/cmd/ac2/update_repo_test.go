package main

import "testing"

// A released build looks for updates in the project's repository unless the
// environment names another one; "off" is passed on and main turns it into
// "not configured".
func TestUpdateRepoDefault(t *testing.T) {
	for env, want := range map[string]string{"": releaseRepo, "  ": releaseRepo, "someone/fork": "someone/fork", "off": "off"} {
		if got := updateRepoDefault(env); got != want {
			t.Fatalf("env %q: %q, want %q", env, got, want)
		}
	}
	if releaseRepo != "Flazer31/archive-center-2" {
		t.Fatalf("release repository %q", releaseRepo)
	}
}
