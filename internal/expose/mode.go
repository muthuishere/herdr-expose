package expose

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// Exposure modes (AMENDMENT E1), resolved in this order:
//
//  1. a tunnel mode — cloudflare (or ngrok, or a JS adapter). Binds loopback;
//     the tunnel is the only remote path.
//  2. lan — binds 0.0.0.0, reachable by anyone on the wifi. Chosen when
//     `lan = true`, or AUTOMATICALLY when a tunnel was requested but its
//     binary is not installed. That fallback logs loudly; it never fails.
//  3. local — binds loopback. The default.
//
// LAN mode is only acceptable because auth is mandatory in every mode and the
// pairing code is shown ONLY on the physically-present machine: no HTTP
// endpoint mints or displays one. A neighbour on the wifi can reach the port
// and gets nowhere without looking at the owner's screen.
const (
	ModeCloudflare = config.ModeCloudflare
	ModeNgrok      = config.ModeNgrok
	ModeJS         = config.ModeJS
	ModeLAN        = config.ModeLAN
	ModeLocal      = config.ModeLocal
)

// Resolution is the settled answer to "what do we bind, and what URL do people
// type?". It is computed once at startup and refreshed on SIGHUP / network
// change (the LAN IP can move under DHCP).
//
// Hand it to the config Store so everything downstream reads the same mode and
// bind address through config.Provider:
//
//	res := mgr.Resolution()
//	store.SetBinding(res.Mode, res.Bind)
type Resolution struct {
	Mode config.Mode `json:"mode"` // cloudflare | ngrok | js | lan | local
	Bind string      `json:"bind"` // 127.0.0.1 or 0.0.0.0 — never user-settable
	Port int         `json:"port"`

	// LANIP is the primary non-loopback IPv4, empty when there is none.
	LANIP string `json:"lan_ip,omitempty"`
	// URL is what the user types / what the pairing QR encodes.
	URL string `json:"url"`
	// Remote is true for the tunnel modes.
	Remote bool `json:"remote"`
	// SecureContext is false for plain HTTP on a LAN IP. Browsers only grant
	// service workers and PWA install in a secure context, and a LAN IP — unlike
	// localhost — is not one. Real property of the mode, not a bug.
	SecureContext bool `json:"secure_context"`
	// FellBack explains an automatic downgrade, e.g. missing cloudflared.
	FellBack string `json:"fell_back,omitempty"`
}

// AllowedOrigins is the browser Origin allowlist for this mode, merged with
// anything the user pinned in config. Without the LAN origin the browser
// refuses the WebSocket in lan mode.
func (r Resolution) AllowedOrigins(configured []string) []string {
	out := make([]string, 0, len(configured)+4)
	seen := map[string]bool{}
	add := func(o string) {
		if o == "" || seen[o] {
			return
		}
		seen[o] = true
		out = append(out, o)
	}
	for _, o := range configured {
		add(strings.TrimSpace(o))
	}
	switch r.Mode {
	case ModeCloudflare, ModeNgrok, ModeJS:
		add(r.URL)
	case ModeLAN:
		if r.LANIP != "" {
			add(fmt.Sprintf("http://%s:%d", r.LANIP, r.Port))
		}
	}
	// Loopback is always allowed: the owner's own browser on this machine.
	add(fmt.Sprintf("http://127.0.0.1:%d", r.Port))
	add(fmt.Sprintf("http://localhost:%d", r.Port))
	return out
}

// Resolve decides the mode. binaryFound reports whether a helper binary is
// installed; pass nil for the real check.
func Resolve(e config.Expose, port int, binaryFound func(string) bool) Resolution {
	if binaryFound == nil {
		binaryFound = func(name string) bool {
			_, err := resolveBinary("", name)
			return err == nil
		}
	}
	r := Resolution{Port: port, Bind: config.BindLoopback, Mode: ModeLocal}

	switch e.Provider() {
	case config.ProviderCloudflare:
		if strings.TrimSpace(e.Domain) != "" && binaryFound("cloudflared") {
			r.Mode, r.Remote, r.URL = ModeCloudflare, true, "https://"+e.Domain
		} else if !binaryFound("cloudflared") {
			r.Mode = ModeLAN
			r.FellBack = "cloudflared is not installed, so the Cloudflare tunnel cannot start; " +
				"falling back to LAN mode (install cloudflared to get the public domain back)"
		}
	case config.ProviderNgrok:
		if strings.TrimSpace(e.Domain) != "" && binaryFound("ngrok") {
			r.Mode, r.Remote, r.URL = ModeNgrok, true, "https://"+e.Domain
		} else if !binaryFound("ngrok") {
			r.Mode = ModeLAN
			r.FellBack = "the ngrok binary is not installed; falling back to LAN mode"
		}
	case config.ProviderJS:
		r.Mode, r.Remote = ModeJS, true // the adapter reports its own URL
	}

	// An explicit `lan = true` wins over a bare local default, but never
	// downgrades a working tunnel (a tunnel already gives remote access, and
	// loopback keeps the wifi off the RCE surface).
	if r.Mode == ModeLocal && e.LAN {
		r.Mode = ModeLAN
	}

	switch r.Mode {
	case ModeLAN:
		r.Bind = config.BindAll
		r.LANIP = PrimaryLANIP()
		if r.LANIP != "" {
			r.URL = fmt.Sprintf("http://%s:%d", r.LANIP, r.Port)
		} else {
			r.URL = fmt.Sprintf("http://127.0.0.1:%d", r.Port)
		}
		r.SecureContext = false // plain HTTP on a LAN IP: no service worker, no PWA install
	case ModeLocal:
		r.Bind = config.BindLoopback
		r.URL = fmt.Sprintf("http://127.0.0.1:%d", r.Port)
		r.SecureContext = true // localhost is a secure context by definition
	default:
		r.Bind = config.BindLoopback
		r.SecureContext = true // https through the tunnel
		r.LANIP = PrimaryLANIP()
	}
	return r
}

// Describe is the single startup log line: mode, bind address, URL, and the
// LAN warning when it applies.
func (r Resolution) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "expose: mode=%s bind=%s:%d url=%s", r.Mode, r.Bind, r.Port, r.URL)
	if r.FellBack != "" {
		fmt.Fprintf(&b, " (fallback: %s)", r.FellBack)
	}
	if r.Mode == ModeLAN {
		b.WriteString(" — WARNING: anyone on this network can reach the port. " +
			"Pairing still requires a code shown only on this machine, and plain HTTP on a LAN IP " +
			"is not a secure context, so the PWA cannot be installed there.")
	}
	return b.String()
}

// PrimaryLANIP returns the primary non-loopback IPv4 address, or "".
//
// It asks the routing table which source address would be used to reach the
// internet (no packet is sent), which picks the right interface on a machine
// with several. It falls back to scanning interfaces.
func PrimaryLANIP() string {
	if conn, err := net.Dial("udp4", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP.To4() != nil && !addr.IP.IsLoopback() {
			return addr.IP.String()
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

// netWatcher keeps the resolved LAN IP fresh: a DHCP lease change must not
// leave a stale URL in status.
type netWatcher struct {
	mu       sync.RWMutex
	current  Resolution
	stop     chan struct{}
	stopOnce sync.Once
	onChange func(Resolution)
	interval time.Duration
}

func newNetWatcher(initial Resolution, interval time.Duration, onChange func(Resolution)) *netWatcher {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &netWatcher{current: initial, stop: make(chan struct{}), onChange: onChange, interval: interval}
}

func (w *netWatcher) get() Resolution {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current
}

func (w *netWatcher) set(r Resolution) {
	w.mu.Lock()
	w.current = r
	w.mu.Unlock()
}

// refresh re-resolves the LAN IP (and the URL derived from it) in place.
// Called on SIGHUP and by the poll loop.
func (w *netWatcher) refresh() (Resolution, bool) {
	cur := w.get()
	ip := PrimaryLANIP()
	if ip == cur.LANIP {
		return cur, false
	}
	cur.LANIP = ip
	if cur.Mode == ModeLAN {
		if ip != "" {
			cur.URL = fmt.Sprintf("http://%s:%d", ip, cur.Port)
		} else {
			cur.URL = fmt.Sprintf("http://127.0.0.1:%d", cur.Port)
		}
	}
	w.set(cur)
	if w.onChange != nil {
		w.onChange(cur)
	}
	return cur, true
}

func (w *netWatcher) run() {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.refresh()
		}
	}
}

func (w *netWatcher) close() {
	w.stopOnce.Do(func() { close(w.stop) })
}
