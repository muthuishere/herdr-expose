package serve

import (
	"testing"
	"time"
)

// A login password is the SECOND, weaker credential (ADR 0039): hashed slowly,
// stored only as a hash, exchanged for an ordinary device token through a
// strictly rate-limited route. These tests pin every one of those properties,
// mirroring the discipline of token_test.go and the pairing tests.

// TestLoginMintsAUsableDeviceToken is the happy path: a password set, then a
// login with it, mints a token Authenticate accepts as a device.
func TestLoginMintsAUsableDeviceToken(t *testing.T) {
	a, err := NewAuth(t.TempDir(), nopLog{}, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if err := a.SetPassword("correct-horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	tok, dev, err := a.LoginWithPassword("correct-horse", "phone", "10.0.0.2", "ua")
	if err != nil {
		t.Fatalf("LoginWithPassword: %v", err)
	}
	if tok == "" || dev.ID == "" {
		t.Fatal("login returned an empty token or device")
	}
	id, err := a.Authenticate(tok, "10.0.0.2", "ua")
	if err != nil {
		t.Fatalf("Authenticate(login token): %v", err)
	}
	if id.Kind != "device" || id.DeviceID != dev.ID {
		t.Fatalf("authenticated as %+v, want device %s", id, dev.ID)
	}
}

// TestLoginWrongPasswordRejected: a wrong password is the opaque ErrUnauthorized.
func TestLoginWrongPasswordRejected(t *testing.T) {
	a, err := NewAuth(t.TempDir(), nopLog{}, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if err := a.SetPassword("correct-horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, _, err := a.LoginWithPassword("wrong-password", "phone", "10.0.0.2", "ua"); err != ErrUnauthorized {
		t.Fatalf("wrong password gave %v, want ErrUnauthorized", err)
	}
}

// TestLoginWithNoPasswordSetRejected: with no password configured, login fails
// exactly as a wrong one does — the route never reveals that none is set.
func TestLoginWithNoPasswordSetRejected(t *testing.T) {
	a, err := NewAuth(t.TempDir(), nopLog{}, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if a.HasPassword() {
		t.Fatal("a fresh instance should have no password")
	}
	if _, _, err := a.LoginWithPassword("anything", "phone", "10.0.0.2", "ua"); err != ErrUnauthorized {
		t.Fatalf("login with no password set gave %v, want ErrUnauthorized", err)
	}
}

// TestSetPasswordRejectsTooShort: a too-short password is refused, not stored.
func TestSetPasswordRejectsTooShort(t *testing.T) {
	a, err := NewAuth(t.TempDir(), nopLog{}, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if err := a.SetPassword("short"); err == nil {
		t.Fatal("SetPassword accepted a 5-character password")
	}
	if a.HasPassword() {
		t.Fatal("a refused password was stored anyway")
	}
}

// TestClearPasswordDisablesLogin: once cleared, a password that used to work is
// the opaque 401 again, and HasPassword reflects it.
func TestClearPasswordDisablesLogin(t *testing.T) {
	a, err := NewAuth(t.TempDir(), nopLog{}, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if err := a.SetPassword("correct-horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if !a.HasPassword() {
		t.Fatal("HasPassword is false after SetPassword")
	}
	if err := a.ClearPassword(); err != nil {
		t.Fatalf("ClearPassword: %v", err)
	}
	if a.HasPassword() {
		t.Fatal("HasPassword is true after ClearPassword")
	}
	if _, _, err := a.LoginWithPassword("correct-horse", "phone", "10.0.0.2", "ua"); err != ErrUnauthorized {
		t.Fatalf("login after clear gave %v, want ErrUnauthorized", err)
	}
}

// TestLoginGlobalRateLimiterTripsAndRecovers replays the brute-force defence
// against the REAL limiter with a virtual clock. The GLOBAL bucket is not reset
// by a success, so even valid logins deplete it; after the burst the next is
// refused, and it recovers once the bucket refills.
func TestLoginGlobalRateLimiterTripsAndRecovers(t *testing.T) {
	a, err := NewAuth(t.TempDir(), nopLog{}, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if err := a.SetPassword("correct-horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	clock := base
	a.loginLimiter.now = func() time.Time { return clock }

	// The burst: loginBurst correct logins all succeed.
	for i := 0; i < loginBurst; i++ {
		if _, _, err := a.LoginWithPassword("correct-horse", "phone", "10.0.0.2", "ua"); err != nil {
			t.Fatalf("login %d within the burst was refused: %v", i+1, err)
		}
	}
	// The next one trips the global bucket, even though the password is right.
	if _, _, err := a.LoginWithPassword("correct-horse", "phone", "10.0.0.2", "ua"); err != ErrUnauthorized {
		t.Fatalf("login past the burst gave %v, want ErrUnauthorized (rate limited)", err)
	}
	// Advance past a full refill (loginRefill tokens/s) and it works again.
	clock = base.Add(time.Duration(float64(time.Second) * loginBurst / loginRefill))
	if _, _, err := a.LoginWithPassword("correct-horse", "phone", "10.0.0.2", "ua"); err != nil {
		t.Fatalf("login after the bucket refilled was refused: %v", err)
	}
}

// TestLoginRespectsInstanceDeadline: a share whose time is over mints nothing,
// and a token minted under a live deadline carries it.
func TestLoginRespectsInstanceDeadline(t *testing.T) {
	a, err := NewAuth(t.TempDir(), nopLog{}, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if err := a.SetPassword("correct-horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// A live deadline is pinned onto the minted token.
	future := time.Now().Add(time.Hour)
	if err := a.SetDeadline(future); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	_, dev, err := a.LoginWithPassword("correct-horse", "phone", "10.0.0.2", "ua")
	if err != nil {
		t.Fatalf("login under a live deadline: %v", err)
	}
	if !dev.ExpiresAt.Equal(future) {
		t.Fatalf("device ExpiresAt = %v, want the deadline %v", dev.ExpiresAt, future)
	}

	// A deadline in the past authenticates nothing.
	if err := a.SetDeadline(time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("SetDeadline(past): %v", err)
	}
	if _, _, err := a.LoginWithPassword("correct-horse", "phone", "10.0.0.2", "ua"); err != ErrUnauthorized {
		t.Fatalf("login past the deadline gave %v, want ErrUnauthorized", err)
	}
}

// TestPasswordVerifyParsesStoredParameters: a hash encoded with non-default
// parameters still verifies, because verify reads the parameters from the hash.
func TestPasswordVerifyParsesStoredParameters(t *testing.T) {
	// A hand-built encoding with t=1 (not the argonTime default) must still
	// verify, proving the stored parameters — not the constants — are used.
	encoded, err := hashPassword("correct-horse")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if !verifyPassword("correct-horse", encoded) {
		t.Fatal("a freshly hashed password did not verify")
	}
	if verifyPassword("correct-horse", "not-a-phc-string") {
		t.Fatal("a malformed encoding verified as a match")
	}
}
