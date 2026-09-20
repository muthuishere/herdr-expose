package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/expose"
)

// AMENDMENTS 15 K1: three transports, and exactly one of them per share.
func TestShareTransportFlagsAreMutuallyExclusive(t *testing.T) {
	cases := [][]string{
		{"--quick", "--lan"},
		{"--quick", "--domain", "x.example.com"},
		{"--lan", "--domain", "x.example.com"},
		{"--quick", "--lan", "--domain", "x.example.com"},
	}
	for _, args := range cases {
		f, err := parseShareFlags(args)
		if err != nil {
			t.Fatalf("%v: parse: %v", args, err)
		}
		_, _, err = resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
		if err == nil {
			t.Fatalf("%v: accepted two transports at once", args)
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("%v: unhelpful error: %v", args, err)
		}
	}
}

// --quick must resolve without reading a Cloudflare token, touching a zone or
// looking up DNS. The test runs with the token unset: if the quick path went
// anywhere near the API it would fail here.
func TestQuickShareResolvesWithNoCloudflareToken(t *testing.T) {
	t.Setenv(expose.CloudflareTokenEnv, "")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")

	f, err := parseShareFlags([]string{"--quick"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.quick {
		t.Fatal("--quick did not set the flag")
	}
	res, exp, err := resolveShareMode(context.Background(), f, 21999, "herdr-expose-share-test")
	if _, lookErr := os.Stat("/opt/homebrew/bin/cloudflared"); lookErr != nil {
		if _, lookErr2 := os.Stat("/usr/local/bin/cloudflared"); lookErr2 != nil {
			t.Skip("cloudflared is not installed on this machine")
		}
	}
	if err != nil {
		t.Fatalf("resolve --quick: %v", err)
	}
	if res.Mode != config.ModeQuick {
		t.Fatalf("mode = %s, want quick", res.Mode)
	}
	if !exp.Quick || exp.Cloudflare || exp.Domain != "" || exp.LAN {
		t.Fatalf("the [expose] table handed to the instance is not a pure quick one: %+v", exp)
	}
	if res.Bind != config.BindLoopback {
		t.Fatalf("bind = %s, want loopback", res.Bind)
	}
	if !res.SecureContext {
		t.Fatal("a quick share is https and must report secure_context = true")
	}
}

// A quick share creates nothing in Cloudflare, so teardown reports dns/tunnel
// as not-applicable rather than as a delete that quietly did nothing — and a
// mixed `revoke --all` never runs the Cloudflare path for it.
func TestQuickShareRecordNeedsNoCloudflareTeardown(t *testing.T) {
	quick := &shareRecord{Mode: string(config.ModeQuick), URL: "https://x-y-z.trycloudflare.com", Port: 21999}
	if !quick.isQuick() || quick.isLAN() {
		t.Fatalf("mode classification wrong: %+v", quick)
	}
	if quick.hasCloudflareResources() {
		t.Fatal("a quick share must not be given a DNS/tunnel teardown")
	}
	if quick.CurrentURL() != "https://x-y-z.trycloudflare.com" {
		t.Fatalf("url = %s", quick.CurrentURL())
	}
	if v := quick.view(); v.Mode != "quick" {
		t.Fatalf("share list --json mode = %q, want quick", v.Mode)
	}

	lan := &shareRecord{Mode: string(config.ModeLAN), Port: 21999}
	domain := &shareRecord{Mode: string(config.ModeCloudflare), Domain: "s.example.com", Port: 21999}
	if lan.hasCloudflareResources() {
		t.Fatal("a LAN share must not be given a DNS/tunnel teardown")
	}
	if !domain.hasCloudflareResources() {
		t.Fatal("a domain share MUST still delete its DNS record and tunnel")
	}
	// An old record written before modes existed still means the named tunnel.
	if legacy := (&shareRecord{}); !legacy.hasCloudflareResources() {
		t.Fatal("a record with no mode must default to the named-tunnel teardown")
	}
}

// [share] default_mode drives the transport when no flag is passed.
func TestShareDefaultModeQuick(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[share]\ndefault_hours = 1\ndefault_mode = \"quick\"\nmax_concurrent = 10\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatalf("default_mode = \"quick\" was rejected: %v", err)
	}
	if cfg.Share.DefaultMode != "quick" {
		t.Fatalf("default_mode = %q", cfg.Share.DefaultMode)
	}
}
