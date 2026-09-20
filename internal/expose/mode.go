package expose

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// Exposure modes (AMENDMENT E1), on the four-rung ladder of AMENDMENTS 16 L1:
//
//	local   127.0.0.1:<port>                  this machine only
//	lan     http://<lan-ip>:<port>            this machine + this network
//	quick   https://<rand>.trycloudflare.com  the internet
//	domain  https://<your host>               the internet, your name
//
// The DEFAULT differs by caller, and deliberately so: the DAEMON defaults to
// local (it serves whoever is sitting at this machine, who just opens
// localhost:21118), while a SHARE defaults to lan (it exists to be opened from
// somewhere else, so loopback-only would make the feature pointless). Same
// principle — the least exposure that still does the job — applied to two
// different jobs. Both defaults live at the CALLER, in the [expose] table it
// builds; this function never picks one.
//
// Resolve is a PURE FUNCTION of the [expose] table it is handed: every rung
// above local has to be switched on in that table, and this function never
// turns one on by itself. Nothing here reads a config file, so the only way to
// climb is for the caller to ask — `share --quick` / `--domain` build the
// table from the FLAG, a bare `share` builds the LAN table, and `--local`
// hands over a zero Expose, which is loopback by construction
// (AMENDMENTS 16 L1).
//
// The ONE automatic movement is DOWNWARD: a requested tunnel whose binary is
// not installed falls back to lan, loudly, via FellBack. That is a failure
// mode of an explicit ask, not a default — the person did ask to be reachable,
// and a LAN address is the nearest honest answer. Nothing in here can move
// UPWARD (L2): every branch below either keeps the requested rung or lowers
// it, and cmd/herdr-expose's TestResolveNeverEscalates holds the whole table
// of user-requestable rungs to that.
//
// Why the asymmetry is worth the inconvenience: this binary runs arbitrary
// commands inside the owner's agent sessions, and the blast radius of each
// rung differs by orders of magnitude. The difference between rung 1 and rung
// 3 must never be a config key somebody forgot they set, because that is what
// makes the pairing, scope and TTL guarantees elsewhere in this spec mean
// anything.
//
// LAN mode is only acceptable because auth is mandatory in every mode and the
// pairing code is shown ONLY on the physically-present machine: no HTTP
// endpoint mints or displays one. A neighbour on the wifi can reach the port
// and gets nowhere without looking at the owner's screen.
// quickBinary names the helper the ephemeral rung needs for the selected
// provider, so that `--quick --provider ngrok` checks for ngrok rather than
// silently checking for cloudflared and reporting the wrong missing tool.
func quickBinary(e config.Expose) (bin, label string) {
	if e.Provider() == config.ProviderNgrok {
		return "ngrok", "ngrok"
	}
	return "cloudflared", "cloudflare"
}

const (
	ModeCloudflare = config.ModeCloudflare
	ModeQuick      = config.ModeQuick
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
	case ModeCloudflare, ModeNgrok, ModeJS, ModeQuick:
		// For a quick tunnel r.URL is empty until the hostname is scraped;
		// Manager.AllowedOrigins appends the live one as soon as it is known.
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

// Rung is where this resolution sits on the exposure ladder (AMENDMENTS 16
// L1). Callers compare rungs rather than re-deriving "is this more exposed
// than that" from a mode name.
func (r Resolution) Rung() config.Rung { return config.RungOf(r.Mode) }

// Reach is the plain-language answer to "who can actually get at this?", for
// the line printed when an exposure is created.
func (r Resolution) Reach() string { return r.Rung().Reach() }

// Resolve decides the mode. binaryFound reports whether a helper binary is
// installed; pass nil for the real check.
//
// It starts at the bottom rung and only climbs where the [expose] table it was
// handed explicitly asks it to (AMENDMENTS 16 L1). A zero Expose therefore
// resolves to loopback, which is what makes the daemon's local default — and
// `share --local` — true by construction rather than by a default somewhere up
// the call stack.
func Resolve(e config.Expose, port int, binaryFound func(string) bool) Resolution {
	if binaryFound == nil {
		binaryFound = func(name string) bool {
			_, err := resolveBinary("", name)
			return err == nil
		}
	}
	// The floor. Every branch below either leaves this alone or is a rung the
	// caller explicitly switched on.
	r := Resolution{Port: port, Bind: config.BindLoopback, Mode: ModeLocal}

	// Quick (AMENDMENTS 15) is checked FIRST and is deliberately unreachable
	// from the config file: `Quick` has no TOML key, so only `share --quick`
	// can set it.
	//
	// It is a RUNG, not a cloudflare spelling: "the ephemeral tunnel of
	// whichever provider was selected". cloudflared's TryCloudflare and
	// ngrok's unreserved tunnel are the same rung — a throwaway public
	// hostname with nothing reserved, nothing provisioned in an account and
	// nothing to clean up — so a user who picks ngrok gets the same ladder,
	// and the same guarantees, as one who picks cloudflare. There is nothing
	// to provision either way, so the only question is whether the selected
	// provider's binary is installed.
	if e.Quick {
		bin, label := quickBinary(e)
		if binaryFound(bin) {
			r.Mode, r.Remote = ModeQuick, true
			// The hostname is assigned by the provider's edge when the process
			// starts and scraped from its output, so there is no URL to
			// predict here.
			r.Bind, r.SecureContext, r.LANIP = config.BindLoopback, true, PrimaryLANIP()
			return r
		}
		r.Mode = ModeLAN
		r.FellBack = bin + " is not installed, so no quick tunnel can be started; " +
			"falling back to LAN mode (install " + bin + " to get a public https URL from " + label + ")"
	}

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
	url := r.URL
	if url == "" && r.Mode == ModeQuick {
		url = "<assigned by cloudflare at start>"
	}
	fmt.Fprintf(&b, "expose: mode=%s bind=%s:%d url=%s", r.Mode, r.Bind, r.Port, url)
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
