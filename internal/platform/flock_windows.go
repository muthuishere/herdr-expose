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
//     are built on — a hard-killed holder must leave nothing to clean up.
//
// Two differences from flock have to be designed around, and the second one bit
// for real on a live Windows box.
//
//  1. LockFileEx locks a byte RANGE, not a file, so every caller must agree on
//     the range. They do: it is spelled once, here.
//
//  2. **A Windows byte-range lock is MANDATORY, not advisory.** flock does not
//     stop anyone reading the file; LockFileEx does. Locking byte 0 therefore
//     made `herdr-expose status` report "pid not running" while the daemon was
//     serving happily three lines further down the same output — os.ReadFile on
//     the pidfile was failing with ERROR_LOCK_VIOLATION against our OWN lock,
//     and the code read that as "no pid".
//
//     So the lock is taken on a byte far past any content a caller will ever
//     write. The range is real and exclusive, and the pid text at offset 0 stays
//     readable by anyone — which restores exactly the flock semantics the
//     callers were written against. (SQLite and bbolt use the same trick for
//     the same reason.)
const (
	// 1<<62: past the end of any real file, and the conventional choice.
	lockOffsetLow  = 0
	lockOffsetHigh = 1 << 30
	lockBytesLow   = 1
	lockBytesHigh  = 0
)

func lockRange() *windows.Overlapped {
	return &windows.Overlapped{
		Offset:     lockOffsetLow,
		OffsetHigh: lockOffsetHigh,
	}
}

// LockFile takes an exclusive lock on f. See the Unix twin for the contract.
func LockFile(f *os.File, block bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !block {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags,
		0, lockBytesLow, lockBytesHigh, lockRange())
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
	return windows.UnlockFileEx(windows.Handle(f.Fd()),
		0, lockBytesLow, lockBytesHigh, lockRange())
}
