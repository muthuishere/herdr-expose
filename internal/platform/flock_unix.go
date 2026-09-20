//go:build !windows

package platform

import (
	"os"
	"syscall"
)

// LockFile takes an exclusive advisory lock on f.
//
// block=false returns (false, nil) when someone else holds it — "held
// elsewhere" is an ANSWER, not an error, and every caller treats it as one.
// The lock is released by UnlockFile or, crash-safely, by the kernel when the
// holding process dies however it dies.
func LockFile(f *os.File, block bool) (bool, error) {
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		if !block {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// UnlockFile drops the lock. Closing f drops it too.
func UnlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
