package serve

import (
	"net"
	"net/http"
	"strings"
)

// Local mode (SPEC amendment F1).
//
// When the listener is bound to loopback, a device token adds nothing: anyone
// who can reach 127.0.0.1 already has a shell on this machine. What a token
// never protected against in the first place is the real attack on a local
// service — the BROWSER. Any page the user visits can issue requests to
// 127.0.0.1, and DNS rebinding turns a hostile site into a client of a server
// that runs arbitrary commands.
//
// So in local mode the token is replaced by two checks that are NOT optional:
//
//   - Origin allowlist, strictly enforced on the WS upgrade and every non-GET.
//     A missing Origin on a browser-initiated upgrade is rejected, not allowed.
//   - Host pinning. A rebound DNS name arrives in Host and will not match
//     127.0.0.1:<port> or localhost:<port>, which is exactly what stops it.
//
// The bypass is derived from the LISTENER, never from a header. The extra
// checks below can only DENY, never grant, so a spoofed header cannot buy
// anything.
type localMode struct {
	enabled bool     // the listener is bound to loopback
	port    string   // the port we actually listen on
	hosts   []string // acceptable Host values
	origins []string // acceptable Origin values
}

func newLocalMode(ln net.Listener) localMode {
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return localMode{}
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		// Bound to a real interface: every request needs a token.
		return localMode{port: port}
	}
	return localMode{
		enabled: true,
		port:    port,
		hosts: []string{
			"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port,
		},
		origins: []string{
			"http://127.0.0.1:" + port, "http://localhost:" + port,
			"http://[::1]:" + port,
		},
	}
}

// hostPinned reports whether Host is one of the loopback forms.
func (l localMode) hostPinned(r *http.Request) bool {
	h := strings.ToLower(r.Host)
	for _, want := range l.hosts {
		if h == want {
			return true
		}
	}
	return false
}

// originPinned reports whether Origin is one of the loopback forms.
func (l localMode) originPinned(origin string) bool {
	o := strings.ToLower(strings.TrimSpace(origin))
	for _, want := range l.origins {
		if o == want {
			return true
		}
	}
	return false
}

// proxied reports whether a request carries evidence of having crossed a proxy
// or CDN. Used ONLY to deny the token bypass, never to grant anything, so a
// forged header can only make us stricter.
func proxied(r *http.Request) bool {
	for _, h := range []string{
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"Forwarded", "CF-Connecting-IP", "CF-Ray", "Cf-Warp-Tag-Id",
	} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// remoteIsLoopback checks the actual peer address.
func remoteIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bypassAuth reports whether this request may skip the token.
//
// Every condition must hold. The listener check is the one that grants; the
// rest only take the grant away. In particular a request that arrived through
// the Cloudflare tunnel reaches the same loopback listener, so it is
// distinguished by its Host (the tunnel hostname, not 127.0.0.1:<port>) and by
// the CDN headers it carries — and it therefore still needs a device token.
func (l localMode) bypassAuth(r *http.Request) bool {
	return l.enabled &&
		remoteIsLoopback(r) &&
		l.hostPinned(r) &&
		!proxied(r)
}
