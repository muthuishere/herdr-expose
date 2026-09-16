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

// Manager runs at most ONE tunnel at a time and owns its lifecycle.
type Manager struct {
	mu   sync.Mutex
	opts Options
	red  *redactor
	cur  tunnel
}

// New builds a Manager. It does not start anything.
func New(opts Options) *Manager {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Manager{opts: opts, red: &redactor{}}
}

// SetExpose swaps in a new [expose] table (SIGHUP reload). It does not restart
// a running tunnel; the caller decides whether to bounce it.
func (m *Manager) SetExpose(e config.Expose, port int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts.Expose = e
	if port > 0 {
		m.opts.Port = port
	}
}

// Start brings up the configured provider and blocks until a public URL is
// known (or the attempt fails). Starting twice is an error, not a second
// tunnel: exactly one adapter is active at a time.
func (m *Manager) Start(ctx context.Context) (Status, error) {
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
		return Status{Provider: string(opts.Expose.Provider())}, err
	}

	m.mu.Lock()
	m.cur = t
	m.mu.Unlock()

	if err := t.Launch(ctx); err != nil {
		_ = t.Stop()
		m.mu.Lock()
		m.cur = nil
		m.mu.Unlock()
		return Status{Provider: t.Name(), Mode: t.Mode(), LastError: m.red.scrub(err.Error())}, err
	}

	url, err := t.WaitForURL(ctx, URLTimeout)
	if err != nil {
		st := t.Snapshot()
		_ = t.Stop()
		m.mu.Lock()
		m.cur = nil
		m.mu.Unlock()
		st.LastError = m.red.scrub(err.Error())
		return st, err
	}
	m.opts.Logf("expose: %s tunnel up at %s", t.Name(), url)
	return t.Snapshot(), nil
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

// Status is the credential-free view of the tunnel.
func (m *Manager) Status() Status {
	m.mu.Lock()
	t := m.cur
	provider := string(m.opts.Expose.Provider())
	m.mu.Unlock()
	if t == nil {
		return Status{Provider: provider, Mode: "", Running: false}
	}
	st := t.Snapshot()
	// Belt and braces: a secret must not reach a client through status.
	st.URL = m.red.scrub(st.URL)
	st.LastError = m.red.scrub(st.LastError)
	return st
}

// URL is the live public URL, empty when there is no tunnel.
func (m *Manager) URL() string { return m.Status().URL }

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
