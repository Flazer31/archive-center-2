// Package datapath resolves persistent storage without creating or reading files.
package datapath

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Resolve applies explicit -data, AC2_DATA_DIR, then the per-user OS default.
// Relative overrides are anchored at base, which belongs to the caller.
func Resolve(explicit, base string) (string, error) {
	return ResolveFor(explicit, base, runtime.GOOS, os.Getenv)
}

// ResolveFor keeps platform/environment selection pure and testable.
func ResolveFor(explicit, base, platform string, getenv func(string) string) (string, error) {
	chosen := explicit
	if chosen == "" {
		chosen = getenv("AC2_DATA_DIR")
	}
	if chosen == "" {
		home := getenv("HOME")
		switch platform {
		case "windows":
			local := getenv("LOCALAPPDATA")
			if local == "" {
				profile := getenv("USERPROFILE")
				if profile == "" {
					return "", fmt.Errorf("cannot resolve shared data directory: LOCALAPPDATA and USERPROFILE are empty")
				}
				local = filepath.Join(profile, "AppData", "Local")
			}
			chosen = filepath.Join(local, "ArchiveCenter2", "data")
		case "darwin":
			if home == "" {
				return "", fmt.Errorf("cannot resolve shared data directory: HOME is empty")
			}
			chosen = filepath.Join(home, "Library", "Application Support", "ArchiveCenter2", "data")
		default:
			xdg := getenv("XDG_DATA_HOME")
			if xdg == "" {
				if home == "" {
					return "", fmt.Errorf("cannot resolve shared data directory: HOME and XDG_DATA_HOME are empty")
				}
				xdg = filepath.Join(home, ".local", "share")
			}
			chosen = filepath.Join(xdg, "archive-center2", "data")
		}
	}
	if !filepath.IsAbs(chosen) {
		chosen = filepath.Join(base, chosen)
	}
	return filepath.Abs(chosen)
}
