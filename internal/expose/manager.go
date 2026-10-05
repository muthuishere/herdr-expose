package expose

import (
	"context"
	"sync"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// Manager resolves where the server listens and keeps that answer honest as
// the network moves under it.
//
// It used to do a great deal more: hold a provider, own a lock so only one
// tunnel ran at a time, supervise a cloudflared subprocess, reconcile a DNS
// record and verify teardown. All of that went when the tunnel did. What is
// left is a resolver with a watcher on it, because a DHCP lease can still
// change the LAN IP under a URL somebody is reading off a QR code.
//
// Start and Stop remain, and do nothing. In lan and local there is no process
// to start: the listener IS the exposure. They are kept so callers that
// bracket a run with them keep compiling and keep reading sensibly.
type Manager struct {
	mu     sync.Mutex
	opts   Options
	net    *netWatcher
	logged bool
}

// Options configures a Manager.
type Options struct {
	// Port is the port the HTTP server listens on.
	Port int
	// Expose is the [expose] table.
	Expose config.Expose
	// Logf receives log lines. Optional.
	//
	// It used to be "already-scrubbed" lines, because the thing being logged
	// was a provider's output and that could carry a credential. Nothing here
	// runs a provider any more, so there is nothing to scrub.
	Logf func(format string, args ...any)
}

// Status is what `herdr-expose status`, /v1/config and the UI see.
type Status struct {
	Mode string `json:"mode"` // lan | local
	// Bind is the resolved listen address: 127.0.0.1, or 0.0.0.0 in lan mode.
	Bind string `json:"bind"`
	Port int    `json:"port"`
	// LANIP is the primary non-loopback IPv4, re-resolved on SIGHUP and every
	// 30s so a DHCP lease change cannot leave a stale URL here.
	LANIP string `json:"lan_ip,omitempty"`
	// SecureContext is false for plain HTTP on a LAN IP: browsers withhold
	// service workers and PWA install there (localhost is exempt, a LAN IP is
	// not). The web app still works; it just cannot be installed.
	SecureContext bool   `json:"secure_context"`
	URL           string `json:"url,omitempty"`
	// Reach is the one-line truth about who can get to this.
	Reach string `json:"reach"`
}

// New builds a Manager. It does not start anything.
func New(opts Options) *Manager {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Manager{opts: opts}
}

// Resolution is the current answer, with a watcher behind it.
func (m *Manager) Resolution() Resolution {
	m.mu.Lock()
	if m.net == nil {
		res := Resolve(m.opts.Expose, m.opts.Port, nil)
		m.net = newNetWatcher(res, 30*time.Second, func(r Resolution) {
			m.opts.Logf("exposure re-resolved: %s", r.Describe())
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

// Refresh re-resolves now, for SIGHUP.
func (m *Manager) Refresh() Resolution {
	m.Resolution() // ensure the watcher exists
	m.mu.Lock()
	res := Resolve(m.opts.Expose, m.opts.Port, nil)
	w := m.net
	m.mu.Unlock()
	if w.get() != res {
		w.set(res)
		m.opts.Logf("exposure re-resolved: %s", res.Describe())
	}
	return res
}

// URL is the address to hand a person, or "" when there is none.
func (m *Manager) URL() string { return m.Resolution().URL }

// AllowedOrigins is the configured list plus the origins this mode implies.
func (m *Manager) AllowedOrigins(configured []string) []string {
	return m.Resolution().AllowedOrigins(configured)
}

// SetExpose replaces the table, for SIGHUP.
func (m *Manager) SetExpose(e config.Expose, port int) Resolution {
	m.mu.Lock()
	m.opts.Expose = e
	if port > 0 {
		m.opts.Port = port
	}
	m.mu.Unlock()
	return m.Refresh()
}

// Start is a no-op, kept so callers read the same as before.
//
// There is no process to start. In lan and local the listener itself is the
// exposure, which was already true of those two rungs before the tunnel was
// removed — this is now true of every rung, because these are the only two.
func (m *Manager) Start(context.Context) (Status, error) { return m.Status(), nil }

// Stop is a no-op, for the same reason.
func (m *Manager) Stop() error { return nil }

// Close stops the network watcher.
func (m *Manager) Close() error {
	m.mu.Lock()
	w := m.net
	m.net = nil
	m.mu.Unlock()
	if w != nil {
		w.close()
	}
	return nil
}

// Status reports the current exposure, credential-free by construction: there
// are no credentials left in this package to leak.
func (m *Manager) Status() Status {
	res := m.Resolution()
	return Status{
		Mode: string(res.Mode), Bind: res.Bind, Port: res.Port,
		LANIP: res.LANIP, SecureContext: res.SecureContext, URL: res.URL,
		Reach: res.Reach(),
	}
}
