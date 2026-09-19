package serve

import (
	"sync"
)

// connGate decides whether a WebSocket handshake may proceed.
//
// WHY IT IS NOT KEYED ON THE REMOTE IP — this is the bug a stress test found.
// Behind cloudflared (and behind any reverse proxy) every remote client reaches
// the loopback listener as 127.0.0.1, so an IP-keyed token bucket is ONE bucket
// for the ENTIRE public endpoint. Measured on the old code: 20 clients
// reconnecting for 90s produced 55 successful upgrades and 8,735 HTTP 429s, a
// sustained ceiling of one new socket every two seconds for everybody. One
// phone flapping on a bad link denied service to the laptop, the tablet and the
// PWA.
//
// So the budget follows IDENTITY, not address: every paired device gets its own
// generous reconnect budget, and identities are independent by construction, so
// one device cannot starve another no matter how hard it flaps.
//
// TRUST BOUNDARY (SPEC F1): the identity used here is the one the token already
// proved in Auth.Authenticate — this gate runs AFTER authentication, never
// before. `X-Forwarded-For` / `CF-Connecting-IP` / `Forwarded` are NOT read
// here and must never become an input to it: those headers are attacker-chosen,
// and under F1 a proxy header may only ever DENY a request, never grant one or
// identify who is making it. If anyone later wants finer buckets for several
// clients behind one device token, the only defensible use is to SUBDIVIDE an
// already-authenticated device's own budget (strictly less capacity than the
// device already has), never to mint a new one — a spoofable header that
// creates budget is a free bypass.
//
// The rate bucket alone is not enough, because an authenticated attacker could
// hold thousands of sockets open without ever tripping a *rate*. Hence two hard
// concurrency caps: per identity, and a global backstop for the process.
const (
	// HandshakeBurstPerIdentity is one device's reconnect burst. A page reload
	// on a flaky link can legitimately produce a rapid run of upgrades (PWA
	// resume, tab restore, network flap), so this is deliberately generous.
	HandshakeBurstPerIdentity = 30
	// HandshakeRefillPerIdentity is that device's sustained rate, per second.
	HandshakeRefillPerIdentity = 1.0

	// HandshakeBurstPerIP / HandshakeRefillPerIP apply when there is no proven
	// identity to key on (local-mode loopback clients). Loopback already means
	// shell on the box, so this only bounds runaway local clients.
	HandshakeBurstPerIP  = 30
	HandshakeRefillPerIP = 1.0

	// MaxConnsPerIdentity caps sockets held at once by one identity. A real
	// client holds one; a few tabs plus a stale socket awaiting its read
	// deadline is still far below this.
	MaxConnsPerIdentity = 16
	// MaxConnsTotal is the process-wide backstop, so no set of authenticated
	// identities can exhaust file descriptors or hub fanout capacity.
	MaxConnsTotal = 128
)

// connGate is the rate bucket plus the two concurrency caps.
type connGate struct {
	rl *rateLimiter

	mu       sync.Mutex
	perID    map[string]int
	total    int
	maxPerID int
	maxTotal int
}

func newConnGate() *connGate {
	return &connGate{
		rl:       newRateLimiterWith(HandshakeBurstPerIdentity, HandshakeRefillPerIdentity),
		perID:    map[string]int{},
		maxPerID: MaxConnsPerIdentity,
		maxTotal: MaxConnsTotal,
	}
}

// identityKey is the bucket key for an authenticated peer. Distinct devices get
// distinct keys, which is the entire point of the fix.
func identityKey(who Identity, remoteIP string) string {
	switch {
	case who.DeviceID != "":
		return "dev:" + who.DeviceID
	case who.Kind == "server":
		return "server"
	case who.Kind == "local":
		// Loopback listener bypass (SPEC F1). The peer address IS the real peer
		// here — a local-mode request that crossed a proxy has already been
		// refused the bypass — so keying on it is sound.
		return "local:" + remoteIP
	default:
		// Unauthenticated / unknown: fall back to the address, tightly.
		return "ip:" + remoteIP
	}
}

// acquire reserves one connection slot. The returned release MUST be called
// when the connection ends; it is a no-op when ok is false.
//
// reason is "" on success, and otherwise names which limit refused, so the log
// line says why a client was turned away instead of a bare 429.
func (g *connGate) acquire(key string) (release func(), ok bool, reason string) {
	if !g.rl.allow("ws:" + key) {
		return func() {}, false, "handshake rate"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.total >= g.maxTotal {
		return func() {}, false, "server connection cap"
	}
	if g.perID[key] >= g.maxPerID {
		return func() {}, false, "per-identity connection cap"
	}
	g.perID[key]++
	g.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if n := g.perID[key] - 1; n > 0 {
				g.perID[key] = n
			} else {
				delete(g.perID, key)
			}
			if g.total > 0 {
				g.total--
			}
		})
	}, true, ""
}

// counts reports the live totals (per-key, global). Test and log surface.
func (g *connGate) counts(key string) (perKey, total int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.perID[key], g.total
}
