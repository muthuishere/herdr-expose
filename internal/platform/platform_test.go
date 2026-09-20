package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These run on every platform, because the CONTRACT is the same everywhere and
// that is the only thing the callers depend on. The implementations diverge
// completely; the behaviour must not.

// TestLockFileExcludesASecondHolder is the one that matters most. Both the
// pidfile and the share run-lock are built on "the second taker is told no",
// and being wrong here means two servers on one port.
func TestLockFileExcludesASecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	held, err := LockFile(a, false)
	if err != nil || !held {
		t.Fatalf("first holder did not get the lock: held=%v err=%v", held, err)
	}

	b, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	held, err = LockFile(b, false)
	if err != nil {
		t.Fatalf("second taker errored instead of being refused: %v", err)
	}
	if held {
		t.Fatal("second taker got the lock while the first still holds it")
	}

	// ...and releasing hands it over. A lock that cannot be handed over is a
	// deadlock dressed up as safety.
	if err := UnlockFile(a); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	held, err = LockFile(b, false)
	if err != nil || !held {
		t.Fatalf("lock was not released: held=%v err=%v", held, err)
	}
	_ = UnlockFile(b)
}

// TestLockFileReleasedOnClose covers the crash-safety property: every caller
// relies on the kernel dropping the lock when the holder's handles go, because
// that is what makes a SIGKILLed daemon leave nothing to clean up.
func TestLockFileReleasedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "y.lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := LockFile(a, false); err != nil || !held {
		t.Fatalf("first lock: held=%v err=%v", held, err)
	}
	a.Close() // no explicit unlock, on purpose

	b, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	held, err := LockFile(b, false)
	if err != nil || !held {
		t.Fatalf("closing the holder did not release the lock: held=%v err=%v", held, err)
	}
	_ = UnlockFile(b)
}

// A held lock must not stop anyone READING the file. flock is advisory and
// never did; LockFileEx is mandatory and does, unless the lock is taken off the
// end of the content -- which is why this test exists. Getting it wrong made
// `status` report "pid not running" against a running daemon, because reading
// our own pidfile failed with ERROR_LOCK_VIOLATION.
func TestLockedFileIsStillReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "z.pid")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if held, err := LockFile(f, false); err != nil || !held {
		t.Fatalf("lock: held=%v err=%v", held, err)
	}
	defer UnlockFile(f)
	if _, err := f.WriteAt([]byte("4242\n"), 0); err != nil {
		t.Fatalf("holder could not write its own pid: %v", err)
	}

	// A DIFFERENT handle, as another process would have.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a held lock blocked a plain read of the file: %v", err)
	}
	if strings.TrimSpace(string(b)) != "4242" {
		t.Errorf("read back %q, want 4242", b)
	}
}

func TestStateAndConfigDirsAreAbsoluteAndNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(string) (string, error)
	}{
		{"StateDir", StateDir},
		{"ConfigDir", ConfigDir},
	} {
		got, err := tc.fn("herdr-expose")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s returned a relative path %q", tc.name, got)
		}
		if !strings.Contains(got, "herdr-expose") {
			t.Errorf("%s(%q) = %q, which does not name the app", tc.name, "herdr-expose", got)
		}
	}
}

func TestHerdrConfigDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))
	got, err := HerdrConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "herdr" {
		t.Errorf("HerdrConfigDir() = %q, want it to end in herdr", got)
	}
	if !strings.Contains(got, "xdg") {
		t.Errorf("HerdrConfigDir() = %q, ignored XDG_CONFIG_HOME", got)
	}
}

func TestAliveOnSelfAndOnNothing(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(self) = false")
	}
	if Alive(0) || Alive(-1) {
		t.Error("Alive() said yes to a non-pid")
	}
}

func TestProcessesMatchingNeedsNeedles(t *testing.T) {
	if got := ProcessesMatching(); got != nil {
		t.Errorf("ProcessesMatching() with no needles = %v, want nil", got)
	}
	// An empty needle must never match everything: this function's output is
	// fed straight into a kill.
	if got := ProcessesMatching(""); len(got) != 0 {
		t.Errorf("ProcessesMatching(%q) matched %d processes; it must match none", "", len(got))
	}
}
