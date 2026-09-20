// Package expose owns everything that puts the loopback server on the public
// internet: the built-in Cloudflare provider (Go, in-process, supervised) and
// the goja-based JS adapter escape hatch, which is how anything else —
// ngrok, tailscale, a corporate proxy — gets carried.
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
	// DNSComment overrides the DNS record comment tag. Empty means the
	// permanent tag; a share passes expose.ShareDNSComment so that its
	// ephemeral teardown can never delete the permanent deployment's record.
	DNSComment string
	// Exclusive declares that this deployment owns its tunnel NAME outright,
	// which is true for a share and false for the permanent deployment. See
	// CloudflareOptions.Exclusive: it decides whether an orphaned named tunnel
	// with no recoverable credentials is deleted and recreated, or refused.
	Exclusive bool

	// Logf receives already-scrubbed log lines. Optional.
	Logf func(format string, args ...any)
}

// Manager resolves the exposure mode, runs at most ONE tunnel at a time, and
// keeps the reported URL honest as the network moves under it.
type Manager struct {
	mu     sync.Mutex
	opts   Options
	red    *redactor
	cur    Provider
	lock   *exposeLock
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
	res := m.Resolution()
	m.mu.Lock()
	t := m.cur
	m.mu.Unlock()
	if t != nil {
		// A quick tunnel's hostname is not known until the edge assigns it, so
		// the Resolution cannot carry it. internal/serve re-reads this list on
		// every request, so appending the live URL here is what makes the
		// browser's Origin AND the Host pin match a *.trycloudflare.com share.
		if u := t.Snapshot().URL; u != "" && u != res.URL {
			configured = append(append([]string{}, configured...), u)
		}
	}
	return res.AllowedOrigins(configured)
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
// known (or the attempt fails).
//
// IT IS IDEMPOTENT, at two levels, because `start` is the verb people re-run:
//
//   - IN PROCESS: a second Start while a tunnel is up returns THAT tunnel's
//     status and nil. It does not raise a second one and it is not an error.
//     Exactly one provider is active per Manager, and asking for the state you
//     already have is not a failure.
//   - ACROSS PROCESSES: an advisory flock on the deployment's state dir means
//     `herdr-expose expose start` typed while the daemon supervises the tunnel
//     reports the running exposure instead of provisioning a second connector
//     against the same named tunnel. Cloudflare accepts two connectors without
//     complaint, so nothing else would have caught it.
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
		m.opts.Logf("expose: %s tunnel is already running (%s); nothing to do", st.Provider, st.URL)
		return st, nil
	}
	opts := m.opts
	m.mu.Unlock()

	lock, held, lerr := tryExposeLock(opts.StateDir)
	if lerr != nil {
		st := m.Status()
		st.LastError = m.red.scrub(lerr.Error())
		return st, lerr
	}
	if !held {
		// Another process in this deployment owns the exposure. Re-running
		// `start` is a no-op, not a second tunnel and not an error.
		m.opts.Logf("expose: another herdr-expose process already owns this exposure " +
			"(state dir lock held); leaving it alone")
		st := m.Status()
		st.Running = true
		return st, nil
	}

	plan, err := m.plan(opts)
	if err != nil {
		lock.release()
		st := m.Status()
		st.LastError = m.red.scrub(err.Error())
		return st, err
	}
	t, err := plan.New()
	if err != nil {
		lock.release()
		st := m.Status()
		st.LastError = m.red.scrub(err.Error())
		return st, err
	}
	m.opts.Logf("expose: %s", t.Footprint().Describe())

	m.mu.Lock()
	m.cur, m.lock = t, lock
	m.mu.Unlock()

	if err := t.Launch(ctx); err != nil {
		_ = t.Stop()
		m.mu.Lock()
		m.cur, m.lock = nil, nil
		m.mu.Unlock()
		lock.release()
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
		m.cur, m.lock = nil, nil
		m.mu.Unlock()
		lock.release()
		st.LastError = m.red.scrub(err.Error())
		return st, err
	}
	m.opts.Logf("expose: %s tunnel up at %s", t.Name(), url)
	return m.Status(), nil
}

// plan is the single place that maps a configuration onto a provider: which
// one it is, what it CREATES, how it is built, and how it is torn down.
//
// Creation and teardown come out of the same function on purpose. `destroy`
// runs when there may never have been a provider object — after a crash, from
// a different process, against a half-finished provisioning — so teardown
// cannot be a method that only exists once creation succeeded. Deriving both
// from the same configuration is what makes them symmetric even then.
//
// The rungs map onto PROVIDERS, not onto cloudflare spellings: `Quick` means
// "the ephemeral rung of whichever provider is selected" (ADR 0028), which is
// why it is answered here, per provider, rather than being hard-wired to
// cloudflared somewhere further down.
func (m *Manager) plan(opts Options) (providerPlan, error) {
	kind := opts.Expose.Provider()

	if opts.Expose.Quick {
		co := CloudflareOptions{Port: opts.Port}
		return providerPlan{
			Kind:      ModeQuick,
			Footprint: quickFootprint(),
			New:       func() (Provider, error) { return newQuickTunnel(co, opts.Logf, m.red) },
			Destroy:   noDestroy,
		}, nil
	}

	switch kind {
	case config.ProviderCloudflare:
		co := m.cloudflareOptionsFrom(opts)
		return providerPlan{
			Kind:      ModeCloudflare,
			Footprint: co.Footprint(),
			New:       func() (Provider, error) { return newCloudflare(co, opts.Logf, m.red) },
			Destroy: func(ctx context.Context, logf func(string, ...any)) error {
				return DestroyCloudflare(ctx, co, logf)
			},
		}, nil

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
			return providerPlan{}, fmt.Errorf("expose.adapter %q has no [[expose.adapters]] entry", id)
		}
		script := adapter.Script
		if !filepath.IsAbs(script) {
			script = filepath.Join(opts.Root, script)
		}
		local := fmt.Sprintf("http://127.0.0.1:%d", opts.Port)
		newJS := func() (Provider, error) {
			return newJSTunnel(id, script, local, adapter.Env, opts.Logf, m.red)
		}
		return providerPlan{
			Kind:      ModeJS,
			Footprint: Footprint{Provider: "js:" + id, Ephemeral: true},
			New:       newJS,
			Destroy: func(ctx context.Context, logf func(string, ...any)) error {
				// The adapter is the only thing that knows what it made, so
				// destroy has to go through it: load it, ask, stop it. The
				// host still never claims to have removed a remote resource.
				j, err := newJS()
				if err != nil {
					return err
				}
				return j.Destroy(ctx, logf)
			},
		}, nil

	default:
		return providerPlan{}, fmt.Errorf("nothing to expose: set `cloudflare = true` with `domain = \"herdr.example.com\"`, " +
			"or `adapter = \"<id>\"` under [expose] to run a JS adapter (the escape hatch for any other transport)")
	}
}

// Footprint is what the configured provider creates in the world. It is
// available BEFORE anything is started, which is what lets `plan` and the
// teardown paths agree on the same set of resources.
func (m *Manager) Footprint() Footprint {
	m.mu.Lock()
	opts := m.opts
	m.mu.Unlock()
	p, err := m.plan(opts)
	if err != nil {
		return Footprint{Provider: "none"}
	}
	return p.Footprint
}

// Stop tears the tunnel down. It is idempotent: calling it when nothing is
// running, or twice in a row, returns nil and changes nothing.
func (m *Manager) Stop() error {
	m.mu.Lock()
	t, lock := m.cur, m.lock
	m.cur, m.lock = nil, nil
	m.mu.Unlock()
	if t == nil {
		lock.release()
		return nil
	}
	err := t.Stop()
	// The cross-process lock is released only after the process is actually
	// down, so a `start` racing a `stop` cannot take the lock while the old
	// cloudflared is still connected.
	lock.release()
	return err
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
	return PlanCloudflare(ctx, m.CloudflareOptions())
}

// Destroy removes the provisioned Cloudflare resources: the DNS record (only
// if herdr-expose created it) and the named tunnel. `expose stop` deliberately
// does NOT do this — the domain is static, and tearing DNS down on every stop
// is the ephemeral behaviour that was removed.
// Destroy works for EVERY provider, and is idempotent for every one of them.
//
// It stops first (Cloudflare refuses to delete a tunnel with a live connector,
// so destroying while our own process is still connected is how a DNS record
// gets deleted and a tunnel orphaned), then removes exactly what the plan's
// Footprint declares. A provider that creates nothing has a Destroy that says
// so and returns nil, rather than an error telling the user their provider is
// second class.
func (m *Manager) Destroy(ctx context.Context) error {
	if err := m.Stop(); err != nil {
		return err
	}
	m.mu.Lock()
	opts := m.opts
	m.mu.Unlock()
	plan, err := m.plan(opts)
	if err != nil {
		// Nothing configured is nothing to destroy: a second `destroy` after a
		// successful one, or one against an unconfigured deployment, succeeds.
		opts.Logf("expose: nothing configured to destroy")
		return nil
	}
	if plan.Footprint.Ephemeral && plan.Footprint.NamedTunnel == "" && plan.Footprint.DNSRecord == "" {
		opts.Logf("expose: %s — nothing to destroy (%s)", plan.Footprint.Provider, plan.Footprint.Describe())
		return nil
	}
	return plan.Destroy(ctx, opts.Logf)
}

// CloudflareOptions is the provider view of this manager's configuration, so
// that plan / destroy / verify all resolve the same hostname, tunnel name,
// state dir and DNS tag BY CONSTRUCTION rather than from a caller's argument.
func (m *Manager) CloudflareOptions() CloudflareOptions {
	m.mu.Lock()
	opts := m.opts
	m.mu.Unlock()
	return m.cloudflareOptionsFrom(opts)
}

func (m *Manager) cloudflareOptionsFrom(opts Options) CloudflareOptions {
	return CloudflareOptions{
		Port:       opts.Port,
		Domain:     opts.Expose.Domain,
		TunnelName: opts.Expose.TunnelID(),
		StateDir:   opts.StateDir,
		Comment:    opts.DNSComment,
		// A SHARE owns its tunnel name outright (`herdr-expose-share-<id>`),
		// so it may delete and recreate an orphan whose secret was lost to a
		// crash. The permanent deployment may not — see CloudflareOptions.
		Exclusive: opts.Exclusive,
	}
}
