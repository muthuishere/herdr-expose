package expose

import (
	"context"
	"fmt"
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
				map[string]string{"hostname": "herdr.example.com", "tunnel": "t", "domain": "d.ngrok.app"},
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
	if !st.Healthy || st.Provider != "js:test" || st.Mode != "js" {
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
	for i := 0; i < 3; i++ {
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop on an idle manager: %v", err)
		}
	}
	if st := m.Status(); st.Running || st.URL != "" {
		t.Fatalf("idle status: %+v", st)
	}
}

func TestManagerRefusesAnEmptyExposeTable(t *testing.T) {
	m := New(Options{Port: 21118})
	if _, err := m.Start(context.Background()); err == nil {
		t.Fatal("starting with nothing configured must fail")
	}
}

// helpers ------------------------------------------------------------------

func syscallKill0(pid int) error { return syscall.Kill(pid, 0) }

func fmtSprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
