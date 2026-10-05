package expose

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// Exposure modes (AMENDMENT E1), on the two-rung ladder of AMENDMENTS 16 L1:
//
//	local   127.0.0.1:<port>         this machine only
//	lan     http://<lan-ip>:<port>   this machine + this network
//
// There is nothing above lan. The rungs that reached the internet went with
// the tunnel; see Resolve.
//
// The DEFAULT differs by caller, and deliberately so: the DAEMON defaults to
// local (it serves whoever is sitting at this machine, who just opens
// localhost:21118), while a SHARE defaults to lan (it exists to be opened from
// somewhere else, so loopback-only would make the feature pointless). Same
// principle — the least exposure that still does the job — applied to two
// different jobs. Both defaults live at the CALLER, in the [expose] table it
// builds; this function never picks one.
//
// Resolve is a PURE FUNCTION of the [expose] table it is handed: the rung
// above local has to be switched on in that table, and this function never
// turns it on by itself. Nothing here reads a config file, so the only way to
// climb is for the caller to ask — a bare `share` builds the LAN table, and
// `--local` hands over a zero Expose, which is loopback by construction.
//
// Nothing in here can move UPWARD (L2), and cmd/herdr-expose's
// TestResolveNeverEscalates holds the whole table of user-requestable rungs to
// that. The asymmetry still earns its keep with two rungs: this binary runs
// arbitrary commands inside the owner's agent sessions, and the difference
// between "this machine" and "this café's wifi" must never be a config key
// somebody forgot they set.
//
// LAN mode is only acceptable because auth is mandatory in every mode and the
// pairing code is shown ONLY on the physically-present machine: no HTTP
// endpoint mints or displays one. A neighbour on the wifi can reach the port
// and gets nowhere without looking at the owner's screen.
const (
	ModeLAN   = config.ModeLAN
	ModeLocal = config.ModeLocal
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
	Mode config.Mode `json:"mode"` // lan | local
	Bind string      `json:"bind"` // 127.0.0.1 or 0.0.0.0 — never user-settable
	Port int         `json:"port"`

	// LANIP is the primary non-loopback IPv4, empty when there is none.
	LANIP string `json:"lan_ip,omitempty"`
	// URL is what the user types / what the pairing QR encodes.
	URL string `json:"url"`
	// SecureContext is false for plain HTTP on a LAN IP. Browsers only grant
	// service workers and PWA install in a secure context, and a LAN IP — unlike
	// localhost — is not one. Real property of the mode, not a bug.
	SecureContext bool `json:"secure_context"`
	// FellBack explained an automatic downgrade. Nothing downgrades any more —
	// there is no helper binary that can be missing — so it is always empty on
	// a fresh resolution. The field stays because a share RECORD written by an
	// older version can carry one, and `share list` must keep printing it.
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
	if r.Mode == ModeLAN && r.LANIP != "" {
		add(fmt.Sprintf("http://%s:%d", r.LANIP, r.Port))
	}
	// Loopback is always allowed: the owner's own browser on this machine.
	add(fmt.Sprintf("http://127.0.0.1:%d", r.Port))
	add(fmt.Sprintf("http://localhost:%d", r.Port))
	return out
}

// Rung is where this resolution sits on the exposure ladder (AMENDMENTS 16
// L1). Callers compare rungs rather than re-deriving "is this more exposed
// than that" from a mode name.
func (r Resolution) Rung() config.Rung { return config.RungOf(r.Mode) }

// Reach is the plain-language answer to "who can actually get at this?", for
// the line printed when an exposure is created.
func (r Resolution) Reach() string { return r.Rung().Reach() }

// Resolve decides where the server listens and what URL to show.
//
// TWO RUNGS, and there will not be a third here. herdr-expose used to carry a
// tunnel: a named Cloudflare hostname for the daily driver and an ephemeral
// *.trycloudflare.com for a share — provider API calls, subprocess
// supervision, DNS reconciliation and teardown verification, all inside the
// binary that also serves the panes. It is gone, deliberately and completely,
// and tunnelling is now a separate tool (cftunnel) pointed at this port.
//
// What that buys: this binary reaches exactly as far as the wire it is plugged
// into. No account to hold credentials for, no third party terminating TLS for
// somebody else's agent sessions, no hostname that outlives the process that
// made it, and no code path that can put a pane on the public internet by
// accident. The failure mode of every bug in here becomes "unreachable",
// never "reachable by strangers".
//
// What it costs, stated plainly: nothing off this network reaches the server
// by itself. Reaching it from elsewhere is the owner's own business now — a
// tunnel they run, a VPN, an SSH forward — and that is a better place for the
// decision than inside this binary.
//
// binaryFound stays in the signature and is ignored: nothing here shells out
// any more, and every caller and test that passes a stub keeps compiling.
func Resolve(e config.Expose, port int, binaryFound func(string) bool) Resolution {
	_ = binaryFound

	// The floor is loopback. The one rung above it is a switch the caller set.
	r := Resolution{Port: port, Bind: config.BindLoopback, Mode: ModeLocal}
	if e.LAN {
		r.Mode = ModeLAN
	}

	switch r.Mode {
	case ModeLAN:
		r.Bind = config.BindAll
		r.LANIP = PrimaryLANIP()
		if r.LANIP != "" {
			r.URL = fmt.Sprintf("http://%s:%d", r.LANIP, r.Port)
		} else {
			// Bound to every interface with no address to name: say loopback
			// rather than invent a hostname nobody can reach.
			r.URL = fmt.Sprintf("http://127.0.0.1:%d", r.Port)
		}
		r.SecureContext = false // plain HTTP on a LAN IP: no service worker, no PWA install
	default:
		r.Bind = config.BindLoopback
		r.URL = fmt.Sprintf("http://127.0.0.1:%d", r.Port)
		r.SecureContext = true // localhost is a secure context by definition
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
	fmt.Fprintf(&b, " reach=%q", r.Reach())
	if r.Mode == ModeLocal {
		b.WriteString(" — loopback only: nothing off this machine can reach it, " +
			"and the Origin allowlist plus Host pinning still stand between it and any web page you visit.")
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
