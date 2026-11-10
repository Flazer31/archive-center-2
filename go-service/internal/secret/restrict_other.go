//go:build !windows

package secret

import "os"

// Restrict makes a secret file (access token, key file) readable and writable
// by its owner only.
func Restrict(path string) error { return os.Chmod(path, 0o600) }
