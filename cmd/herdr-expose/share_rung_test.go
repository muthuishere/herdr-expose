package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// AMENDMENTS 16 — least exposure by default. The ladder is climbed, never
// guessed.
//
// These tests exist because the difference between the default rung and the
// one above it is the difference between "this room" and "the entire
// internet", for a binary that runs arbitrary commands inside the owner's
// agent sessions. Every assertion below is about REACH, and the two directions
// are tested separately: a failed explicit ask may degrade DOWN, and nothing
// may ever climb UP.

// isZeroExpose reports whether nothing at all is switched on in an [expose]
// table — which is what makes a share local BY CONSTRUCTION: no provider to
// build, no domain to write DNS for, no 0.0.0.0 bind.
func isZeroExpose(e config.Expose) bool {
	return !e.Cloudflare && !e.Ngrok && !e.Quick && !e.LAN && !e.Autostart &&
		e.Domain == "" && e.Adapter == "" && len(e.Adapters) == 0
}

// installed / missing are stand-ins for the real `which cloudflared`, so the
// fallback behaviour is testable on a machine in either state.
func installed(string) bool { return true }
func missing(string) bool   { return false }

// withBinaryProbe swaps the helper-binary probe for the duration of a test.
func withBinaryProbe(t *testing.T, probe func(string) bool) {
	t.Helper()
	prev := shareBinaryFound
	shareBinaryFound = probe
	t.Cleanup(func() { shareBinaryFound = prev })
}

// ------------------------------------------------------------ the default

// L1: no flag means LAN. A bare `share` binds 0.0.0.0 — which covers loopback
// AND the wifi — starts NO tunnel and creates NO DNS. It does that WITHOUT
// reading a domain from anywhere, so there is no key that can turn a bare
// share into a public one.
//
// LAN rather than loopback because a share exists to be OPENED FROM SOMEWHERE
// ELSE: at this machine you would just use the daemon on localhost:21118, so a
// loopback-only default would make the feature pointless.
func TestBareShareIsLANAndRaisesNoTunnel(t *testing.T) {
	withBinaryProbe(t, installed) // cloudflared available: still irrelevant

	f, err := parseShareFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.namedARung() {
		t.Fatal("a bare `share` must not count as having named a rung")
	}
	if f.rung() != config.RungLAN {
		t.Fatalf("requested rung = %s, want lan", f.rung())
	}

	res, exp, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
	if err != nil {
		t.Fatalf("resolve a bare share: %v", err)
	}
	if res.Mode != config.ModeLAN {
		t.Fatalf("mode = %s, want lan — a share nobody asked to publish must not be published", res.Mode)
	}
	if res.Bind != config.BindAll {
		t.Fatalf("bind = %s, want %s", res.Bind, config.BindAll)
	}
	if !strings.HasPrefix(res.URL, "http://") {
		t.Fatalf("url = %s, want a plain-HTTP LAN address", res.URL)
	}
	if res.Remote {
		t.Fatal("a LAN share is not remote: there is no tunnel to run")
	}
	// The [expose] table handed to the instance is what decides whether a
	// tunnel process is ever started and whether a DNS record is ever written.
	// For a LAN share it must switch on nothing but the bind.
	if !exp.LAN || exp.Quick || exp.Cloudflare || exp.Ngrok || exp.Domain != "" || exp.Adapter != "" {
		t.Fatalf("a bare share was handed a tunnel-capable [expose] table (%+v)", exp)
	}
	// And the record written from it asks for no Cloudflare teardown, because
	// nothing was ever created.
	rec := &shareRecord{Mode: string(res.Mode), Port: 21999}
	if !rec.isLAN() || rec.hasCloudflareResources() {
		t.Fatalf("a LAN share must have no DNS/tunnel teardown: %+v", rec)
	}
	if rec.rung() != config.RungLAN || rec.rung().Reach() != "anyone on this network" {
		t.Fatalf("a LAN share must state its reach as this network, got %q", rec.rung().Reach())
	}
}

// `--local` is the explicit opt-IN: loopback only, for testing. It is no
// longer the default, but it must still be a real, complete rung.
func TestExplicitLocalIsLoopbackOnly(t *testing.T) {
	withBinaryProbe(t, installed)

	f, err := parseShareFlags([]string{"--local"})
	if err != nil {
		t.Fatal(err)
	}
	if f.rung() != config.RungLocal {
		t.Fatalf("requested rung = %s, want local", f.rung())
	}
	res, exp, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != config.ModeLocal || res.Bind != config.BindLoopback {
		t.Fatalf("--local resolved to %s on %s", res.Mode, res.Bind)
	}
	if want := "http://127.0.0.1:21999"; res.URL != want {
		t.Fatalf("url = %s, want %s", res.URL, want)
	}
	if !isZeroExpose(exp) {
		t.Fatalf("--local was handed a non-empty [expose] table (%+v): "+
			"something in it would start a tunnel or bind the network", exp)
	}
	rec := &shareRecord{Mode: string(res.Mode), Port: 21999}
	if !rec.isLocal() || rec.hasCloudflareResources() {
		t.Fatalf("a local share must have no DNS/tunnel teardown: %+v", rec)
	}
	if got := rec.CurrentURL(); got != "http://127.0.0.1:21999" {
		t.Fatalf("record url = %s, want loopback", got)
	}
	if rec.rung().Reach() != "this machine only" {
		t.Fatalf("reach = %q", rec.rung().Reach())
	}
}

// The exact regression AMENDMENTS 16 was written for: the owner's own config
// has `[expose] domain = "herdr.deemwar.com"` for the PERMANENT deployment,
// and under the withdrawn AMENDMENTS 15 ladder a bare `share` therefore
// resolved to a public hostname. A config key set for something else must
// never publish a session.
func TestBareShareStaysOnTheLANWithAConfiguredDomain(t *testing.T) {
	withBinaryProbe(t, installed)
	writeTestConfig(t, `
[expose]
cloudflare = true
domain = "herdr.example.com"

[share]
default_hours = 1
default_mode = "lan"
max_concurrent = 10
`)
	f, err := parseShareFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.applyConfigDefaults(); err != nil {
		t.Fatal(err)
	}
	if f.quick || f.local || strings.TrimSpace(f.domain) != "" {
		t.Fatalf("a configured [expose] domain leaked into a bare share: %+v", f)
	}
	res, exp, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != config.ModeLAN || exp.Cloudflare || exp.Quick || exp.Domain != "" {
		t.Fatalf("a bare share was promoted to %s because a domain was configured", res.Mode)
	}
	if res.Remote {
		t.Fatal("a bare share raised a tunnel")
	}
}

// The withdrawn "auto" still loads — an existing config file must not break —
// but it now means LAN, never the old domain -> quick -> lan ladder. The
// owner's real config says default_mode = "auto" AND has an [expose] domain
// for the permanent deployment, which is exactly the combination that used to
// publish a bare share.
func TestLegacyAutoDefaultModeMeansLAN(t *testing.T) {
	withBinaryProbe(t, installed)
	writeTestConfig(t, `
[expose]
cloudflare = true
domain = "herdr.example.com"

[share]
default_hours = 1
default_mode = "auto"
max_concurrent = 10
`)
	if got := config.ShareRung("auto"); got != config.RungLAN {
		t.Fatalf(`ShareRung("auto") = %s, want lan`, got)
	}
	f, err := parseShareFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.applyConfigDefaults(); err != nil {
		t.Fatal(err)
	}
	if f.rung() != config.RungLAN {
		t.Fatalf(`default_mode = "auto" resolved to %s, want lan`, f.rung())
	}
	res, _, err := resolveShareMode(context.Background(), f, 21999, "t")
	if err != nil {
		t.Fatal(err)
	}
	if res.Remote || res.Mode != config.ModeLAN {
		t.Fatalf(`default_mode = "auto" raised %s`, res.Mode)
	}
}

// A domain_suffix is a NAMING convention. On its own it must not be what
// promotes a share to the public internet — that was the other half of the
// withdrawn auto ladder.
func TestDomainSuffixAloneDoesNotPublish(t *testing.T) {
	withBinaryProbe(t, installed)
	writeTestConfig(t, `
[share]
domain_suffix = "share.example.com"
default_hours = 1
default_mode = "lan"
max_concurrent = 10
`)
	f, err := parseShareFlags([]string{"--name", "review"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.applyConfigDefaults(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(f.domain) != "" {
		t.Fatalf("domain_suffix published a bare share on %q", f.domain)
	}
	if f.rung() != config.RungLAN {
		t.Fatalf("rung = %s, want lan", f.rung())
	}
}

// ---------------------------------------------------------- never escalate

// L2, upward: a `--lan` request must NEVER become a tunnel — not when
// cloudflared is installed, not when a domain is configured, not ever. Someone
// who asked for the wifi did not ask for the internet.
func TestLanRequestNeverBecomesATunnel(t *testing.T) {
	withBinaryProbe(t, installed) // every tunnel is available; none may be used
	writeTestConfig(t, `
[expose]
cloudflare = true
domain = "herdr.example.com"

[share]
default_hours = 1
default_mode = "quick"
max_concurrent = 10
`)
	f, err := parseShareFlags([]string{"--lan"})
	if err != nil {
		t.Fatal(err)
	}
	// Even the config's own default_mode = "quick" must not touch an explicit
	// flag: a named rung is a request, and a request is never overridden.
	if err := f.applyConfigDefaults(); err != nil {
		t.Fatal(err)
	}
	if f.quick {
		t.Fatal("[share] default_mode overrode an explicit --lan")
	}

	res, exp, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != config.ModeLAN {
		t.Fatalf("--lan resolved to %s: a LAN request was escalated to a tunnel", res.Mode)
	}
	if res.Remote || exp.Quick || exp.Cloudflare || exp.Ngrok || exp.Domain != "" {
		t.Fatalf("--lan was handed a tunnel-capable [expose] table: %+v", exp)
	}
	if config.RungOf(res.Mode) > config.RungLAN {
		t.Fatalf("resolved rung %s is above the requested lan", config.RungOf(res.Mode))
	}
}

// The invariant itself, over the WHOLE table rather than one case: for every
// rung a user can ask for, in both environments (cloudflared present and
// absent), the resolved reach is never greater than the requested reach.
func TestResolveNeverEscalates(t *testing.T) {
	writeTestConfig(t, `
[expose]
cloudflare = true
domain = "herdr.example.com"

[share]
default_hours = 1
default_mode = "domain"
max_concurrent = 10
`)
	for _, probe := range []struct {
		name string
		fn   func(string) bool
	}{{"cloudflared installed", installed}, {"cloudflared missing", missing}} {
		cases := [][]string{nil, {"--local"}, {"--lan"}, {"--quick"}}
		if !probe.fn("cloudflared") {
			// --domain is only exercised in the no-cloudflared environment:
			// with cloudflared present it would reach the real Cloudflare API
			// for the zone check, and a unit test must not do that. Its
			// escalation direction is covered anyway — RungDomain is the top
			// of the ladder, so it has nothing above it to escalate INTO.
			cases = append(cases, []string{"--domain", "s.example.com"})
		}
		for _, args := range cases {
			name := fmt.Sprintf("%s/%v", probe.name, args)
			t.Run(name, func(t *testing.T) {
				withBinaryProbe(t, probe.fn)
				f, err := parseShareFlags(args)
				if err != nil {
					t.Fatal(err)
				}
				res, _, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				want, got := f.rung(), config.RungOf(res.Mode)
				if got > want {
					t.Fatalf("requested %s (%s) but resolved %s (%s) — exposure escalated",
						want, want.Reach(), got, got.Reach())
				}
			})
		}
	}
}

// ------------------------------------------------------- degrade downward

// L2, downward: an EXPLICIT --quick with no cloudflared degrades to lan and
// says why. The person did ask to be reachable, and a LAN address is the
// nearest honest answer — but it is still strictly less reach than they asked
// for, and it is announced rather than silent.
func TestQuickFallsBackToLANWhenCloudflaredIsMissing(t *testing.T) {
	withBinaryProbe(t, missing)

	f, err := parseShareFlags([]string{"--quick"})
	if err != nil {
		t.Fatal(err)
	}
	res, exp, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
	if err != nil {
		t.Fatalf("--quick with no cloudflared must degrade, not fail: %v", err)
	}
	if res.Mode != config.ModeLAN {
		t.Fatalf("mode = %s, want lan", res.Mode)
	}
	if !exp.LAN || exp.Quick || exp.Cloudflare {
		t.Fatalf("the fallback [expose] table is not a pure LAN one: %+v", exp)
	}
	if res.FellBack == "" || !strings.Contains(res.FellBack, "quick tunnel") {
		t.Fatalf("the downgrade was silent or unexplained: %q", res.FellBack)
	}
	if config.RungOf(res.Mode) >= config.RungQuick {
		t.Fatal("a degrade must land BELOW the requested rung")
	}
}

// The same for --domain: a missing cloudflared is an environment problem, not
// a reason to fail the person who asked to be reachable.
func TestDomainFallsBackToLANWhenCloudflaredIsMissing(t *testing.T) {
	withBinaryProbe(t, missing)

	f, err := parseShareFlags([]string{"--domain", "s.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	res, exp, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
	if err != nil {
		t.Fatalf("--domain with no cloudflared must degrade, not fail: %v", err)
	}
	if res.Mode != config.ModeLAN || !exp.LAN {
		t.Fatalf("mode = %s (%+v), want lan", res.Mode, exp)
	}
	if !strings.Contains(res.FellBack, "s.example.com") {
		t.Fatalf("the downgrade does not name what was asked for: %q", res.FellBack)
	}
}

// ------------------------------------------------------------- plumbing

// Four rungs, exactly one of them per share.
func TestRungFlagsAreMutuallyExclusive(t *testing.T) {
	withBinaryProbe(t, installed)
	for _, args := range [][]string{
		{"--local", "--lan"},
		{"--local", "--quick"},
		{"--local", "--domain", "x.example.com"},
		{"--quick", "--lan"},
		{"--lan", "--domain", "x.example.com"},
	} {
		f, err := parseShareFlags(args)
		if err != nil {
			t.Fatalf("%v: parse: %v", args, err)
		}
		if _, _, err := resolveShareMode(context.Background(), f, 21999, "t"); err == nil {
			t.Fatalf("%v: accepted two rungs at once", args)
		} else if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("%v: unhelpful error: %v", args, err)
		}
	}
}

// `--local` is how somebody with a different personal default gets back to
// loopback for one share.
func TestExplicitLocalOverridesTheConfiguredDefault(t *testing.T) {
	withBinaryProbe(t, installed)
	writeTestConfig(t, `
[share]
default_hours = 1
default_mode = "quick"
max_concurrent = 10
`)
	f, err := parseShareFlags([]string{"--local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.applyConfigDefaults(); err != nil {
		t.Fatal(err)
	}
	if f.quick {
		t.Fatal("[share] default_mode = \"quick\" overrode an explicit --local")
	}
	res, _, err := resolveShareMode(context.Background(), f, 21999, "t")
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != config.ModeLocal {
		t.Fatalf("mode = %s, want local", res.Mode)
	}
}

// The shipped default is LAN. This is a value, not a behaviour, so it is
// asserted directly: it is the single line of config that decides what
// `herdr-expose share` does on a machine nobody has configured.
func TestShippedDefaultModeIsLocal(t *testing.T) {
	if got := config.Defaults().Share.DefaultMode; got != "lan" {
		t.Fatalf("shipped [share] default_mode = %q, want \"lan\"", got)
	}
	if config.ShareRung(config.DefaultShareMode) != config.RungLAN {
		t.Fatal("the shipped default_mode does not resolve to the lan rung")
	}
	// The DAEMON, by contrast, keeps its loopback default: it serves whoever
	// is sitting at this machine. The asymmetry is the point, so it is pinned.
	if got := config.Defaults().Expose; got.LAN || got.Cloudflare || got.Ngrok || got.Quick {
		t.Fatalf("the daemon's [expose] default left loopback: %+v", got)
	}
}

// writeTestConfig points $HOME at a throwaway dir holding this config, so a
// test can exercise the config-reading paths without touching the real one.
func writeTestConfig(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "herdr-expose")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", filepath.Join(home, "state"))
}
