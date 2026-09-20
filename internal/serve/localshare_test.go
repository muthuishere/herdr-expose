package serve

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/core"
)

// A LOCAL share (AMENDMENTS 16 L1, rung 0 — `herdr-expose share --local`) and
// the daemon's own default binding both put this server on a loopback
// listener, where the F1 bypass applies: a device token adds nothing against
// someone who already has a shell on this machine. The obvious worry is that
// "no token" therefore means "wide open to anything running here".
//
// It does not, and this test is what says so. In local mode the token is
// REPLACED, not removed, by two checks that are not optional:
//
//   - the Origin allowlist, so a web page the owner happens to visit cannot
//     drive a server that runs arbitrary commands, and
//   - Host pinning, so a rebound DNS name (the actual attack on a local
//     service) arrives with a Host that does not match and is refused.
//
// Both are derived from the LISTENER, so a share cannot opt out of them, and
// both can only DENY — a forged header buys nothing.
func newLoopbackShareServer(t *testing.T) (*Server, string) {
	t.Helper()
	log := testLogger()
	auth, err := NewAuth(t.TempDir(), log, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	// A real loopback listener on an ephemeral port: exactly what a local
	// share binds, and exactly where localMode takes its grant from.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	s, err := New(Options{
		Version: "test",
		Config: &testConfig{
			port: 0, bind: "127.0.0.1", mode: "local",
			// A local share configures NO extra origins: the loopback forms
			// that newLocalMode pins are the entire allowlist.
			origins: nil,
		},
		Hub:  core.NewHub(core.NewStore(log), log),
		Auth: auth,
		Log:  log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.local = newLocalMode(ln)
	if !s.local.enabled {
		t.Fatal("a loopback listener did not enable local mode")
	}
	return s, port
}

func TestLocalShareIsNotOpenToAnyBrowserPage(t *testing.T) {
	s, port := newLoopbackShareServer(t)
	base := "http://127.0.0.1:" + port

	// The owner's own tab: loopback Host, loopback Origin. This is the one
	// caller local mode is for, and it works without pairing.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, base+"/v1/config", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Origin", base)
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the owner's own loopback tab was refused: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["auth_required"] != false {
		t.Fatalf("local mode did not apply on a loopback share: %s", rec.Body.String())
	}

	// A HOSTILE PAGE the owner happens to have open. It can absolutely send
	// requests to 127.0.0.1 — that is the whole attack — but the browser
	// stamps its own Origin on them, and the allowlist refuses it.
	for _, origin := range []string{
		"https://evil.example",
		"http://evil.example",
		"http://127.0.0.1.evil.example",
		"null",
	} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, base+"/v1/config", nil)
		req.RemoteAddr = "127.0.0.1:50001"
		req.Header.Set("Origin", origin)
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("Origin %q reached a local share: %d %s", origin, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("a refused Origin was reflected back as CORS: %q", got)
		}
	}

	// DNS REBINDING: the attacker's name resolves to 127.0.0.1, so the request
	// really does arrive on the loopback listener from a loopback peer. Host
	// pinning is what stops it, and it is not waived by the bypass.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://rebound.evil.example:"+port+"/v1/config", nil)
	req.RemoteAddr = "127.0.0.1:50002"
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a rebound Host reached a local share: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "forbidden host") {
		t.Fatalf("rebinding was refused for the wrong reason: %s", rec.Body.String())
	}

	// A state-changing request with NO Origin is not a browser request we can
	// vouch for, on loopback as much as anywhere.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, base+"/v1/metrics", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:50003"
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an origin-less POST reached a local share: %d %s", rec.Code, rec.Body.String())
	}
}

// The socket is the path that matters — it is the one that can type into the
// owner's panes — so the same two checks are asserted on the upgrade itself,
// where the bypass is actually granted.
func TestLocalShareWebSocketStillPinsOriginAndHost(t *testing.T) {
	s, port := newLoopbackShareServer(t)
	base := "http://127.0.0.1:" + port
	const chrome = "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/131 Safari/537.36"

	// Hostile page, loopback target: refused before the upgrade.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, base+"/v1/stream", nil)
	req.RemoteAddr = "127.0.0.1:50010"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("User-Agent", chrome)
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a hostile Origin got a socket on a local share: %d %s", rec.Code, rec.Body.String())
	}

	// A browser upgrade with NO Origin is refused rather than allowed: a
	// missing Origin must never be the way around the allowlist.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, base+"/v1/stream", nil)
	req.RemoteAddr = "127.0.0.1:50011"
	req.Header.Set("User-Agent", chrome)
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a browser upgrade with no Origin got through: %d %s", rec.Code, rec.Body.String())
	}

	// Rebound Host on the socket: same answer.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://rebound.evil.example:"+port+"/v1/stream", nil)
	req.RemoteAddr = "127.0.0.1:50012"
	req.Header.Set("Origin", "http://rebound.evil.example:"+port)
	req.Header.Set("User-Agent", chrome)
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a rebound Host got a socket on a local share: %d %s", rec.Code, rec.Body.String())
	}
}

// A share that is reached THROUGH a tunnel lands on the same loopback
// listener, so the bypass has to distinguish them. It does, by Host and by the
// CDN headers the request carries — which is what keeps the pairing gate real
// for the three rungs above local.
func TestTunnelledRequestToAShareStillNeedsAToken(t *testing.T) {
	s, port := newLoopbackShareServer(t)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/v1/config", nil)
	req.RemoteAddr = "127.0.0.1:50020" // cloudflared, on loopback
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	if s.local.bypassAuth(req) {
		t.Fatal("a request carrying CDN headers was granted the local-mode bypass")
	}
}
