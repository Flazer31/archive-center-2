// Package osfile holds file operations the backend and updater share.
package osfile

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"
)

// retryFor bounds how long Rename keeps trying a briefly held file.
var retryFor = 10 * time.Second

// Rename is os.Rename. On Windows a file or folder just written is often held
// for a moment (an antivirus scan, the indexer), and renaming it then fails
// with "Access is denied" or a sharing violation; Rename tries again for a few
// seconds before returning that error. Elsewhere it is os.Rename unchanged.
func Rename(oldpath, newpath string) error {
	err := os.Rename(oldpath, newpath)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	deadline := time.Now().Add(retryFor)
	for wait := 50 * time.Millisecond; held(err) && time.Now().Before(deadline); {
		time.Sleep(wait)
		if wait < time.Second {
			wait *= 2
		}
		if err = os.Rename(oldpath, newpath); err == nil {
			return nil
		}
	}
	return err
}

// held reports the Windows errors of a file another process has open:
// ERROR_ACCESS_DENIED (5), ERROR_SHARING_VIOLATION (32), ERROR_LOCK_VIOLATION (33).
func held(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == 5 || errno == 32 || errno == 33)
}
