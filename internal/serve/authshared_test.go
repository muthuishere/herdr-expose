package serve

import (
	"testing"
	"time"
)

type nopLog struct{}

func (nopLog) Info(string, ...any)  {}
func (nopLog) Warn(string, ...any)  {}
func (nopLog) Debug(string, ...any) {}

// Two Auth instances over one state dir are the real deployment: `herdr-expose
// pair` mints in the CLI's process, the daemon redeems. The daemon rewrote the
// file from its own memory on every authenticated request, so the code the CLI
// had just written was erased before the owner could type it — every pair
// printed a code the server answered with "no such code".
func TestACodeMintedByTheCLIIsRedeemableByTheDaemon(t *testing.T) {
	dir := t.TempDir()
	cli, err := NewAuth(dir, nopLog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := NewAuth(dir, nopLog{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The daemon has a device and is serving it, which is what drove the
	// clobbering write. Pair it first, the way the owner's phone already was.
	seed, _, err := daemon.NewPairingCode("phone")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := daemon.RedeemPairing(seed, "phone", "10.0.0.2", "ua")
	if err != nil {
		t.Fatal(err)
	}

	// Now the CLI mints a code in its own process...
	code, _, err := cli.NewPairingCode("laptop")
	if err != nil {
		t.Fatal(err)
	}

	// ...while the daemon keeps serving the paired phone. Each of these used
	// to rewrite auth.json from memory and drop the pairing list.
	for i := 0; i < 5; i++ {
		if _, err := daemon.Authenticate(tok, "10.0.0.2", "ua"); err != nil {
			t.Fatalf("serving the paired device failed: %v", err)
		}
	}

	if _, _, err := daemon.RedeemPairing(code, "laptop", "10.0.0.3", "ua"); err != nil {
		t.Fatalf("the daemon rejected a code the CLI minted: %v", err)
	}
}

// And the reverse direction: a device the daemon pairs must survive the CLI
// writing to the same file, or redeeming a code would log the phone out.
func TestTheCLIsWriteDoesNotDropTheDaemonsDevices(t *testing.T) {
	dir := t.TempDir()
	daemon, err := NewAuth(dir, nopLog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := NewAuth(dir, nopLog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := daemon.NewPairingCode("phone")
	tok, _, err := daemon.RedeemPairing(code, "phone", "10.0.0.2", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.NewPairingCode("laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Authenticate(tok, "10.0.0.2", "ua"); err != nil {
		t.Fatalf("the paired device stopped working after a CLI mint: %v", err)
	}
	fresh, err := NewAuth(dir, nopLog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Authenticate(tok, "10.0.0.2", "ua"); err != nil {
		t.Fatalf("the device did not survive on disk: %v", err)
	}
}

// Last-seen is persisted at most once a minute now; in memory it is always
// current. The write storm it replaced is what erased pairing codes.
func TestLastSeenIsNotWrittenOnEveryRequest(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAuth(dir, nopLog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := a.NewPairingCode("phone")
	tok, _, err := a.RedeemPairing(code, "phone", "10.0.0.2", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if LastSeenPersistInterval < time.Second {
		t.Fatalf("interval %v is too small to be a throttle", LastSeenPersistInterval)
	}
	for i := 0; i < 20; i++ {
		if _, err := a.Authenticate(tok, "10.0.0.9", "ua"); err != nil {
			t.Fatal(err)
		}
	}
	// A code minted mid-stream still redeems: nothing stamped over it.
	c2, _, _ := a.NewPairingCode("laptop")
	if _, _, err := a.RedeemPairing(c2, "laptop", "10.0.0.3", "ua"); err != nil {
		t.Fatalf("code minted during a request stream was lost: %v", err)
	}
}
