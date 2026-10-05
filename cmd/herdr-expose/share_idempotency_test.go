package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// EVERY VERB, TWICE.
//
// The owner's rule is "make sure we are idempotent — whatever transport they
// want", and idempotency here is not "the second run does not
// error". It is "the second run reaches the same state as the first, and an
// INTERRUPTED first run converges on it too". These tests run each verb twice
// against real state on disk, and several of them kill the state halfway
// first.
//
// Nothing here reaches the network. Both rungs that exist create nothing off
// this machine, and the records from rungs this build no longer has are
// exercised as what they are: paperwork left on disk by an older version,
// which still has to be listable and still has to be wipeable.

// withStateDir points the whole state tree — shares, ledgers, locks — at a
// scratch directory. It is also the safety rail: without it these tests would
// read the real state dir of the daemon serving the owner's live sessions.
func withStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	return dir
}

// seedShare writes a share record the way a create would, without spawning
// anything. PID 0 means "no process", which is the crashed-share case.
func seedShare(t *testing.T, id string, rec *shareRecord) string {
	t.Helper()
	dir, err := shareDirFor(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec.ID = id
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	if rec.ExpiresAt.IsZero() {
		rec.ExpiresAt = time.Now().Add(time.Hour)
	}
	if err := writeShareFile(dir, rec); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ---------------------------------------------------------------- revoke

// `share revoke <id>` twice. The second run finds nothing and succeeds: the
// desired state — that share gone — is already reached.
func TestRevokeTwiceIsSuccess(t *testing.T) {
	withStateDir(t)
	dir := seedShare(t, "aaaa1111", &shareRecord{Mode: string(config.ModeLAN), Port: 0})

	if err := cmdShareRevoke([]string{"aaaa1111", "--json"}); err != nil {
		t.Fatalf("revoke #1: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the state dir survived revoke: %v", err)
	}
	if err := cmdShareRevoke([]string{"aaaa1111", "--json"}); err != nil {
		t.Fatalf("revoke #2 on an already-gone share must succeed, got: %v", err)
	}
}

// `share revoke --all` twice, across a MIX of rungs — which is the case that
// matters, because one rung's teardown must not abort the others.
func TestRevokeAllTwiceAcrossEveryRung(t *testing.T) {
	withStateDir(t)
	seedShare(t, "bbbb1111", &shareRecord{Mode: string(config.ModeLocal)})
	seedShare(t, "bbbb2222", &shareRecord{Mode: string(config.ModeLAN)})
	// Records on rungs this build no longer has: a quick tunnel and a named
	// domain, written by the version before the tunnel was removed, plus the
	// even older ngrok one. They must still be reaped in the same pass — a
	// share this binary can no longer CREATE is still a share it has to be
	// able to WIPE, or an upgrade would strand state forever, and the state it
	// stranded would be a live pairing store.
	seedShare(t, "bbbb3333", &shareRecord{Mode: "quick", URL: "https://x.trycloudflare.com"})
	seedShare(t, "bbbb4444", &shareRecord{Mode: "cloudflare", URL: "https://x.example.com"})
	seedShare(t, "bbbb5555", &shareRecord{Mode: "ngrok", URL: "https://x.ngrok-free.app"})

	for i := 0; i < 2; i++ {
		if err := cmdShareRevoke([]string{"--all", "--json"}); err != nil {
			t.Fatalf("revoke --all #%d: %v", i+1, err)
		}
		recs, err := listShareRecords()
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 0 {
			t.Fatalf("after revoke --all #%d, %d share(s) remain", i+1, len(recs))
		}
	}
}

// A share.json truncated by a crash mid-write used to make its share
// UNREVOCABLE forever: loadShare returned a parse error and revoke refused.
// The interrupted state has to converge like any other.
func TestRevokeWipesAShareWithAnUnreadableRecord(t *testing.T) {
	withStateDir(t)
	dir := seedShare(t, "cccc1111", &shareRecord{Mode: string(config.ModeLAN)})
	if err := os.WriteFile(filepath.Join(dir, "share.json"), []byte("{trunca"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdShareRevoke([]string{"cccc1111", "--json"}); err != nil {
		t.Fatalf("a share with a corrupt record must still be revocable, got: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the corrupt share's state dir survived")
	}
}

// ---------------------------------------------------------------- panic

// `panic` twice. It is the kill switch, so the second run must not fail just
// because the first one worked.
func TestPanicTwiceIsSuccess(t *testing.T) {
	withStateDir(t)
	seedShare(t, "dddd1111", &shareRecord{Mode: string(config.ModeLAN)})
	seedShare(t, "dddd2222", &shareRecord{Mode: "quick", URL: "https://y.trycloudflare.com"})

	for i := 0; i < 2; i++ {
		if err := cmdPanic([]string{"--json"}); err != nil {
			t.Fatalf("panic #%d: %v", i+1, err)
		}
		recs, _ := listShareRecords()
		if len(recs) != 0 {
			t.Fatalf("panic #%d left %d share(s)", i+1, len(recs))
		}
	}
}

// ---------------------------------------------------------------- list / sweep

// `share list` is also the reaper (G5.3), so running it repeatedly must reap
// an expired crashed share ONCE and then be quiet.
func TestListReapsAnExpiredCrashedShareAndThenIsStable(t *testing.T) {
	withStateDir(t)
	seedShare(t, "eeee1111", &shareRecord{
		Mode:      string(config.ModeLAN),
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour), // deadline passed
		PID:       0,                          // and the process is gone
	})

	reaped := sweepShares(nil)
	if len(reaped) != 1 || reaped[0] != "eeee1111" {
		t.Fatalf("the expired crashed share was not reaped: %v", reaped)
	}
	if again := sweepShares(nil); len(again) != 0 {
		t.Fatalf("a second sweep reaped something that was already gone: %v", again)
	}
	if err := cmdShareList([]string{"--json"}); err != nil {
		t.Fatalf("list after reaping: %v", err)
	}
}

// ---------------------------------------------------------------- restore

// `share restore` twice must converge on ONE instance, not spawn a second.
//
// pidAlive cannot close this window — share.json's pid is stale by definition
// at exactly the moment restore looks at it — so liveness is decided by a run
// lock the instance holds for its whole life. This asserts the lock IS the
// answer, including that it survives a stale pid.
func TestRestoreDoesNotSpawnASecondInstance(t *testing.T) {
	withStateDir(t)
	dir := seedShare(t, "ffff1111", &shareRecord{
		Mode:      string(config.ModeLAN),
		ExpiresAt: time.Now().Add(time.Hour),
		PID:       0, // as if the process had died
		State:     "ready",
	})

	// Stand in for the live instance: hold the run lock.
	held, ok := tryShareRunLock(dir)
	if !ok {
		t.Fatal("could not take the run lock")
	}
	defer held.Close()

	if !shareIsRunning(dir) {
		t.Fatal("a share holding its run lock must read as running, whatever share.json's pid says")
	}
	// A second instance must be refused by the kernel, not by a pid check.
	if _, ok := tryShareRunLock(dir); ok {
		t.Fatal("a second instance took the run lock: two processes would race for the port")
	}

	// And the sweep must treat a lock-holding share as alive even though its
	// recorded pid is 0, so it is not torn down underneath itself.
	_, _ = updateShare("ffff1111", func(r *shareRecord) { r.ExpiresAt = time.Now().Add(-time.Minute) })
	if reaped := sweepShares(nil); len(reaped) != 0 {
		t.Fatalf("the sweep reaped a share whose instance is alive: %v", reaped)
	}

	// Once the instance is gone, the lock is free and the share is reapable.
	held.Close()
	if shareIsRunning(dir) {
		t.Fatal("the run lock outlived its holder")
	}
	if reaped := sweepShares(nil); len(reaped) != 1 {
		t.Fatalf("an expired share with no instance must be reaped: %v", reaped)
	}
}

// ---------------------------------------------------------------- extend

// `extend` is cumulative BY DESIGN — that is what the verb means, and
// AMENDMENTS 10 makes it an explicit, logged, revocable act. The idempotency
// that matters is the ceiling: no number of extensions may add up to a share
// that is effectively permanent.
func TestExtendIsCumulativeButBounded(t *testing.T) {
	withStateDir(t)
	seedShare(t, "1111aaaa", &shareRecord{Mode: string(config.ModeLAN), ExpiresAt: time.Now().Add(time.Hour)})

	if err := cmdShareExtend([]string{"1111aaaa", "--hours", "1", "--json"}); err != nil {
		t.Fatal(err)
	}
	rec, err := loadShare("1111aaaa")
	if err != nil {
		t.Fatal(err)
	}
	first := rec.ExpiresAt
	if time.Until(first) < 100*time.Minute {
		t.Fatalf("extend did not push the deadline: %s", time.Until(first))
	}
	if err := cmdShareExtend([]string{"1111aaaa", "--hours", "1", "--json"}); err != nil {
		t.Fatal(err)
	}
	rec, _ = loadShare("1111aaaa")
	if !rec.ExpiresAt.After(first) {
		t.Fatal("a second extend must extend again: the verb is cumulative on purpose")
	}

	// Now hammer it. However many times it is run, a share stays time-boxed.
	for i := 0; i < 5; i++ {
		if err := cmdShareExtend([]string{"1111aaaa", "--days", "300", "--json"}); err != nil {
			t.Fatal(err)
		}
	}
	rec, _ = loadShare("1111aaaa")
	if time.Until(rec.ExpiresAt) > 366*24*time.Hour {
		t.Fatalf("repeated extends produced an effectively permanent share: expires in %s",
			time.Until(rec.ExpiresAt))
	}
}

// ---------------------------------------------------------------- create

// A bare `share` twice is CREATE-SECOND, not refuse: each gets its own free
// port and its own scope, nothing collides, and that is the documented rule.
func TestTwoBareSharesDoNotCollide(t *testing.T) {
	withStateDir(t)
	p1, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	seedShare(t, "3333aaaa", &shareRecord{Mode: string(config.ModeLAN), Port: p1, Session: "s1"})
	seedShare(t, "3333bbbb", &shareRecord{Mode: string(config.ModeLAN), Port: p2, Session: "s1"})
	recs, err := listShareRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("two bare shares must coexist, got %d", len(recs))
	}
}

// ---------------------------------------------------------------- providers

// The rung is decided by the rung FLAGS and nothing else. Every rung is
// requested explicitly, and every one resolves to itself — no case is skipped
// now, because neither rung touches anything off this machine.
func TestRungComesFromTheFlagsAndNothingElse(t *testing.T) {
	withBinaryProbe(t, installed)
	cases := []struct {
		args []string
		want config.Rung
	}{
		{nil, config.RungLAN},
		{[]string{"--lan"}, config.RungLAN},
		{[]string{"--local"}, config.RungLocal},
	}
	for _, tc := range cases {
		f, err := parseShareFlags(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if f.rung() != tc.want {
			t.Fatalf("%v requested rung %s, want %s", tc.args, f.rung(), tc.want)
		}
		res, _, err := resolveShareMode(context.Background(), f, 21999)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got := config.RungOf(res.Mode); got != tc.want {
			t.Fatalf("%v resolved to rung %s (mode %s), want %s", tc.args, got, res.Mode, tc.want)
		}
	}
}

// `--provider` is GONE, and it fails loudly rather than being accepted and
// ignored. There is no transport left to select, so the refusal has to say
// where transports went instead of naming one.
func TestProviderFlagIsRefusedAndPointsAtTheEscapeHatch(t *testing.T) {
	for _, args := range [][]string{
		{"--provider", "ngrok"},
		{"--provider", "cloudflare"},
	} {
		_, err := parseShareFlags(args)
		if err == nil {
			t.Fatalf("%v: --provider was removed and must not be silently accepted", args)
		}
		if !strings.Contains(err.Error(), "cftunnel") {
			t.Fatalf("%v: the refusal must point at the escape hatch, got: %v", args, err)
		}
	}
}

// A share record on a rung this build no longer has must be REFUSED by the
// instance rather than quietly served on the LAN.
//
// This is the upgrade path, and it is the one place where "do something
// sensible" would be wrong. `share restore` runs on every daemon start, so a
// record left by the tunnel build WILL be handed to cmdShareRun. Coming back
// on 192.168.x.x under the same share id would be a different share wearing
// its name: `share list` would print a URL that was public yesterday, the
// device tokens paired against the old origin would not work, and nobody would
// be told why. Refusing names the share and says what to do with it.
func TestAShareOnAWithdrawnRungIsRefusedNotDowngraded(t *testing.T) {
	withStateDir(t)
	for i, mode := range []string{"quick", "cloudflare", "ngrok", "js"} {
		id := fmt.Sprintf("f0f0000%d", i)
		seedShare(t, id, &shareRecord{Mode: mode, Port: 0, Session: "s1"})
		err := cmdShareRun([]string{"--id", id})
		if err == nil {
			t.Fatalf("mode %q: a withdrawn rung was served anyway", mode)
		}
		for _, want := range []string{"no longer has", "share revoke"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("mode %q: the refusal does not mention %q: %v", mode, want, err)
			}
		}
		// And it is still wipeable, which is the other half of the contract.
		if err := cmdShareRevoke([]string{id, "--json"}); err != nil {
			t.Fatalf("mode %q: a withdrawn-rung share must still be revocable: %v", mode, err)
		}
	}
}

// ---------------------------------------------------------------- service

// A supervised unit MUST set HERDR_EXPOSE_SUPERVISED, or `service install`
// silently disables the product.
//
// The mechanism is worth stating because the failure was invisible: cmdServe
// refuses to start when a service manager is in charge UNLESS this variable
// says "you ARE the supervised copy". A unit without it loads fine, runs
// `serve`, hits that guard, exits 0 — and KeepAlive restarts it forever, while
// every other start path (including the plugin's own daemon hook) now refuses
// because a manager is nominally in charge. Nothing is serving, and nothing
// reports an error.
func TestSupervisedUnitsMarkThemselvesAsSupervised(t *testing.T) {
	const exe = "/opt/herdr/bin/herdr-expose"
	home, state := "/Users/test", "/Users/test/.local/state/herdr-expose"

	for name, unit := range map[string]string{
		"launchd": launchAgentPlist(exe, home, state),
		"systemd": systemdUnit(exe, home, state),
	} {
		if !strings.Contains(unit, "HERDR_EXPOSE_SUPERVISED") {
			t.Fatalf("the %s unit does not mark itself supervised; `service install` would "+
				"silently disable the product:\n%s", name, unit)
		}
		// The guard in cmdServe compares against "", so the value must be
		// non-empty; "1" is what both templates use.
		if !strings.Contains(unit, "HERDR_EXPOSE_SUPERVISED=1") &&
			!strings.Contains(unit, "<key>HERDR_EXPOSE_SUPERVISED</key><string>1</string>") {
			t.Fatalf("the %s unit sets the variable but not to a non-empty value:\n%s", name, unit)
		}
		// It must also pass the state dir through, or the supervised copy
		// reads a different state tree from every CLI invocation.
		if !strings.Contains(unit, state) {
			t.Fatalf("the %s unit does not pin HERDR_PLUGIN_STATE_DIR to %s", name, state)
		}
		if !strings.Contains(unit, exe) {
			t.Fatalf("the %s unit does not exec the resolved binary path", name)
		}
	}
}

// `service status` is read-only and must never claim more than it knows.
func TestServiceStateHealthyRequiresARunningProcess(t *testing.T) {
	cases := []struct {
		st   serviceState
		want bool
	}{
		{serviceState{}, false},
		{serviceState{Installed: true}, false},
		{serviceState{Loaded: true}, false},
		{serviceState{Loaded: true, Active: true}, false}, // active but no pid: not proven
		{serviceState{Loaded: true, Active: true, PID: 42}, true},
	}
	for _, tc := range cases {
		if got := tc.st.Healthy(); got != tc.want {
			t.Fatalf("Healthy(%+v) = %v, want %v — a second install must only be a "+
				"no-op when something is genuinely running", tc.st, got, tc.want)
		}
	}
}
