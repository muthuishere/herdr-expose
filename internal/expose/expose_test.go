package expose

import (
	"context"
	"fmt"

	"github.com/muthuishere/herdr-expose/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// testLog collects (already scrubbed) log lines so tests can assert that no
// secret ever reaches a sink.
type testLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(sprintf(format, args...)))
}

func (l *testLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func sprintf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmtSprintf(format, args...)
}

// -------------------------------------------------------------------------
// redactor
// -------------------------------------------------------------------------

func TestRedactorScrubsSecrets(t *testing.T) {
	r := &redactor{}
	r.add("s3cret-value-long-enough")
	r.add("tiny") // too short to redact safely; ignored on purpose
	got := r.scrub("Authorization: Bearer s3cret-value-long-enough here")
	if strings.Contains(got, "s3cret-value-long-enough") {
		t.Fatalf("secret survived scrubbing: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("no redaction marker: %s", got)
	}
	if !r.contains("xx s3cret-value-long-enough xx") {
		t.Fatal("contains() missed a known secret")
	}
}

// -------------------------------------------------------------------------
// process supervision
// -------------------------------------------------------------------------

// healthServer stands in for the public endpoint: it 503s until it is armed.
func healthServer(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	ready := &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && ready.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv, ready
}

// C2 step 7: a process that started is not a tunnel that works. The URL must
// not be reported until the public endpoint actually answers 200.
func TestURLIsPublishedOnlyAfterHealthCheckPasses(t *testing.T) {
	srv, ready := healthServer(t)
	bin := fakeBin(t, "faketunnel", "sleep 30")

	tun := newProcTunnel(procSpec{
		Name: "cloudflare", Mode: "named", Bin: bin,
		Build: func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, bin), nil
		},
		StaticURL: srv.URL, Verify: true, VerifyTimeout: 10 * time.Second,
		Log: func(string, ...any) {},
	})
	t.Cleanup(func() { _ = tun.Stop() })

	if err := tun.Launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if u := tun.URL(); u != "" {
		t.Fatalf("url published before verification: %s", u)
	}
	if _, err := tun.WaitForURL(context.Background(), 1500*time.Millisecond); err == nil {
		t.Fatal("WaitForURL succeeded while the endpoint was still failing")
	}

	ready.Store(true)
	url, err := tun.WaitForURL(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForURL after the endpoint came up: %v", err)
	}
	if url != srv.URL {
		t.Fatalf("url = %q, want %q", url, srv.URL)
	}
	if st := tun.Snapshot(); !st.Running || !st.Healthy {
		t.Fatalf("status after verification: %+v", st)
	}
}

// A crashing tunnel process must be restarted, and must never take the server
// with it.
func TestProcessCrashIsRestartedWithBackoff(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	bin := fakeBin(t, "crashy", "echo run >> "+counter+"\nexit 7")

	log := &testLog{}
	tun := newProcTunnel(procSpec{
		Name: "cloudflare", Mode: "named", Bin: bin,
		Build: func(ctx context.Context) (*exec.Cmd, error) { return exec.CommandContext(ctx, bin), nil },
		Log:   log.logf,
	})
	t.Cleanup(func() { _ = tun.Stop() })

	if err := tun.Launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	// Wait for the process to have actually been respawned, not merely for the
	// first exit to be noticed (the first backoff is one second).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(counter)
		if strings.Count(string(body), "run") >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	st := tun.Snapshot()
	if st.Restarts < 1 {
		t.Fatalf("no restart recorded: %+v\n%s", st, log.text())
	}
	if st.LastError == "" {
		t.Fatal("the crash was not surfaced in status")
	}
	body, _ := os.ReadFile(counter)
	if strings.Count(string(body), "run") < 2 {
		t.Fatalf("process was not respawned, ran %q times", body)
	}
}

func TestProcTunnelStopIsIdempotent(t *testing.T) {
	bin := fakeBin(t, "faketunnel", "sleep 30")
	tun := newProcTunnel(procSpec{
		Name: "cloudflare", Mode: "named", Bin: bin,
		Build: func(ctx context.Context) (*exec.Cmd, error) { return exec.CommandContext(ctx, bin), nil },
		Log:   func(string, ...any) {},
	})
	if err := tun.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := tun.Snapshot().PID
	if pid == 0 {
		t.Fatal("no pid recorded")
	}
	for i := 0; i < 3; i++ {
		if err := tun.Stop(); err != nil {
			t.Fatalf("Stop #%d: %v", i+1, err)
		}
	}
	if st := tun.Snapshot(); st.Running || st.PID != 0 {
		t.Fatalf("status after stop: %+v", st)
	}
	// The process really is gone.
	time.Sleep(200 * time.Millisecond)
	if err := syscallKill0(pid); err == nil {
		t.Fatalf("pid %d survived Stop", pid)
	}
}

// A missing binary is a real error from Start, not a background mystery.
func TestManagerSurfacesMissingBinary(t *testing.T) {
	_, err := newCloudflare(CloudflareOptions{
		Port: 21118, Domain: "herdr.example.com", Bin: "/nonexistent/cloudflared-xyz",
	}, func(string, ...any) {}, &redactor{})
	if err == nil || !strings.Contains(err.Error(), "cloudflared") {
		t.Fatalf("want a cloudflared-not-found error, got: %v", err)
	}
}

// -------------------------------------------------------------------------
// goja adapters (the escape hatch)
// -------------------------------------------------------------------------

func loadAdapter(t *testing.T, src string, cfg map[string]string, log *testLog) *jsTunnel {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adapter.js")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := newJSTunnel("test", path, "http://127.0.0.1:21118", cfg, log.logf, &redactor{})
	if err != nil {
		t.Fatalf("load adapter: %v", err)
	}
	t.Cleanup(func() { _ = j.Stop() })
	return j
}

// Every shipped adapter must at least parse and expose the three entry points
// under goja — `export function` and all.
func TestShippedAdaptersLoad(t *testing.T) {
	matches, err := filepath.Glob("../../adapters/*.js")
	if err != nil || len(matches) == 0 {
		t.Fatalf("no adapters found: %v", err)
	}
	for _, m := range matches {
		m := m
		t.Run(filepath.Base(m), func(t *testing.T) {
			log := &testLog{}
			j, err := newJSTunnel(filepath.Base(m), m, "http://127.0.0.1:21118",
				map[string]string{"hostname": "herdr.example.com", "tunnel": "t", "domain": "d.example.com"},
				log.logf, &redactor{})
			if err != nil {
				t.Fatalf("%s failed to load under goja: %v", m, err)
			}
			defer func() { _ = j.Stop() }()
			if j.fnStart == nil || j.fnStatus == nil || j.fnStop == nil {
				t.Fatalf("%s is missing one of start/status/stop", m)
			}
		})
	}
}

func TestJSAdapterSpawnOnLineSetUrlAndStop(t *testing.T) {
	log := &testLog{}
	j := loadAdapter(t, `
		export function start(ctx) {
		  const p = ctx.spawn("sh", ["-c", "echo ready https://tunnel.example.com; sleep 30"]);
		  ctx.onLine(p, (line) => {
		    const m = /(https:\/\/\S+)/.exec(line);
		    if (m) ctx.setUrl(m[1]);
		  });
		  ctx.log("local is " + ctx.localUrl);
		  return { pid: p.pid };
		}
		export function status(ctx) { return { url: ctx.url, healthy: ctx.isAlive() }; }
		export function stop(ctx) { ctx.kill(); }
	`, nil, log)

	if err := j.Launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	url, err := j.WaitForURL(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForURL: %v\n%s", err, log.text())
	}
	if url != "https://tunnel.example.com" {
		t.Fatalf("url = %q", url)
	}
	st := j.Snapshot()
	if !st.Healthy || st.Provider != "js:test" || st.Tunnel != "js" {
		t.Fatalf("status: %+v", st)
	}
	if !strings.Contains(log.text(), "local is http://127.0.0.1:21118") {
		t.Fatalf("ctx.localUrl / ctx.log not wired:\n%s", log.text())
	}

	pid := st.PID
	for i := 0; i < 3; i++ {
		if err := j.Stop(); err != nil {
			t.Fatalf("Stop #%d: %v", i+1, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if err := syscallKill0(pid); err == nil {
		t.Fatalf("adapter child pid %d survived Stop", pid)
	}
}

// SECURITY: ctx.env(name) hands the value to the adapter for use, but the value
// must never reach a log line, a status payload or a state file.
func TestJSEnvValueIsUsableButNeverLeaks(t *testing.T) {
	t.Setenv("HERDR_TEST_SECRET", "top-secret-token-value-123")
	log := &testLog{}
	j := loadAdapter(t, `
		export function start(ctx) {
		  const s = ctx.env("HERDR_TEST_SECRET");
		  if (!s) throw new Error("env() returned nothing");
		  // Usable: pass it to a spawned tool through the environment.
		  const p = ctx.spawn("sh", ["-c", "echo url https://ok.example.com; sleep 30"], { TOKEN: s });
		  ctx.onLine(p, (l) => { const m = /(https:\/\/\S+)/.exec(l); if (m) ctx.setUrl(m[1]); });
		  // Hostile: try to leak it three ways.
		  ctx.log("leaking " + s);
		  ctx.setUrl("https://evil.example.com/" + s);
		  return { pid: p.pid, secret: s };
		}
		export function status(ctx) { return { url: ctx.url, healthy: true, secret: ctx.env("HERDR_TEST_SECRET") }; }
		export function stop(ctx) { ctx.kill(); }
	`, nil, log)

	if err := j.Launch(context.Background()); err != nil {
		t.Fatalf("launch: %v\n%s", err, log.text())
	}
	if _, err := j.WaitForURL(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("WaitForURL: %v\n%s", err, log.text())
	}
	st := j.Snapshot()
	blob := st.URL + "|" + st.LastError + "|" + st.Provider + "|" + log.text()
	if strings.Contains(blob, "top-secret-token-value-123") {
		t.Fatalf("the env value leaked:\n%s", blob)
	}
	if !strings.Contains(log.text(), "[redacted]") {
		t.Fatalf("expected a redaction marker in the log:\n%s", log.text())
	}
	if st.URL != "https://ok.example.com" {
		t.Fatalf("the secret-bearing url should have been refused, url = %q", st.URL)
	}
}

// A throwing adapter fails its own start and nothing else.
func TestJSAdapterCrashIsContained(t *testing.T) {
	log := &testLog{}
	j := loadAdapter(t, `
		export function start(ctx) { throw new Error("boom"); }
		export function status(ctx) { return {}; }
		export function stop(ctx) { throw new Error("boom again"); }
	`, nil, log)

	if err := j.Launch(context.Background()); err == nil {
		t.Fatal("a throwing start() must return an error")
	}
	// A throwing stop() is still idempotent and still returns nil.
	for i := 0; i < 2; i++ {
		if err := j.Stop(); err != nil {
			t.Fatalf("Stop #%d returned %v; stop must always be safe", i+1, err)
		}
	}
	if st := j.Snapshot(); st.Running {
		t.Fatalf("status after a failed start: %+v", st)
	}
}

// A hanging adapter is interrupted, not tolerated.
func TestJSAdapterHangIsInterrupted(t *testing.T) {
	old := jsCallTimeout
	jsCallTimeout = 500 * time.Millisecond
	t.Cleanup(func() { jsCallTimeout = old })

	log := &testLog{}
	j := loadAdapter(t, `
		export function start(ctx) { while (true) {} }
		export function status(ctx) { return {}; }
		export function stop(ctx) { ctx.kill(); }
	`, nil, log)

	start := time.Now()
	err := j.Launch(context.Background())
	if err == nil {
		t.Fatal("a hanging start() must fail")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("interrupt took %s; the budget is not being enforced", elapsed)
	}
	if err := j.Stop(); err != nil {
		t.Fatalf("Stop after a hang: %v", err)
	}
}

// The VM is bare on purpose: no fs, no network, no require.
func TestJSSandboxHasNoFilesystemOrNetwork(t *testing.T) {
	log := &testLog{}
	for _, probe := range []string{"require", "fetch", "XMLHttpRequest", "process", "setTimeout", "readFile"} {
		j := loadAdapter(t, `
			export function start(ctx) {
			  if (typeof `+probe+` !== "undefined") throw new Error("`+probe+` is reachable");
			  ctx.setUrl("https://ok.example.com");
			}
			export function status(ctx) { return { url: ctx.url }; }
			export function stop(ctx) {}
		`, nil, log)
		if err := j.Launch(context.Background()); err != nil {
			t.Fatalf("%s: %v", probe, err)
		}
		_ = j.Stop()
	}
}

// ctx.config carries the adapter's [expose.adapters.env] table.
func TestJSAdapterConfigTable(t *testing.T) {
	log := &testLog{}
	j := loadAdapter(t, `
		export function start(ctx) { ctx.setUrl("https://" + ctx.config.hostname); }
		export function status(ctx) { return { url: ctx.url, healthy: true }; }
		export function stop(ctx) {}
	`, map[string]string{"hostname": "herdr.example.com"}, log)

	if err := j.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u, _ := j.WaitForURL(context.Background(), time.Second); u != "https://herdr.example.com" {
		t.Fatalf("url = %q", u)
	}
}

// -------------------------------------------------------------------------
// manager
// -------------------------------------------------------------------------

func TestManagerStopWithNothingRunningIsNoOp(t *testing.T) {
	m := New(Options{Port: 21118})
	t.Cleanup(func() { _ = m.Close() })
	for i := 0; i < 3; i++ {
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop on an idle manager: %v", err)
		}
	}
	st := m.Status()
	if st.Tunnel != "" || st.PID != 0 || st.Provider != "none" {
		t.Fatalf("idle status: %+v", st)
	}
	// In local mode the listener itself is the exposure, so there is nothing to
	// start and nothing to fail.
	if st.Mode != string(ModeLocal) || st.Bind != config.BindLoopback || st.URL != "http://127.0.0.1:21118" {
		t.Fatalf("local mode status: %+v", st)
	}
}

// E1: an empty [expose] table means local mode, not an error.
func TestLocalModeStartIsNotAnError(t *testing.T) {
	m := New(Options{Port: 21118})
	t.Cleanup(func() { _ = m.Close() })
	st, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("local mode start: %v", err)
	}
	if st.Mode != string(ModeLocal) || !st.SecureContext {
		t.Fatalf("local status: %+v", st)
	}
}

func TestManagerRefusesAnUnknownAdapter(t *testing.T) {
	m := New(Options{Port: 21118, Expose: config.Expose{Adapter: "nope"}})
	t.Cleanup(func() { _ = m.Close() })
	if _, err := m.Start(context.Background()); err == nil {
		t.Fatal("starting an adapter with no [[expose.adapters]] entry must fail")
	}
}

// -------------------------------------------------------------------------
// mode resolution (E1)
// -------------------------------------------------------------------------

func TestModeResolution(t *testing.T) {
	present := func(string) bool { return true }
	absent := func(string) bool { return false }

	cases := []struct {
		name       string
		e          config.Expose
		found      func(string) bool
		wantMode   config.Mode
		wantBind   string
		wantSecure bool
		wantFall   bool
	}{
		{"default is local", config.Expose{}, present, ModeLocal, config.BindLoopback, true, false},
		{"lan flag", config.Expose{LAN: true}, present, ModeLAN, config.BindAll, false, false},
		{"cloudflare with binary", config.Expose{Cloudflare: true, Domain: "herdr.deemwar.com"}, present,
			ModeCloudflare, config.BindLoopback, true, false},
		{"cloudflare without binary falls back to lan", config.Expose{Cloudflare: true, Domain: "herdr.deemwar.com"},
			absent, ModeLAN, config.BindAll, false, true},
		{"cloudflare without binary falls back even when lan is false",
			config.Expose{Cloudflare: true, Domain: "x.example.com", LAN: false}, absent, ModeLAN, config.BindAll, false, true},
		{"js adapter binds loopback", config.Expose{Adapter: "x"}, present, ModeJS, config.BindLoopback, true, false},
		{"a working tunnel is not downgraded by lan = true",
			config.Expose{Cloudflare: true, Domain: "herdr.deemwar.com", LAN: true}, present,
			ModeCloudflare, config.BindLoopback, true, false},
	}
	for _, c := range cases {
		res := Resolve(c.e, 21118, c.found)
		if res.Mode != c.wantMode {
			t.Errorf("%s: mode = %s, want %s", c.name, res.Mode, c.wantMode)
		}
		if res.Bind != c.wantBind {
			t.Errorf("%s: bind = %s, want %s", c.name, res.Bind, c.wantBind)
		}
		if res.SecureContext != c.wantSecure {
			t.Errorf("%s: secure_context = %v, want %v", c.name, res.SecureContext, c.wantSecure)
		}
		if (res.FellBack != "") != c.wantFall {
			t.Errorf("%s: fell_back = %q", c.name, res.FellBack)
		}
		if res.URL == "" && c.wantMode != ModeJS {
			t.Errorf("%s: no url reported", c.name)
		}
		if err := config.ValidateBind(res.Bind); err != nil {
			t.Errorf("%s: resolved bind is not acceptable: %v", c.name, err)
		}
	}
}

func TestLANFallbackWarnsLoudly(t *testing.T) {
	res := Resolve(config.Expose{Cloudflare: true, Domain: "herdr.deemwar.com"}, 21118, func(string) bool { return false })
	line := res.Describe()
	for _, want := range []string{"mode=lan", "bind=0.0.0.0:21118", "cloudflared is not installed", "WARNING", "secure context"} {
		if !strings.Contains(line, want) {
			t.Fatalf("startup line missing %q:\n%s", want, line)
		}
	}
}

func TestURLAndOriginsPerMode(t *testing.T) {
	// cloudflare: the domain, and the domain is an allowed origin.
	cf := Resolve(config.Expose{Cloudflare: true, Domain: "herdr.deemwar.com"}, 21118, func(string) bool { return true })
	if cf.URL != "https://herdr.deemwar.com" {
		t.Fatalf("cloudflare url = %s", cf.URL)
	}
	if !contains(cf.AllowedOrigins(nil), "https://herdr.deemwar.com") {
		t.Fatalf("cloudflare origins: %v", cf.AllowedOrigins(nil))
	}

	// lan: the LAN IP, which must also be an allowed origin or the browser
	// refuses the WebSocket.
	lan := Resolve(config.Expose{LAN: true}, 21118, func(string) bool { return true })
	if ip := PrimaryLANIP(); ip != "" {
		want := "http://" + ip + ":21118"
		if lan.URL != want {
			t.Fatalf("lan url = %s, want %s", lan.URL, want)
		}
		if !contains(lan.AllowedOrigins(nil), want) {
			t.Fatalf("lan origin missing: %v", lan.AllowedOrigins(nil))
		}
	}
	if !contains(lan.AllowedOrigins([]string{"https://pinned.example.com"}), "https://pinned.example.com") {
		t.Fatal("configured origins must be preserved")
	}
	if !contains(lan.AllowedOrigins(nil), "http://127.0.0.1:21118") {
		t.Fatal("loopback origin must always be allowed")
	}

	// local
	loc := Resolve(config.Expose{}, 21118, func(string) bool { return true })
	if loc.URL != "http://127.0.0.1:21118" {
		t.Fatalf("local url = %s", loc.URL)
	}
}

// A DHCP lease change must not leave a stale URL in status.
func TestNetWatcherRefreshUpdatesURL(t *testing.T) {
	res := Resolution{Mode: ModeLAN, Bind: config.BindAll, Port: 21118, LANIP: "10.0.0.99",
		URL: "http://10.0.0.99:21118"}
	var seen Resolution
	w := newNetWatcher(res, time.Hour, func(r Resolution) { seen = r })
	defer w.close()

	got, changed := w.refresh()
	real := PrimaryLANIP()
	if real == "10.0.0.99" {
		t.Skip("this machine really is on 10.0.0.99")
	}
	if !changed {
		t.Fatal("a changed LAN ip was not detected")
	}
	if got.LANIP != real || seen.LANIP != real {
		t.Fatalf("refresh did not adopt the new ip: %+v", got)
	}
	want := "http://127.0.0.1:21118"
	if real != "" {
		want = "http://" + real + ":21118"
	}
	if got.URL != want {
		t.Fatalf("url after refresh = %s, want %s", got.URL, want)
	}
	if _, changed := w.refresh(); changed {
		t.Fatal("a second refresh with no change should report no change")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// helpers ------------------------------------------------------------------

func syscallKill0(pid int) error { return syscall.Kill(pid, 0) }

func fmtSprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
