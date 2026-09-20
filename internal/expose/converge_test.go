package expose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE REAL TEST OF IDEMPOTENCY IS A CRASH IN THE MIDDLE.
//
// "Does not error on a second run" is the easy half. Provisioning a named
// tunnel is four remote steps and two local ones, and the dangerous states are
// the ones BETWEEN them — a tunnel that exists with no DNS record, a DNS
// record pointing at a tunnel whose secret was never written, a record written
// by a process that was killed before it ever ran cloudflared.
//
// Each test below puts the world into one of those states by hand and then
// re-runs provisioning, asserting that it CONVERGES on the correct end state
// rather than erroring, duplicating, or leaving the operator to clean up by
// hand. The mock API counts creates, so "converged" means "reached the right
// state without making a second of anything".

// interrupted state 1: TUNNEL CREATED, DNS NEVER WRITTEN.
//
// This is what a SIGKILL between step 2 and step 4 leaves. The tunnel is real
// and its credentials are on disk; only the record is missing. Re-running must
// reuse the tunnel and finish the job.
func TestConvergesWhenTunnelExistsButDNSIsMissing(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)

	// First pass creates everything...
	p1, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	// ...then the interruption: the DNS record vanishes (or was never made).
	mock.mu.Lock()
	mock.records = map[string]map[string]any{}
	mock.mu.Unlock()

	p2, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err != nil {
		t.Fatalf("re-run after an interrupted provision must converge, got: %v", err)
	}
	if p2.TunnelID != p1.TunnelID {
		t.Fatalf("a second tunnel was created: %s then %s", p1.TunnelID, p2.TunnelID)
	}
	if n := countCalls(mock, "POST /accounts/acct1/cfd_tunnel"); n != 1 {
		t.Fatalf("created the tunnel %d times; find-or-create must find it", n)
	}
	mock.mu.Lock()
	rec, ok := mock.records["herdr.example.com"]
	mock.mu.Unlock()
	if !ok {
		t.Fatal("the missing DNS record was not recreated: the re-run did not converge")
	}
	if rec["content"] != p1.TunnelID+".cfargotunnel.com" {
		t.Fatalf("record points at %v, want the reused tunnel", rec["content"])
	}
}

// interrupted state 2: DNS WRITTEN, CLOUDFLARED NEVER CAME UP.
//
// Everything in the account is correct and nothing local is missing; the
// process just died before or during launch. A re-run must make NO changes at
// all — not a re-POST, not a PATCH of a record that is already right.
func TestConvergesWithNoChangesWhenOnlyTheProcessDied(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)

	if _, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	beforePost := countCalls(mock, "POST /zones/zone123/dns_records")
	beforePatch := countCalls(mock, "PATCH /zones/zone123/dns_records/rec1")

	if _, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := countCalls(mock, "POST /zones/zone123/dns_records"); got != beforePost {
		t.Fatalf("re-created the DNS record (%d -> %d)", beforePost, got)
	}
	if got := countCalls(mock, "PATCH /zones/zone123/dns_records/rec1"); got != beforePatch {
		t.Fatalf("rewrote a record that was already correct (%d -> %d)", beforePatch, got)
	}
	if n := countCalls(mock, "POST /accounts/acct1/cfd_tunnel"); n != 1 {
		t.Fatalf("created the tunnel %d times", n)
	}
}

// interrupted state 3: TUNNEL EXISTS, CREDENTIALS FILE GONE.
//
// The nastiest one, because it is UNRECOVERABLE in place: the tunnel secret is
// issued once, at creation, so a tunnel whose credentials file was lost can
// never be run again. The two deployments answer it differently on purpose.
//
// The PERMANENT deployment refuses and says how to fix it: deleting a tunnel
// other things may point at, on the strength of a missing local file, is not a
// decision this tool makes silently.
func TestOrphanCredentialsAreRefusedForThePermanentDeployment(t *testing.T) {
	_, srv := newMockCF(t)
	opts, red := withMock(t, srv)

	p, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if err := os.Remove(p.CredsPath); err != nil {
		t.Fatal(err)
	}

	_, err = provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err == nil {
		t.Fatal("the permanent deployment must refuse to delete a tunnel it cannot prove is only its own")
	}
	if !strings.Contains(err.Error(), "credentials file") || !strings.Contains(err.Error(), "destroy") {
		t.Fatalf("the refusal must name the problem and the way out, got: %v", err)
	}
}

// ...while a SHARE converges, because `herdr-expose-share-<id>` is derived from
// its own id and can collide with nothing. Leaving a share permanently stuck
// on a crash between two API calls would be strictly worse.
func TestOrphanCredentialsAreRecreatedForAnExclusiveShare(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)
	opts.Exclusive = true
	opts.TunnelName = "herdr-expose-share-abcd1234"
	opts.Comment = ShareDNSComment

	p1, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if err := os.Remove(p1.CredsPath); err != nil {
		t.Fatal(err)
	}

	p2, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err != nil {
		t.Fatalf("a share must converge on a lost tunnel secret, got: %v", err)
	}
	if p2.TunnelID == p1.TunnelID {
		t.Fatal("the unusable tunnel was reused rather than replaced")
	}
	// The orphan was DELETED, not merely abandoned: leaving it behind is the
	// asymmetry this whole exercise is about.
	mock.mu.Lock()
	deleted := strings.Join(mock.deleted, ",")
	mock.mu.Unlock()
	if !strings.Contains(deleted, p1.TunnelID) {
		t.Fatalf("the orphaned tunnel %s was never deleted (deleted: %s)", p1.TunnelID, deleted)
	}
	if _, err := os.Stat(p2.CredsPath); err != nil {
		t.Fatalf("the replacement has no credentials file: %v", err)
	}
	// And the record now points at the replacement, not the deleted tunnel.
	mock.mu.Lock()
	rec := mock.records["herdr.example.com"]
	mock.mu.Unlock()
	if rec["content"] != p2.TunnelID+".cfargotunnel.com" {
		t.Fatalf("DNS still points at the dead tunnel: %v", rec["content"])
	}
}

// interrupted state 4: DESTROY KILLED HALFWAY.
//
// Destroy deletes the DNS record before the tunnel (the tunnel delete is the
// one that can be refused while a connector lingers). A crash in between
// leaves a tunnel with no record. Re-running must finish, and running it a
// third time — with nothing left — must still succeed.
func TestDestroyIsIdempotentAndFinishesAnInterruptedTeardown(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)

	p, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// The interruption: the record is gone, the tunnel is not.
	mock.mu.Lock()
	mock.records = map[string]map[string]any{}
	mock.mu.Unlock()

	ctx := context.Background()
	if err := DestroyCloudflare(ctx, opts, func(string, ...any) {}); err != nil {
		t.Fatalf("destroy after a half-finished teardown: %v", err)
	}
	dnsGone, tunGone, _, err := VerifyGone(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !dnsGone || !tunGone {
		t.Fatalf("teardown did not finish: dns_gone=%v tunnel_gone=%v", dnsGone, tunGone)
	}
	// Twice more, on an already-empty account.
	for i := 0; i < 2; i++ {
		if err := DestroyCloudflare(ctx, opts, func(string, ...any) {}); err != nil {
			t.Fatalf("destroy #%d with nothing left: %v", i+2, err)
		}
	}
	// And the local credentials went with it: teardown is symmetric with
	// creation, including the files creation wrote.
	if _, err := os.Stat(p.CredsPath); !os.IsNotExist(err) {
		t.Fatalf("the credentials file outlived the tunnel: %v", err)
	}
}

// Teardown NEVER removes a record this tool did not create, whatever it is
// pointed at. This is the mechanism that keeps a share's expiry away from the
// permanent deployment's hostname, so it is asserted directly rather than
// inferred from the tags being different strings.
func TestTeardownLeavesForeignRecordsAlone(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)

	if _, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Now a SHARE teardown is aimed at the very same hostname.
	shareOpts := opts
	shareOpts.Comment = ShareDNSComment
	shareOpts.TunnelName = "herdr-expose-share-deadbeef"
	if err := DestroyCloudflare(context.Background(), shareOpts, func(string, ...any) {}); err != nil {
		t.Fatalf("share teardown: %v", err)
	}
	mock.mu.Lock()
	_, stillThere := mock.records["herdr.example.com"]
	_, tunnelThere := mock.tunnels["herdr-expose"]
	mock.mu.Unlock()
	if !stillThere {
		t.Fatal("a share teardown deleted the PERMANENT deployment's DNS record")
	}
	if !tunnelThere {
		t.Fatal("a share teardown deleted the PERMANENT deployment's tunnel")
	}
}

// A provisioning that fails at the DNS step must leave NOTHING behind — and
// then a retry, once the permission is fixed, must succeed cleanly rather than
// tripping over the wreckage of the first attempt.
func TestFailedProvisionRollsBackThenRetrySucceeds(t *testing.T) {
	mock, srv := newMockCF(t)
	opts, red := withMock(t, srv)

	mock.mu.Lock()
	mock.failDNS = true
	mock.mu.Unlock()
	if _, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red); err == nil {
		t.Fatal("a DNS failure must fail the provision")
	}
	mock.mu.Lock()
	leftovers := len(mock.tunnels)
	mock.failDNS = false
	mock.mu.Unlock()
	if leftovers != 0 {
		t.Fatalf("%d tunnel(s) survived a rolled-back provision", leftovers)
	}

	p, err := provisionCloudflare(context.Background(), opts, func(string, ...any) {}, red)
	if err != nil {
		t.Fatalf("retry after a rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p.CredsPath), "config.yml")); err != nil {
		t.Fatalf("the retry did not write the ingress config: %v", err)
	}
}
