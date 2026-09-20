package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// EVERY VERB, TWICE.
//
// The owner's rule is "make sure we are idempotent — cloudflared or ngrok,
// whatever they want", and idempotency here is not "the second run does not
// error". It is "the second run reaches the same state as the first, and an
// INTERRUPTED first run converges on it too". These tests run each verb twice
// against real state on disk, and several of them kill the state halfway
// first.
//
// Nothing here touches a real account: every case is a rung that creates
// nothing (local / lan / quick / ngrok), or a record whose Cloudflare half is
// asserted through classification rather than executed.

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
	seedShare(t, "bbbb3333", &shareRecord{Mode: string(config.ModeQuick), URL: "https://x.trycloudflare.com"})
	seedShare(t, "bbbb4444", &shareRecord{Mode: string(config.ModeQuick), Provider: "ngrok",
		URL: "https://x.ngrok-free.app"})
	seedShare(t, "bbbb5555", &shareRecord{Mode: string(config.ModeNgrok), Provider: "ngrok",
		Domain: "reserved.example.ngrok.app"})

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
	state := withStateDir(t)
	seedShare(t, "dddd1111", &shareRecord{Mode: string(config.ModeLAN)})
	seedShare(t, "dddd2222", &shareRecord{Mode: string(config.ModeQuick), URL: "https://y.trycloudflare.com"})

	for i := 0; i < 2; i++ {
		if err := cmdPanic([]string{"--json"}); err != nil {
			t.Fatalf("panic #%d: %v", i+1, err)
		}
		if _, err := os.Stat(haltFilePath(state)); err != nil {
			t.Fatalf("panic #%d did not leave the halt flag: %v", i+1, err)
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

// `share --domain X` twice: REFUSE, deterministically, and say how to resolve
// it. Two shares on one hostname is the worst outcome — the second repoints
// the DNS record onto its own tunnel, silently breaking the first, and the
// first revoke then deletes the record out from under the other.
func TestSecondShareOnTheSameDomainIsRefused(t *testing.T) {
	withStateDir(t)
	seedShare(t, "2222aaaa", &shareRecord{
		Mode: string(config.ModeCloudflare), Domain: "hex-idem-test.example.com",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	if owner := hostnameOwner("hex-idem-test.example.com", ""); owner != "2222aaaa" {
		t.Fatalf("the live share on that hostname was not found: %q", owner)
	}
	// Case-insensitively, because hostnames are.
	if owner := hostnameOwner("HEX-IDEM-TEST.example.com", ""); owner != "2222aaaa" {
		t.Fatalf("hostname matching is case sensitive: %q", owner)
	}
	// A different hostname is not a collision.
	if owner := hostnameOwner("something-else.example.com", ""); owner != "" {
		t.Fatalf("an unrelated hostname reported a collision: %q", owner)
	}
	// And an EXPIRED share is not a collision either: it is about to be
	// reaped, and blocking the hostname on its residue would be wrong.
	_, _ = updateShare("2222aaaa", func(r *shareRecord) { r.ExpiresAt = time.Now().Add(-time.Minute) })
	if owner := hostnameOwner("hex-idem-test.example.com", ""); owner != "" {
		t.Fatal("an expired share must not block its hostname forever")
	}
}

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
	// Neither has a hostname, so neither can collide with the other.
	for _, r := range recs {
		if r.Domain != "" {
			t.Fatalf("a LAN share carries a hostname it never created: %+v", r)
		}
	}
}

// ---------------------------------------------------------------- providers

// The ladder is provider-independent. `--provider ngrok` must land on exactly
// the same rung as its cloudflare twin, and must never move a share up or down
// it — the point of AMENDMENTS 16/17 is REACH, and reach cannot depend on
// which binary happens to be installed.
func TestProviderChoiceNeverChangesTheRung(t *testing.T) {
	withBinaryProbe(t, installed)
	cases := []struct {
		args []string
		want config.Rung
	}{
		{nil, config.RungLAN},
		{[]string{"--provider", "ngrok"}, config.RungLAN},
		{[]string{"--local", "--provider", "ngrok"}, config.RungLocal},
		{[]string{"--quick"}, config.RungQuick},
		{[]string{"--quick", "--provider", "ngrok"}, config.RungQuick},
		{[]string{"--domain", "x.example.ngrok.app", "--provider", "ngrok"}, config.RungDomain},
	}
	for _, tc := range cases {
		f, err := parseShareFlags(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if f.rung() != tc.want {
			t.Fatalf("%v requested rung %s, want %s", tc.args, f.rung(), tc.want)
		}
		res, exp, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got := config.RungOf(res.Mode); got != tc.want {
			t.Fatalf("%v resolved to rung %s (mode %s), want %s", tc.args, got, res.Mode, tc.want)
		}
		if f.isNgrok() && (exp.Cloudflare || (exp.Domain != "" && !exp.Ngrok)) {
			t.Fatalf("%v was handed a cloudflare table: %+v", tc.args, exp)
		}
	}
}

// An unknown provider is refused at parse time, before anything is created.
func TestUnknownProviderIsRefused(t *testing.T) {
	if _, err := parseShareFlags([]string{"--provider", "tailscale"}); err == nil ||
		!strings.Contains(err.Error(), "cloudflare") {
		t.Fatalf("an unknown provider must be refused with the list of real ones, got: %v", err)
	}
}

// An ngrok share creates NOTHING that this tool may delete — a reserved domain
// belongs to the user's ngrok account — so teardown must classify it as such
// rather than running the Cloudflare path and no-opping.
func TestNgrokShareHasNoCloudflareTeardown(t *testing.T) {
	for _, rec := range []*shareRecord{
		{Mode: string(config.ModeNgrok), Provider: "ngrok", Domain: "r.example.ngrok.app"},
		{Mode: string(config.ModeQuick), Provider: "ngrok", URL: "https://q.ngrok-free.app"},
	} {
		if rec.hasCloudflareResources() {
			t.Fatalf("an ngrok share must not be given a Cloudflare teardown: %+v", rec)
		}
		if rec.provider() != "ngrok" {
			t.Fatalf("provider = %q", rec.provider())
		}
	}
	// ...and the cloudflare domain rung still does.
	cf := &shareRecord{Mode: string(config.ModeCloudflare), Domain: "s.example.com"}
	if !cf.hasCloudflareResources() {
		t.Fatal("a cloudflare domain share MUST still delete its record and tunnel")
	}
	// An old record with no provider field is a cloudflare one.
	legacy := &shareRecord{Mode: string(config.ModeCloudflare), Domain: "s.example.com"}
	if legacy.provider() != "cloudflare" || !legacy.hasCloudflareResources() {
		t.Fatal("a record written before providers existed must mean cloudflare")
	}
}
