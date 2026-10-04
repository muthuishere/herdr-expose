package serve

import (
	"os"
	"path/filepath"

	"github.com/muthuishere/herdr-expose/internal/platform"
)

// auth.json is written by MORE THAN ONE PROCESS, and that is the whole reason
// this file exists.
//
// `herdr-expose pair` mints a code in the CLI's own process and appends it to
// auth.json. The daemon meanwhile rewrites the same file on EVERY
// authenticated request, to slide the connected device's last-seen forward. It
// wrote from memory, and its memory held no pairing codes — it had read the
// file once at startup and never looked again — so the daemon's next request
// erased the code the CLI had just minted. With a phone connected and polling,
// that next request was milliseconds away, so every `pair` printed a code the
// server then rejected as "no such code". Reported from the owner's phone; the
// state file on the machine had no `pairing` key at all, only devices.
//
// Reloading before each write shrinks the window; it does not close it. Two
// processes can still read, modify and write over one another. So the whole
// read-modify-write is serialised on a lock file beside the state — the one
// thing both processes can agree on without talking to each other.

// commit runs mutate against state freshly read from disk and writes the
// result, holding the cross-process lock for the whole read-modify-write.
//
// Callers hold a.mu, which keeps goroutines in THIS process out; the file lock
// keeps the other process out. Both are needed: neither covers the other's case.
func (a *Auth) commit(mutate func()) error {
	rel, err := a.lockState()
	if err != nil {
		// A lock we cannot take must not make the daemon unable to write at
		// all: fall back to the unlocked path, which is what it always did.
		a.log.Warn("auth: proceeding without the state lock", "err", err)
		a.reloadLocked()
		mutate()
		return a.save()
	}
	defer rel()
	a.reloadLocked()
	mutate()
	return a.save()
}

// holdState takes the cross-process lock for a critical section with several
// exits, and always returns a release. A lock we cannot take must not make the
// daemon unable to write at all, so a failure logs and degrades to the old
// unlocked behaviour rather than refusing.
func (a *Auth) holdState() func() {
	rel, err := a.lockState()
	if err != nil {
		a.log.Warn("auth: proceeding without the state lock", "err", err)
		return func() {}
	}
	return rel
}

func (a *Auth) lockState() (func(), error) {
	p := filepath.Join(filepath.Dir(a.statePath), "auth.lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := platform.LockFile(f, true); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = platform.UnlockFile(f)
		_ = f.Close()
	}, nil
}
