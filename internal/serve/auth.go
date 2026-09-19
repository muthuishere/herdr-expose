// Package serve is the HTTP + WebSocket surface: the split-plane protocol,
// bearer/device auth, pairing, and the static app handler.
package serve

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Auth is the whole authentication surface.
//
// This binary can run `pane.send_text` and `agent.prompt`. Behind a tunnel it
// is a remote code execution endpoint, so: two separate secrets, hashes only on
// disk, constant-time comparison everywhere, and rate limits on every path that
// accepts a guess.
type Auth struct {
	statePath string
	log       Logger

	mu    sync.Mutex
	state authState

	limiter *rateLimiter

	origins map[string]struct{}

	// deadline, when non-zero, is a HARD ceiling on every credential this
	// instance issues or accepts (SPEC AMENDMENTS 9, G5.2). A share sets it to
	// the share's expiry, so a device token cannot outlive the share even if
	// teardown fails and the tunnel somehow survives. It is belt to the
	// in-process timer's braces, and it is enforced on every authentication,
	// not only at issue time.
	deadline time.Time
}

type authState struct {
	// Pending pairing codes, stored as SHA-256 HASHES with an expiry. They are
	// persisted (not held in memory) for one reason: `herdr-expose pair` runs
	// in a DIFFERENT process from the server, and a code the server cannot see
	// is a code that can never be redeemed.
	Pairing []*pairingRecord `json:"pairing,omitempty"`

	// ServerTokenSHA is the hex SHA-256 of the server token. The plaintext is
	// shown once, at generation time, and never stored.
	ServerTokenSHA string          `json:"server_token_sha256"`
	Devices        []*DeviceRecord `json:"devices"`
}

// DeviceRecord is one paired client. The hash is never exported to any API.
type DeviceRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenSHA  string    `json:"token_sha256"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	LastIP    string    `json:"last_ip"`
	UserAgent string    `json:"user_agent"`
	// ExpiresAt is an absolute expiry that overrides the sliding DeviceTTL
	// when it is sooner. Zero means "sliding TTL only" (the normal server).
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Public is the safe projection of a device, for status output and /v1/config.
type Public struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	LastIP    string    `json:"last_ip"`
	UserAgent string    `json:"user_agent"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// pairingRecord is one outstanding pairing code.
//
// SECURITY: a pairing code is minted ONLY by the local CLI and displayed ONLY
// on the physically-present machine (terminal QR / Herdr overlay pane). No HTTP
// route issues one. POST /v1/pair only REDEEMS. That is what makes exposing a
// remote-code-execution endpoint on a public domain defensible: reaching the
// public URL is not enough, you must be sitting at the laptop.
type pairingRecord struct {
	SHA     string    `json:"sha256"`
	Expires time.Time `json:"expires"`
	Name    string    `json:"name,omitempty"`
}

// Auth policy constants (SPEC amendment B5).
const (
	// PairingTTL is deliberately 10 minutes. 60 seconds is hostile on a phone:
	// unlock, open camera, scan, switch app, tap through a trust prompt.
	PairingTTL = 10 * time.Minute
	// DeviceTTL is a sliding window refreshed on every successful use.
	DeviceTTL = 30 * 24 * time.Hour
	// MaxDevices caps the paired set; the least recently seen is evicted.
	MaxDevices = 32
	// DeviceTokenBytes is the entropy of a device token.
	DeviceTokenBytes = 32
	// ServerTokenBytes is the entropy of the server token.
	ServerTokenBytes = 32
)

// Logger is the minimal logging surface serve needs.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
}

// NewAuth loads or creates auth state under stateDir (0600).
func NewAuth(stateDir string, log Logger, allowedOrigins []string) (*Auth, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	a := &Auth{
		statePath: filepath.Join(stateDir, "auth.json"),
		log:       log,
		limiter:   newRateLimiter(),
		origins:   map[string]struct{}{},
	}
	for _, o := range allowedOrigins {
		a.origins[strings.ToLower(strings.TrimSpace(o))] = struct{}{}
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Auth) load() error {
	b, err := os.ReadFile(a.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &a.state)
}

// save writes state atomically at 0600. Callers hold a.mu.
func (a *Auth) save() error {
	b, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.statePath)
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("serve: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// EnsureServerToken returns the existing token's presence, minting a new one if
// there is none. The plaintext is returned ONLY when freshly minted — it is
// never recoverable afterwards, because only the hash is stored.
func (a *Auth) EnsureServerToken() (plaintext string, created bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.ServerTokenSHA != "" {
		return "", false, nil
	}
	tok := randToken(ServerTokenBytes)
	a.state.ServerTokenSHA = sha256hex(tok)
	if err := a.save(); err != nil {
		return "", false, err
	}
	return tok, true, nil
}

// ResetServerToken mints a fresh server token, invalidating the old one.
func (a *Auth) ResetServerToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	tok := randToken(ServerTokenBytes)
	a.state.ServerTokenSHA = sha256hex(tok)
	if err := a.save(); err != nil {
		return "", err
	}
	return tok, nil
}

// SetDeadline pins an absolute expiry on every credential this Auth issues or
// accepts, and clamps the ones already on disk to it. Used by `share`: a share
// that is gone must leave nothing behind that still opens the door.
//
// It only ever moves an existing device expiry when the deadline moves —
// `share extend` pushes tokens out with the share (otherwise they would expire
// underneath a share that is still running), and a shortened deadline pulls
// them all in.
func (a *Auth) SetDeadline(t time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deadline = t
	if t.IsZero() {
		return nil
	}
	changed := false
	for _, d := range a.state.Devices {
		if !d.ExpiresAt.Equal(t) {
			d.ExpiresAt = t
			changed = true
		}
	}
	for _, p := range a.state.Pairing {
		if p.Expires.After(t) {
			p.Expires = t
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return a.save()
}

// Deadline is the absolute credential ceiling, zero when there is none.
func (a *Auth) Deadline() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deadline
}

// Identity is who a request turned out to be.
type Identity struct {
	Kind     string // "server" or "device"
	DeviceID string
	Name     string
}

// ErrUnauthorized is the single failure any auth path reports. It never
// distinguishes "no such device" from "wrong token".
var ErrUnauthorized = errors.New("unauthorized")

// Authenticate validates a bearer token against the server token and every
// device token, in constant time, and refreshes the device's sliding TTL.
func (a *Auth) Authenticate(token, remoteIP, userAgent string) (Identity, error) {
	// Every rejection is logged with its REASON and never with the token: an
	// after-the-fact "why could my phone not connect" is answerable only if the
	// reason was recorded, and "unauthorized" on its own answers nothing.
	if token == "" {
		a.reject(remoteIP, "no token presented")
		return Identity{}, ErrUnauthorized
	}
	sum := sha256hex(token)

	a.mu.Lock()
	defer a.mu.Unlock()

	// Past the instance deadline NOTHING authenticates, including the server
	// token. A share whose process is still up but whose time is over is shut,
	// not merely scheduled to be shut.
	if !a.deadline.IsZero() && time.Now().After(a.deadline) {
		a.reject(remoteIP, "instance deadline passed")
		return Identity{}, ErrUnauthorized
	}

	matched := false
	if a.state.ServerTokenSHA != "" &&
		subtle.ConstantTimeCompare([]byte(sum), []byte(a.state.ServerTokenSHA)) == 1 {
		matched = true
	}
	if matched {
		return Identity{Kind: "server", Name: "server-token"}, nil
	}

	now := time.Now()
	var found *DeviceRecord
	for _, d := range a.state.Devices {
		if subtle.ConstantTimeCompare([]byte(sum), []byte(d.TokenSHA)) == 1 {
			found = d
		}
	}
	if found == nil {
		a.reject(remoteIP, "no device matches that token")
		return Identity{}, ErrUnauthorized
	}
	if !found.ExpiresAt.IsZero() && now.After(found.ExpiresAt) {
		a.log.Info("auth: device token expired, revoking", "device", found.ID, "ip", remoteIP)
		a.removeDeviceLocked(found.ID)
		_ = a.save()
		return Identity{}, ErrUnauthorized
	}
	if now.Sub(found.LastSeen) > DeviceTTL {
		a.log.Info("auth: device idle past its TTL, revoking", "device", found.ID, "ip", remoteIP)
		a.removeDeviceLocked(found.ID)
		_ = a.save()
		return Identity{}, ErrUnauthorized
	}
	found.LastSeen = now
	found.LastIP = remoteIP
	if userAgent != "" {
		found.UserAgent = userAgent
	}
	_ = a.save()
	a.limiter.reset("authfail:" + remoteIP)
	return Identity{Kind: "device", DeviceID: found.ID, Name: found.Name}, nil
}

var codeAlphabet = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NewPairingCode mints a single-use 6-character pairing code.
//
// Callable only from the local CLI. There is deliberately no HTTP route that
// reaches this.
func (a *Auth) NewPairingCode(name string) (string, time.Time, error) {
	raw := make([]byte, 5)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	code := codeAlphabet.EncodeToString(raw)[:6]
	exp := time.Now().Add(PairingTTL)

	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.deadline.IsZero() && exp.After(a.deadline) {
		exp = a.deadline
	}
	a.reloadLocked()
	a.gcPairingLocked()
	a.state.Pairing = append(a.state.Pairing,
		&pairingRecord{SHA: sha256hex(code), Expires: exp, Name: name})
	if err := a.save(); err != nil {
		return "", time.Time{}, err
	}
	// The CODE itself is never logged — only that one was issued, for whom and
	// until when. It is displayed on this machine's screen and nowhere else.
	a.log.Info("pairing code issued", "name", name, "expires_at", exp)
	return code, exp, nil
}

func (a *Auth) gcPairingLocked() {
	now := time.Now()
	kept := a.state.Pairing[:0]
	for _, p := range a.state.Pairing {
		if now.Before(p.Expires) {
			kept = append(kept, p)
		}
	}
	a.state.Pairing = kept
}

// reloadLocked re-reads state from disk. The CLI and the server are separate
// processes writing the same file, so a redeem must see a code minted a second
// ago by the other one.
func (a *Auth) reloadLocked() {
	b, err := os.ReadFile(a.statePath)
	if err != nil {
		return
	}
	var fresh authState
	if json.Unmarshal(b, &fresh) != nil {
		return
	}
	a.state.Pairing = fresh.Pairing
	if len(fresh.Devices) > len(a.state.Devices) {
		a.state.Devices = fresh.Devices
	}
	if fresh.ServerTokenSHA != "" {
		a.state.ServerTokenSHA = fresh.ServerTokenSHA
	}
}

// RedeemPairing exchanges a one-time code for a long-lived device token.
// The plaintext token is returned once; only its hash is stored.
func (a *Auth) RedeemPairing(code, name, remoteIP, userAgent string) (token string, dev Public, err error) {
	if !a.limiter.allow("pair:" + remoteIP) {
		a.log.Warn("pairing rejected", "reason", "rate limited", "ip", remoteIP)
		return "", Public{}, ErrUnauthorized
	}
	sum := sha256hex(strings.ToUpper(strings.TrimSpace(code)))

	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadLocked()
	a.gcPairingLocked()

	idx := -1
	for i, p := range a.state.Pairing {
		if subtle.ConstantTimeCompare([]byte(sum), []byte(p.SHA)) == 1 {
			idx = i
		}
	}
	if idx < 0 {
		a.log.Warn("pairing rejected", "reason", "no such code", "ip", remoteIP)
		return "", Public{}, ErrUnauthorized
	}
	hit := a.state.Pairing[idx]
	// Single use: consumed whether or not the rest succeeds.
	a.state.Pairing = append(a.state.Pairing[:idx], a.state.Pairing[idx+1:]...)
	if time.Now().After(hit.Expires) {
		a.log.Warn("pairing rejected", "reason", "code expired", "ip", remoteIP)
		_ = a.save()
		return "", Public{}, ErrUnauthorized
	}
	if name == "" {
		name = hit.Name
	}
	if name == "" {
		name = "device"
	}
	tok := randToken(DeviceTokenBytes)
	now := time.Now()
	rec := &DeviceRecord{
		ID:        randToken(8),
		Name:      name,
		TokenSHA:  sha256hex(tok),
		CreatedAt: now,
		LastSeen:  now,
		LastIP:    remoteIP,
		UserAgent: userAgent,
		ExpiresAt: a.deadline,
	}
	a.state.Devices = append(a.state.Devices, rec)
	a.evictLocked()
	if err := a.save(); err != nil {
		return "", Public{}, err
	}
	a.limiter.reset("pair:" + remoteIP)
	a.log.Info("pairing redeemed", "device", rec.ID, "name", rec.Name, "ip", remoteIP,
		"expires_at", rec.ExpiresAt)
	return tok, publicOf(rec), nil
}

// reject records a failed authentication. The reason is the whole point; the
// token never appears, here or anywhere else.
//
// The per-address bucket here throttles the LOG, not the credential check: a
// valid device token is never refused because some other client burned the
// bucket. That matters because behind a proxy every remote client shares one
// address, so a pre-check bucket keyed on the address is a shared bucket that a
// stranger can empty on everyone's behalf — exactly the denial-of-service this
// limiter exists to prevent.
//
// Brute force is not what is being held back here, and it must not be confused
// with pairing: a device token is 256 bits, so guessing is not a threat model,
// whereas a 6-character pairing code is ~30 bits and its limiter in
// RedeemPairing IS load-bearing and stays a strict pre-check.
func (a *Auth) reject(remoteIP, reason string) {
	if !a.limiter.allow("authfail:" + remoteIP) {
		// Already warned plenty from this address; stay quiet rather than let
		// a flood write the log.
		a.log.Debug("auth rejected (log throttled)", "reason", reason, "ip", remoteIP)
		return
	}
	a.log.Warn("auth rejected", "reason", reason, "ip", remoteIP)
}

// evictLocked enforces MaxDevices with least-recently-seen eviction.
func (a *Auth) evictLocked() {
	if len(a.state.Devices) <= MaxDevices {
		return
	}
	sort.Slice(a.state.Devices, func(i, j int) bool {
		return a.state.Devices[i].LastSeen.After(a.state.Devices[j].LastSeen)
	})
	for _, d := range a.state.Devices[MaxDevices:] {
		a.log.Warn("auth: evicting least-recently-seen device", "device", d.Name, "id", d.ID)
	}
	a.state.Devices = a.state.Devices[:MaxDevices]
}

// deviceExpiry is the ONE instant at which a device credential dies, and every
// expiry a client is told must be derived from it.
//
// Two rules are in play and the sooner wins: the absolute instance deadline
// (a share's expiry, pinned into ExpiresAt), and otherwise the sliding
// DeviceTTL measured from last use — which, for a token just issued or just
// used, is LastSeen + DeviceTTL.
func deviceExpiry(d Public) time.Time {
	sliding := d.LastSeen.Add(DeviceTTL)
	if d.LastSeen.IsZero() {
		sliding = time.Now().Add(DeviceTTL)
	}
	if d.ExpiresAt.IsZero() {
		return sliding
	}
	if d.ExpiresAt.Before(sliding) {
		return d.ExpiresAt
	}
	return sliding
}

func publicOf(d *DeviceRecord) Public {
	return Public{ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt,
		LastSeen: d.LastSeen, LastIP: d.LastIP, UserAgent: d.UserAgent,
		ExpiresAt: d.ExpiresAt}
}

// Devices lists paired devices, hashes excluded.
func (a *Auth) Devices() []Public {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Public, 0, len(a.state.Devices))
	for _, d := range a.state.Devices {
		out = append(out, publicOf(d))
	}
	return out
}

// RevokeDevice removes one device by id.
func (a *Auth) RevokeDevice(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.removeDeviceLocked(id) {
		return fmt.Errorf("serve: no device %q", id)
	}
	a.log.Info("device revoked", "device", id)
	return a.save()
}

func (a *Auth) removeDeviceLocked(id string) bool {
	for i, d := range a.state.Devices {
		if d.ID == id {
			a.state.Devices = append(a.state.Devices[:i], a.state.Devices[i+1:]...)
			return true
		}
	}
	return false
}

// OriginAllowed checks an Origin header against the allowlist BEFORE anything
// echoes it back into a CORS header.
func (a *Auth) OriginAllowed(origin string) bool {
	if origin == "" {
		return true // same-origin / non-browser client
	}
	o := strings.ToLower(origin)
	if _, ok := a.origins[o]; ok {
		return true
	}
	// Loopback is always same-machine.
	if strings.HasPrefix(o, "http://127.0.0.1:") ||
		strings.HasPrefix(o, "http://localhost:") ||
		strings.HasPrefix(o, "http://[::1]:") {
		return true
	}
	return false
}

// AllowHandshake is deliberately gone. WebSocket upgrades are gated by
// connGate (connlimit.go), keyed on the identity the token proved, because an
// address-keyed bucket is one shared bucket for every client behind a proxy.

// BearerFrom extracts a token from the Authorization header or ?token=.
func BearerFrom(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	// Browsers cannot set headers on a WebSocket handshake; the subprotocol is
	// the standard carrier.
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "bearer.") {
			return strings.TrimPrefix(p, "bearer.")
		}
	}
	return ""
}

// RemoteIP is the peer address without the port.
func RemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a token bucket keyed by an arbitrary string.
//
// The map is bounded: buckets idle long enough to have refilled to full are
// indistinguishable from buckets that do not exist, so they are evicted. Before
// this, `buckets` was a map keyed by remote IP that only ever grew — every
// address that ever touched the server kept a live entry for the lifetime of
// the process, which is an unbounded, attacker-driven allocation.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	burst   float64
	refill  float64 // tokens per second
	idleTTL time.Duration
	maxKeys int
	lastGC  time.Time
	// now is time.Now, replaceable so a 90-second reconnect storm can be
	// replayed against the REAL limiter in a test that finishes instantly.
	now func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	rlBurst  = 10
	rlRefill = 0.5 // tokens per second
	// rlGCInterval is how often allow() sweeps idle buckets. The sweep is O(n)
	// over a map that is normally tiny, and it is amortised across calls rather
	// than run by a goroutine, so the limiter still owns no background work.
	rlGCInterval = time.Minute
	// rlMaxKeys is a hard ceiling on tracked keys; past it the sweep runs
	// regardless of the interval and drops the least recently used.
	rlMaxKeys = 4096
)

func newRateLimiter() *rateLimiter { return newRateLimiterWith(rlBurst, rlRefill) }

func newRateLimiterWith(burst, refill float64) *rateLimiter {
	// A bucket that has been idle for longer than it takes to refill from empty
	// to full holds exactly `burst` tokens, which is what a brand-new bucket
	// holds. Evicting it therefore cannot hand anyone capacity they had not
	// already earned. The floor keeps the sweep cheap on a busy server.
	full := time.Duration(float64(time.Second) * burst / refill)
	ttl := 10 * time.Minute
	if full > ttl {
		ttl = full
	}
	return &rateLimiter{
		buckets: map[string]*bucket{},
		burst:   burst,
		refill:  refill,
		idleTTL: ttl,
		maxKeys: rlMaxKeys,
		now:     time.Now,
	}
}

func (l *rateLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) >= rlGCInterval || len(l.buckets) > l.maxKeys {
		l.gcLocked(now)
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.refill
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// gcLocked evicts buckets that have gone idle, and, if the map is still over
// its ceiling, the least recently used until it is not. Callers hold l.mu.
func (l *rateLimiter) gcLocked(now time.Time) {
	l.lastGC = now
	for k, b := range l.buckets {
		if now.Sub(b.last) >= l.idleTTL {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) <= l.maxKeys {
		return
	}
	// Still oversized: a flood of distinct keys inside one interval. Trim well
	// below the ceiling rather than to it, so the O(n log n) sweep runs once
	// per flood instead of once per insert.
	target := l.maxKeys * 3 / 4
	type kv struct {
		key  string
		last time.Time
	}
	all := make([]kv, 0, len(l.buckets))
	for k, b := range l.buckets {
		all = append(all, kv{k, b.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	for _, e := range all[:len(all)-target] {
		delete(l.buckets, e.key)
	}
}

// size is the number of live buckets.
func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// gc forces a sweep. Exposed for the daemon's idle path and for tests.
func (l *rateLimiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(l.now())
}

func (l *rateLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}
