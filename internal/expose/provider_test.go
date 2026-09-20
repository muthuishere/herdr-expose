package expose

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// The providers are INTERCHANGEABLE, and these tests are what makes that a
// property rather than a claim. Every assertion below is written against the
// Provider interface, and each one runs for every implementation, so a
// provider that quietly gives up verification, supervision or a symmetric
// teardown fails here rather than in somebody's account.
//
// The table shrank when the built-in ngrok provider was removed (AMENDMENTS 18
// / ADR 0034) — it did not go away. The JS adapter is now the answer for ngrok,
// tailscale and anything else, so it is IN the table: the escape hatch is held
// to the same rung, footprint and idempotency rules as the built-in, which is
// the only thing that makes it a usable answer rather than a shrug.

// providerCase is one implementation, built the way the Manager builds it.
type providerCase struct {
	name string
	// expose is the table that selects this provider.
	expose config.Expose
	// bins are the fake helper binaries this provider needs.
	bins map[string]string
	// wantRung is where this provider sits on the exposure ladder.
	wantRung config.Rung
	// createsRemote is true only for the one provider that makes something in
	// an account someone owns.
	createsRemote bool
}

func providerCases(t *testing.T) []providerCase {
	t.Helper()
	// A fake agent that behaves like the real one: prints a public hostname,
	// then stays up.
	cfQuickBin := fakeBinDir(t, "cloudflared",
		`echo "|  https://silly-fake-words.trycloudflare.com  |"; sleep 30`)
	cfNamedBin := fakeBinDir(t, "cloudflared", `sleep 30`)
	adapter, err := filepath.Abs(filepath.Join("..", "..", "adapters", "template.js"))
	if err != nil {
		t.Fatal(err)
	}

	return []providerCase{
		{
			name:          "cloudflare-named",
			expose:        config.Expose{Cloudflare: true, Domain: "herdr.example.com"},
			bins:          cfNamedBin,
			wantRung:      config.RungDomain,
			createsRemote: true,
		},
		{
			name:     "cloudflare-quick",
			expose:   config.Expose{Quick: true},
			bins:     cfQuickBin,
			wantRung: config.RungQuick,
		},
		{
			// The escape hatch, built exactly as the Manager builds it. It is
			// a full Provider or it is not an answer for the transports that
			// are no longer built in.
			name: "js-adapter",
			expose: config.Expose{
				Adapter:  "template",
				Adapters: []config.Adapter{{ID: "template", Script: adapter}},
			},
			bins:     cfNamedBin, // unused by the adapter; keeps PATH shaped the same
			wantRung: config.RungDomain,
		},
	}
}

// fakeBinDir writes a fake helper into its own directory and returns a map the
// test can prepend to PATH, so `resolveBinary` finds it by NAME the way the
// real code does.
func fakeBinDir(t *testing.T, name, body string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return map[string]string{name: dir}
}

func withFakePath(t *testing.T, bins map[string]string) {
	t.Helper()
	var dirs []string
	for _, d := range bins {
		dirs = append(dirs, d)
	}
	t.Setenv("PATH", strings.Join(dirs, ":")+":"+os.Getenv("PATH"))
}

// EVERY provider declares what it creates, and only one of them creates
// anything at all. A provider whose Footprint were empty while it did create
// something is how a tunnel gets orphaned, so the declaration is asserted
// against the provider's identity rather than trusted.
func TestEveryProviderDeclaresItsFootprint(t *testing.T) {
	for _, tc := range providerCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			withFakePath(t, tc.bins)
			m := New(Options{Port: 21999, Expose: tc.expose, StateDir: t.TempDir()})

			fp := m.Footprint()
			if fp.Provider == "" || fp.Provider == "none" {
				t.Fatalf("no footprint for %s: %+v", tc.name, fp)
			}
			if got := fp.RemoteAccount && fp.NamedTunnel != ""; got != tc.createsRemote {
				t.Fatalf("%s creates-remote = %v, want %v (%s)", tc.name, got, tc.createsRemote, fp.Describe())
			}
			if tc.createsRemote {
				if fp.DNSRecord == "" || fp.DNSTag == "" {
					t.Fatalf("a provider that writes DNS must declare the record AND the tag it deletes by: %+v", fp)
				}
			} else if !fp.Ephemeral && fp.ReservedName == "" {
				t.Fatalf("a provider that creates nothing must say so, either as ephemeral or as a "+
					"reserved name it did not create: %+v", fp)
			}
			// A footprint is a teardown plan, never a credential store.
			for _, name := range fp.SecretEnv {
				if v := os.Getenv(name); v != "" && strings.Contains(fp.Describe(), v) {
					t.Fatalf("a secret VALUE reached a footprint: %s", fp.Describe())
				}
			}
		})
	}
}

// The ladder is the same for every provider. `--quick` is the quick rung
// whoever carries it, and a domain is the domain rung either way — otherwise
// "least exposure by default" would mean different things depending on which
// binary the user happens to have. A JS adapter is NOT a way off the ladder:
// it sorts at the top, because an unknown transport must never look safer than
// it is.
func TestRungIsTheSameWhicheverProviderCarriesIt(t *testing.T) {
	for _, tc := range providerCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			withFakePath(t, tc.bins)
			res := Resolve(tc.expose, 21999, func(string) bool { return true })
			if got := config.RungOf(res.Mode); got != tc.wantRung {
				t.Fatalf("%s resolved to rung %s (mode %s), want %s", tc.name, got, res.Mode, tc.wantRung)
			}
			if !res.Remote {
				t.Fatalf("%s must be a remote rung", tc.name)
			}
			if res.Bind != config.BindLoopback {
				t.Fatalf("%s bound %s; a tunnel rung keeps the wifi off the listener", tc.name, res.Bind)
			}
		})
	}
}

// A missing binary degrades to LAN, and names the binary that is actually
// missing. The message is built from quickBinary rather than hard-coded, so a
// provider added later reports ITS binary: telling somebody "cloudflared is
// not installed" when they asked for something else is how an hour gets lost.
func TestMissingBinaryDegradesToLANPerProvider(t *testing.T) {
	cases := []struct {
		name   string
		expose config.Expose
		want   string
	}{
		{"cloudflare-quick", config.Expose{Quick: true}, "cloudflared"},
		{"cloudflare-domain", config.Expose{Cloudflare: true, Domain: "x.example.com"}, "cloudflared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Resolve(tc.expose, 21999, func(string) bool { return false })
			if res.Mode != ModeLAN {
				t.Fatalf("mode = %s, want a downward fallback to lan", res.Mode)
			}
			if !strings.Contains(res.FellBack, tc.want) {
				t.Fatalf("fallback reason %q does not name the missing binary %q", res.FellBack, tc.want)
			}
			// L2: down, never up.
			if config.RungOf(res.Mode) > config.RungLAN {
				t.Fatalf("a fallback escalated to %s", res.Mode)
			}
		})
	}
}

// Every provider publishes a URL only once it has ANSWERED. A started process
// is not a working tunnel, and the Cloudflare edge serves a perfectly good
// 502 through one that is not routing.
func TestEveryProviderVerifiesBeforePublishing(t *testing.T) {
	for _, tc := range []struct{ name, shape string }{
		{"static-hostname", "static"},
		{"scraped-hostname", "scraped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A gate PER SUBTEST: the origin answers 502 (which the Cloudflare
			// edge really does serve through a tunnel that is not routing)
			// until the test opens it.
			answers := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-answers:
					w.WriteHeader(http.StatusOK)
				default:
					w.WriteHeader(http.StatusBadGateway)
				}
			}))
			defer srv.Close()

			spec := procSpec{
				Name: tc.shape, Mode: "named",
				Verify: true, VerifyTimeout: 20 * time.Second,
				Bin:   fakeBin(t, "agent", "sleep 20"),
				Build: buildFake(t, "sleep 20"),
			}
			if tc.shape == "static" {
				spec.StaticURL = srv.URL
			} else {
				spec.Mode = "quick"
				spec.Build = buildFake(t, "echo URL="+srv.URL+"; sleep 20")
				spec.ScanURL = func(line string) string {
					if strings.Contains(line, "URL=") {
						return strings.TrimPrefix(strings.TrimSpace(line), "URL=")
					}
					return ""
				}
			}

			p := newProcTunnel(spec)
			if err := p.Launch(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer p.Stop()
			// While the origin answers 502, nothing may be published.
			time.Sleep(1500 * time.Millisecond)
			if u := p.URL(); u != "" {
				t.Fatalf("published %s before it answered", u)
			}
			close(answers)
			if _, err := p.WaitForURL(context.Background(), 20*time.Second); err != nil {
				t.Fatalf("never published after the origin came up: %v", err)
			}
		})
	}
}

// buildFake returns a procSpec.Build for a shell one-liner.
func buildFake(t *testing.T, body string) func(context.Context) (*exec.Cmd, error) {
	t.Helper()
	bin := fakeBin(t, "agent", body)
	return func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, bin), nil
	}
}

// Stop and Destroy are idempotent for EVERY provider, including the case that
// actually happens: called when nothing was ever started.
func TestStopAndDestroyAreIdempotentForEveryProvider(t *testing.T) {
	for _, tc := range providerCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			withFakePath(t, tc.bins)
			state := t.TempDir()
			m := New(Options{Port: 21999, Expose: tc.expose, StateDir: state})

			for i := 0; i < 3; i++ {
				if err := m.Stop(); err != nil {
					t.Fatalf("Stop #%d on a manager with nothing running: %v", i+1, err)
				}
			}
			if tc.createsRemote {
				return // covered against the mock API in TestDestroyIsIdempotent
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			for i := 0; i < 3; i++ {
				if err := m.Destroy(ctx); err != nil {
					t.Fatalf("Destroy #%d on a provider that creates nothing: %v", i+1, err)
				}
			}
		})
	}
}

// The cross-process singleton. Two herdr-expose processes on one deployment —
// the daemon plus an operator typing `expose start` — must not both provision
// against the same named tunnel. Cloudflare accepts two connectors happily, so
// nothing downstream would have noticed.
func TestSecondProcessDoesNotRaiseASecondTunnel(t *testing.T) {
	state := t.TempDir()

	first, held, err := tryExposeLock(state)
	if err != nil || !held {
		t.Fatalf("first holder could not take the lock: held=%v err=%v", held, err)
	}
	if !ExposureHeldElsewhere(state) {
		t.Fatal("a held exposure must be visible to the next process")
	}

	// A manager in the "other process" must report the running exposure and
	// succeed, not provision a second one and not error.
	withFakePath(t, fakeBinDir(t, "cloudflared", `echo "https://x.trycloudflare.com"; sleep 30`))
	m := New(Options{Port: 21999, Expose: config.Expose{Quick: true}, StateDir: state})
	st, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start while another process holds the exposure must be a no-op, got: %v", err)
	}
	if !st.Running {
		t.Fatal("status must report the exposure as running")
	}

	first.release()
	first.release() // idempotent
	if ExposureHeldElsewhere(state) {
		t.Fatal("the lock must be free once the holder releases it")
	}
}

// Start twice in ONE process returns the same tunnel, not a second one and not
// an error. `start` is the verb people re-run.
func TestStartTwiceReusesTheRunningTunnel(t *testing.T) {
	withFakePath(t, fakeBinDir(t, "cloudflared", `echo "https://reuse-me.trycloudflare.com"; sleep 30`))
	m := New(Options{Port: 21999, Expose: config.Expose{Quick: true}, StateDir: t.TempDir()})
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The URL never verifies (no such host), so Start returns an error — but
	// what is asserted here is the SECOND call's behaviour once one is live.
	_, _ = m.Start(ctx)

	m.mu.Lock()
	m.cur = newProcTunnel(procSpec{Name: "fake", Mode: "quick"})
	m.mu.Unlock()

	st, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start #2 with a tunnel already up must be a no-op, got: %v", err)
	}
	if st.Provider != "fake" {
		t.Fatalf("Start #2 replaced the running tunnel: %+v", st)
	}
}

// --- the escape hatch, held to the built-in's standard ---------------------
//
// ngrok was a built-in provider until AMENDMENTS 18 / ADR 0034 removed it as
// UNVERIFIED SURFACE: no binary and no token on the machine it was written on,
// so it was unit-tested and never once exercised end to end. What replaced it
// is a JS adapter, and an escape hatch is only an honest answer if it is a
// real Provider — so these assert that, with no account and no network.

func TestJSAdapterIsAFullProviderNotASecondClassOne(t *testing.T) {
	adapter, err := filepath.Abs(filepath.Join("..", "..", "adapters", "template.js"))
	if err != nil {
		t.Fatal(err)
	}
	exp := config.Expose{
		Adapter:  "template",
		Adapters: []config.Adapter{{ID: "template", Script: adapter}},
	}
	m := New(Options{Port: 21999, Expose: exp, StateDir: t.TempDir()})
	defer m.Close()

	// It declares a footprint BEFORE anything runs, like every other provider.
	fp := m.Footprint()
	if fp.Provider != "js:template" {
		t.Fatalf("the adapter does not declare itself: %+v", fp)
	}
	if !fp.Ephemeral {
		t.Fatalf("an adapter that creates nothing in the host's name must say so: %s", fp.Describe())
	}
	if strings.Contains(fp.Describe(), "trycloudflare") {
		t.Fatalf("the adapter inherited a Cloudflare footprint: %s", fp.Describe())
	}

	// It sits on the ladder — at the top, not off it.
	res := Resolve(exp, 21999, func(string) bool { return false })
	if res.Mode != ModeJS || !res.Remote || res.Bind != config.BindLoopback {
		t.Fatalf("an adapter must be a remote rung on a loopback listener: %+v", res)
	}
	if config.RungOf(res.Mode) != config.RungDomain {
		t.Fatalf("an unknown transport must never sort BELOW the top rung: %s", config.RungOf(res.Mode))
	}

	// And Stop is idempotent with nothing running, like every other provider.
	for i := 0; i < 3; i++ {
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop #%d on an adapter with nothing running: %v", i+1, err)
		}
	}
}
