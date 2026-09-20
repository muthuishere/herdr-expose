package expose

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/muthuishere/herdr-expose/internal/platform"
)

// exposeLock is the CROSS-PROCESS singleton for one deployment's tunnel.
//
// In-process idempotency is easy — Manager.Start returns the running tunnel
// rather than a second one. The hole is across processes: the daemon supervises
// cloudflared while the owner types `herdr-expose expose start` in a terminal,
// and without this both provision and both connect. Cloudflare accepts two
// connectors for one named tunnel quite happily, so the failure is silent: two
// supervised processes, two restart loops, and a `stop` that kills one of them.
//
// An advisory lock on a file under the deployment's own state dir fixes it by
// construction, and it is crash-safe in the way a pidfile is not: the kernel
// drops the lock when the holder dies, so a hard-killed daemon leaves nothing
// to clean up and the next start simply takes it. (flock on Unix, LockFileEx
// on Windows -- see internal/platform; both have exactly that property.) A share holds its own lock in
// its own state dir, so shares never contend with the daemon or each other.
type exposeLock struct {
	f    *os.File
	once sync.Once
}

// tryExposeLock takes the lock without blocking. held=false means another
// process owns this deployment's exposure — which is an ANSWER, not an error:
// the caller reports "already running" and succeeds, because running `start`
// twice must be safe.
func tryExposeLock(stateDir string) (l *exposeLock, held bool, err error) {
	if stateDir == "" {
		// No state dir means no deployment identity to be a singleton OF
		// (tests, one-shot probes). Nothing to serialise against.
		return nil, true, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, false, err
	}
	path := filepath.Join(stateDir, "expose.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	held, err = platform.LockFile(f, false)
	if err != nil || !held {
		f.Close()
		return nil, false, nil
	}
	// Best effort, for a human reading the state dir. Never load-bearing: the
	// flock is the truth, and the pid in here may be stale by a microsecond.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return &exposeLock{f: f}, true, nil
}

// release is idempotent.
func (l *exposeLock) release() {
	if l == nil || l.f == nil {
		return
	}
	l.once.Do(func() {
		_ = platform.UnlockFile(l.f)
		_ = l.f.Close()
	})
}

// ExposureHeldElsewhere reports whether another live process already owns this
// deployment's exposure. It takes the lock and immediately drops it, so it is a
// read, not a claim.
func ExposureHeldElsewhere(stateDir string) bool {
	l, held, err := tryExposeLock(stateDir)
	if err != nil || !held {
		return err == nil
	}
	l.release()
	return false
}
