package expose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// AMENDMENTS 15: a quick tunnel needs cloudflared and nothing else. No
// account, no zone, no DNS, no API token.
func TestQuickResolvesWithoutADomainOrAToken(t *testing.T) {
	res := Resolve(config.Expose{Quick: true}, 21200, func(string) bool { return true })
	if res.Mode != ModeQuick {
		t.Fatalf("mode = %s, want quick", res.Mode)
	}
	if res.Bind != config.BindLoopback {
		t.Fatalf("bind = %s, want loopback: a quick tunnel is the only remote path", res.Bind)
	}
	if !res.Remote || !res.SecureContext {
		t.Fatalf("quick must be remote and a secure context (it is https): %+v", res)
	}
	if res.URL != "" {
		t.Fatalf("url = %q, want empty: the hostname is assigned by the edge at start", res.URL)
	}
	if !strings.Contains(res.Describe(), "mode=quick") {
		t.Fatalf("startup line does not name the mode: %s", res.Describe())
	}
}

func TestQuickFallsBackToLANWithoutCloudflared(t *testing.T) {
	res := Resolve(config.Expose{Quick: true}, 21200, func(string) bool { return false })
	if res.Mode != ModeLAN {
		t.Fatalf("mode = %s, want lan", res.Mode)
	}
	if res.FellBack == "" || !strings.Contains(res.FellBack, "cloudflared is not installed") {
		t.Fatalf("the downgrade must say why: %q", res.FellBack)
	}
}

// C1 still stands where it was aimed: the permanent deployment cannot become
// ephemeral, because `quick` is not a key any config file can set.
func TestQuickIsNotReachableFromTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[expose]\ncloudflare = true\ndomain = \"herdr.example.com\"\nquick = true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Expose.Quick {
		t.Fatal("a `quick = true` key in [expose] enabled the ephemeral tunnel; it must be ignored")
	}
	if Resolve(cfg.Expose, 21118, func(string) bool { return true }).Mode != ModeCloudflare {
		t.Fatal("the permanent deployment stopped resolving to its static domain")
	}
}

// The hostname cloudflared prints for a quick tunnel, in the shape it really
// prints it: an INF log line inside an ASCII box.
func TestQuickURLIsScrapedFromCloudflaredOutput(t *testing.T) {
	lines := map[string]string{
		"2026-09-20T09:00:01Z INF |  https://mid-ancient-stuff-tokyo.trycloudflare.com                    |": "https://mid-ancient-stuff-tokyo.trycloudflare.com",
		"2026-09-20T09:00:01Z INF +--------------------------------------------------+":                      "",
		"2026-09-20T09:00:00Z INF Requesting new quick Tunnel on trycloudflare.com...":                       "",
		"INF |  https://a1b2c3.trycloudflare.com  |":                                                         "https://a1b2c3.trycloudflare.com",
	}
	for line, want := range lines {
		if got := quickURLRe.FindString(line); got != want {
			t.Errorf("scrape(%q) = %q, want %q", line, got, want)
		}
	}
	// It must not match the permanent deployment's hostname, or a restart of
	// the wrong process would republish somebody else's URL.
	if quickURLRe.MatchString("https://herdr.deemwar.com") {
		t.Fatal("the quick scraper matched a real domain")
	}
}

// A scraped hostname is a hostname cloudflared has PRINTED, not one the edge
// routes yet. Same rule as the named tunnel: publish only after /healthz
// answers through it.
func TestScrapedURLIsVerifiedBeforeItIsPublished(t *testing.T) {
	srv, ready := healthServer(t)
	bin := fakeBin(t, "fakequick", "echo \"INF |  "+srv.URL+"  |\"\nsleep 30")
	re := regexp.MustCompile(regexp.QuoteMeta(srv.URL))

	tun := newProcTunnel(procSpec{
		Name: "cloudflare-quick", Mode: "quick", Bin: bin,
		Build: func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, bin), nil
		},
		ScanURL:       func(line string) string { return re.FindString(line) },
		Verify:        true,
		VerifyTimeout: 10 * time.Second,
		Log:           func(string, ...any) {},
	})
	t.Cleanup(func() { _ = tun.Stop() })

	if err := tun.Launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := tun.WaitForURL(context.Background(), 1500*time.Millisecond); err == nil {
		t.Fatal("the scraped url was published before it answered")
	}
	if u := tun.URL(); u != "" {
		t.Fatalf("url published before verification: %s", u)
	}

	ready.Store(true)
	url, err := tun.WaitForURL(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForURL: %v", err)
	}
	if url != srv.URL {
		t.Fatalf("url = %q, want %q", url, srv.URL)
	}
}

// The whole point: no token is read, so the command line carries --url and
// never --config, and the child's environment carries no Cloudflare
// credential at all.
func TestQuickTunnelReadsNoCloudflareToken(t *testing.T) {
	t.Setenv(CloudflareTokenEnv, "")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	// Sanity: with no token the API path is genuinely unusable, so a quick
	// tunnel that starts anyway proves it never went near it.
	if _, err := newCFAPI(&redactor{}); err == nil {
		t.Fatal("newCFAPI succeeded without a token; this test proves nothing")
	}

	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	env := filepath.Join(dir, "env")
	bin := fakeBin(t, "fakequick",
		"echo \"$@\" > "+args+"\nenv > "+env+"\nsleep 30")

	tun, err := newQuickTunnel(CloudflareOptions{Port: 21777, Bin: bin}, func(string, ...any) {}, &redactor{})
	if err != nil {
		t.Fatalf("newQuickTunnel with no token: %v", err)
	}
	t.Cleanup(func() { _ = tun.Stop() })
	if err := tun.Launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitForFile(t, args)

	got := strings.TrimSpace(readFile(t, args))
	if !strings.Contains(got, "--url http://127.0.0.1:21777") {
		t.Fatalf("args = %q, want a --url quick tunnel on the share's own port", got)
	}
	if strings.Contains(got, "--config") || strings.Contains(got, "run ") {
		t.Fatalf("args = %q: a quick tunnel must not run a NAMED tunnel", got)
	}
	for _, line := range strings.Split(readFile(t, env), "\n") {
		if strings.HasPrefix(line, "CLOUDFLARE") || strings.Contains(line, "TUNNEL_TOKEN") {
			t.Fatalf("a cloudflare credential reached the quick tunnel's environment: %s",
				strings.SplitN(line, "=", 2)[0])
		}
	}
}

// A quick share is torn down by killing a process matched on an argument we
// DERIVED from its own port, so it can never reach the permanent deployment's
// cloudflared (which runs with --config, never --url).
func TestQuickURLPatternIsPortDerived(t *testing.T) {
	p := QuickURLPattern(21777)
	if p != "--url http://127.0.0.1:21777" {
		t.Fatalf("pattern = %q", p)
	}
	if strings.Contains(p, "--config") {
		t.Fatal("the quick teardown matcher must not match a named tunnel")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
