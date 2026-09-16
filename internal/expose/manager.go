// Package expose owns everything that puts the loopback server on the public
// internet: the built-in Cloudflare and ngrok providers (Go, in-process,
// supervised) and the goja-based JS adapter escape hatch.
//
// Nothing in this package ever persists, logs or returns a credential. API
// tokens are read from named environment variables at the point of use and
// handed to a child process's environment; anything registered as a secret is
// scrubbed out of every log line and every Status.
package expose

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// URLTimeout is how long Start waits for a public URL before giving up.
const URLTimeout = 90 * time.Second

// Options is what the server hands the Manager.
type Options struct {
	// Port is the loopback port the HTTP server listens on.
	Port int
	// Expose is the [expose] table.
	Expose config.Expose
	// Root is the directory relative adapter script paths resolve against
	// (normally $HERDR_PLUGIN_ROOT or the working directory).
	Root string
	// StateDir overrides where the Cloudflare credentials file and generated
	// config.yml are written. Empty => $HERDR_PLUGIN_STATE_DIR, else
	// ~/.local/state/herdr-expose.
	StateDir string
	// Logf receives already-scrubbed log lines. Optional.
	Logf func(format string, args ...any)
}

// Manager resolves the exposure mode, runs at most ONE tunnel at a time, and
// keeps the reported URL honest as the network moves under it.
type Manager struct {
	mu     sync.Mutex
	opts   Options
	red    *redactor
	cur    tunnel
	net    *netWatcher
	logged bool
}

// New builds a Manager. It does not start anything.
func New(opts Options) *Manager {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Manager{opts: opts, red: &redactor{}}
}

// Resolution is the settled exposure decision: mode, bind address, LAN IP and
// user-facing URL. It is computed on first use, logged once, and kept fresh by
// a background poll so a DHCP lease change cannot leave a stale URL in status.
func (m *Manager) Resolution() Resolution {
	m.mu.Lock()
	if m.net == nil {
		res := Resolve(m.opts.Expose, m.opts.Port, nil)
		m.net = newNetWatcher(res, 30*time.Second, func(r Resolution) {
			m.opts.Logf("expose: lan address changed, url is now %s", r.URL)
		})
		go m.net.run()
		if !m.logged {
			m.logged = true
			m.opts.Logf("%s", res.Describe())
		}
	}
	w := m.net
	m.mu.Unlock()
	return w.get()
}

// Refresh re-resolves mode and LAN IP. Call it on SIGHUP and on any network
// change notification.
func (m *Manager) Refresh() Resolution {
	m.Resolution() // ensure the watcher exists
	m.mu.Lock()
	res := Resolve(m.opts.Expose, m.opts.Port, nil)
	prev := m.net.get()
	m.net.set(res)
	w := m.net
	m.mu.Unlock()
	if prev.Mode != res.Mode || prev.Bind != res.Bind || prev.URL != res.URL {
		m.opts.Logf("%s", res.Describe())
	}
	return w.get()
}

// AllowedOrigins is the Origin allowlist for the resolved mode, merged with
// whatever the user pinned in [server].allowed_origins. In lan mode the LAN
// origin MUST be in this list or the browser refuses the WebSocket.
func (m *Manager) AllowedOrigins(configured []string) []string {
	return m.Resolution().AllowedOrigins(configured)
}

// SetExpose swaps in a new [expose] table (SIGHUP reload) and re-resolves the
// mode. It does not restart a running tunnel; the caller decides whether to
// bounce it.
func (m *Manager) SetExpose(e config.Expose, port int) Resolution {
	m.mu.Lock()
	m.opts.Expose = e
	if port > 0 {
		m.opts.Port = port
	}
	m.mu.Unlock()
	return m.Refresh()
}

// Start brings up the configured provider and blocks until a public URL is
// known (or the attempt fails). Starting twice is an error, not a second
// tunnel: exactly one adapter is active at a time.
func (m *Manager) Start(ctx context.Context) (Status, error) {
	res := m.Resolution()
	if !res.Remote {
		// lan and local modes have no tunnel process: the listener itself is
		// the exposure. Not an error — this is the documented fallback when
		// cloudflared is missing.
		if res.FellBack != "" {
			m.opts.Logf("expose: %s", res.FellBack)
		}
		m.opts.Logf("expose: nothing to start in %s mode; reachable at %s", res.Mode, res.URL)
		return m.Status(), nil
	}

	m.mu.Lock()
	if m.cur != nil {
		st := m.cur.Snapshot()
		m.mu.Unlock()
		return st, fmt.Errorf("a %s tunnel is already running (%s); stop it first", st.Provider, st.URL)
	}
	opts := m.opts
	m.mu.Unlock()

	t, err := m.build(opts)
	if err != nil {
		st := m.Status()
		st.LastError = m.red.scrub(err.Error())
		return st, err
	}

	m.mu.Lock()
	m.cur = t
	m.mu.Unlock()

	if err := t.Launch(ctx); err != nil {
		_ = t.Stop()
		m.mu.Lock()
		m.cur = nil
		m.mu.Unlock()
		st := m.Status()
		st.Provider, st.Tunnel = t.Name(), t.Mode()
		st.LastError = m.red.scrub(err.Error())
		return st, err
	}

	url, err := t.WaitForURL(ctx, URLTimeout)
	if err != nil {
		st := m.Status()
		_ = t.Stop()
		m.mu.Lock()
		m.cur = nil
		m.mu.Unlock()
		st.LastError = m.red.scrub(err.Error())
		return st, err
	}
	m.opts.Logf("expose: %s tunnel up at %s", t.Name(), url)
	return m.Status(), nil
}

// build picks the provider. JS adapter wins, then cloudflare, then ngrok.
func (m *Manager) build(opts Options) (tunnel, error) {
	switch opts.Expose.Provider() {
	case config.ProviderCloudflare:
		return newCloudflare(CloudflareOptions{
			Port:       opts.Port,
			Domain:     opts.Expose.Domain,
			TunnelName: opts.Expose.TunnelID(),
			StateDir:   opts.StateDir,
		}, opts.Logf, m.red)

	case config.ProviderNgrok:
		return newNgrok(NgrokOptions{
			Port:   opts.Port,
			Domain: opts.Expose.Domain,
		}, opts.Logf, m.red)

	case config.ProviderJS:
		id := opts.Expose.Adapter
		var adapter config.Adapter
		found := false
		for _, a := range opts.Expose.Adapters {
			if a.ID == id {
				adapter, found = a, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("expose.adapter %q has no [[expose.adapters]] entry", id)
		}
		script := adapter.Script
		if !filepath.IsAbs(script) {
			script = filepath.Join(opts.Root, script)
		}
		local := fmt.Sprintf("http://127.0.0.1:%d", opts.Port)
		return newJSTunnel(id, script, local, adapter.Env, opts.Logf, m.red)

	default:
		return nil, fmt.Errorf("nothing to expose: set `cloudflare = true` with `domain = \"herdr.example.com\"`, " +
			"or `ngrok = true` with a reserved domain, or `adapter = \"<id>\"` under [expose]")
	}
}

// Stop tears the tunnel down. It is idempotent: calling it when nothing is
// running, or twice in a row, returns nil and changes nothing.
func (m *Manager) Stop() error {
	m.mu.Lock()
	t := m.cur
	m.cur = nil
	m.mu.Unlock()
	if t == nil {
		return nil
	}
	return t.Stop()
}

// Status is the credential-free view: the resolved mode and bind address, plus
// whatever tunnel is running on top of it.
func (m *Manager) Status() Status {
	res := m.Resolution()
	m.mu.Lock()
	t := m.cur
	m.mu.Unlock()

	st := Status{
		Provider:      "none",
		Mode:          string(res.Mode),
		Bind:          res.Bind,
		Port:          res.Port,
		LANIP:         res.LANIP,
		URL:           res.URL,
		SecureContext: res.SecureContext,
		FellBack:      res.FellBack,
		Running:       !res.Remote, // in lan/local the listener IS the exposure
		Healthy:       !res.Remote,
	}
	if t != nil {
		snap := t.Snapshot()
		st.Provider, st.Tunnel = snap.Provider, snap.Tunnel
		st.Running, st.Healthy = snap.Running, snap.Healthy
		st.PID, st.Restarts, st.StartedAt = snap.PID, snap.Restarts, snap.StartedAt
		st.LastError = snap.LastError
		if snap.URL != "" {
			st.URL = snap.URL // a JS adapter reports its own hostname
		}
	}
	// Belt and braces: a secret must not reach a client through status.
	st.URL = m.red.scrub(st.URL)
	st.LastError = m.red.scrub(st.LastError)
	return st
}

// URL is the URL a user should open for the resolved mode.
func (m *Manager) URL() string { return m.Status().URL }

// Close stops the tunnel and the network watcher.
func (m *Manager) Close() error {
	err := m.Stop()
	m.mu.Lock()
	w := m.net
	m.mu.Unlock()
	if w != nil {
		w.close()
	}
	return err
}

// Plan reports what a real `expose start` would do to Cloudflare without
// changing anything. Read-only.
func (m *Manager) Plan(ctx context.Context) ([]PlanStep, error) {
	m.mu.Lock()
	opts := m.opts
	m.mu.Unlock()
	if opts.Expose.Provider() != config.ProviderCloudflare {
		return nil, fmt.Errorf("plan is only implemented for the built-in Cloudflare provider")
	}
	return PlanCloudflare(ctx, CloudflareOptions{
		Port:       opts.Port,
		Domain:     opts.Expose.Domain,
		TunnelName: opts.Expose.TunnelID(),
		StateDir:   opts.StateDir,
	})
}

// Destroy removes the provisioned Cloudflare resources: the DNS record (only
// if herdr-expose created it) and the named tunnel. `expose stop` deliberately
// does NOT do this — the domain is static, and tearing DNS down on every stop
// is the ephemeral behaviour that was removed.
func (m *Manager) Destroy(ctx context.Context) error {
	if err := m.Stop(); err != nil {
		return err
	}
	m.mu.Lock()
	opts := m.opts
	m.mu.Unlock()
	if opts.Expose.Provider() != config.ProviderCloudflare {
		return fmt.Errorf("destroy is only implemented for the built-in Cloudflare provider")
	}
	return DestroyCloudflare(ctx, CloudflareOptions{
		Port:       opts.Port,
		Domain:     opts.Expose.Domain,
		TunnelName: opts.Expose.TunnelID(),
		StateDir:   opts.StateDir,
	}, opts.Logf)
}
