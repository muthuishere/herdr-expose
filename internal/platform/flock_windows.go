//go:build windows

package platform

import (
	"os"

	"golang.org/x/sys/windows"
)

// Windows has no flock. LockFileEx is the equivalent primitive and gives the
// same three properties the callers actually depend on:
//
//   - EXCLUSIVE: LOCKFILE_EXCLUSIVE_LOCK.
//   - NON-BLOCKING on request: LOCKFILE_FAIL_IMMEDIATELY fails with
//     ERROR_LOCK_VIOLATION instead of waiting.
//   - RELEASED ON DEATH: the kernel drops a file lock when the last handle to
//     the file closes, and it closes every handle when the process exits,
//     however it exits. That is the property the pidfile and the share run-lock
//     are built on — a SIGKILLed (here: TerminateProcess'd) holder must leave
//     nothing to clean up.
//
// Unlike flock, LockFileEx locks a byte RANGE, so every caller must agree on
// the range. They do: one byte at offset 0, spelled once, here.
const (
	lockOffsetLow  = 0
	lockOffsetHigh = 0
	lockBytesLow   = 1
	lockBytesHigh  = 0
)

// LockFile takes an exclusive lock on f. See the Unix twin for the contract.
func LockFile(f *os.File, block bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !block {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags,
		0, lockBytesLow, lockBytesHigh, ol)
	if err != nil {
		if !block {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// UnlockFile drops the lock. Closing f drops it too.
func UnlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()),
		0, lockBytesLow, lockBytesHigh, ol)
}
