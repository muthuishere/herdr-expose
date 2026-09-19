package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/core"
)

// ---------------------------------------------------------------- helpers

type testConfig struct {
	port    int
	bind    string
	mode    string
	origins []string
}

func (c *testConfig) Port() int                { return c.port }
func (c *testConfig) Bind() string             { return c.bind }
func (c *testConfig) Mode() string             { return c.mode }
func (c *testConfig) AllowedOrigins() []string { return c.origins }
func (c *testConfig) UI() map[string]any       { return map[string]any{} }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	log := testLogger()
	auth, err := NewAuth(t.TempDir(), log, nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	s, err := New(Options{
		Version: "test",
		Config:  &testConfig{port: 21118, bind: "127.0.0.1", mode: "cloudflare", origins: []string{"https://herdr.example"}},
		Hub:     core.NewHub(core.NewStore(log), log),
		Auth:    auth,
		Log:     log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// pairDevice mints a code and redeems it, returning the device token.
func pairDevice(t *testing.T, a *Auth, name string) string {
	t.Helper()
	code, _, err := a.NewPairingCode(name)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	tok, _, err := a.RedeemPairing(code, name, "127.0.0.1", "test-ua")
	if err != nil {
		t.Fatalf("RedeemPairing: %v", err)
	}
	return tok
}

// ------------------------------------------------- FIX 1: handshake budget

// TestHandshakeStormBeforeAfter replays the 90-second, 20-client reconnect
// storm that the stress test ran against the public tunnel, against the REAL
// limiter with a virtual clock.
//
// Behind cloudflared every client arrives as 127.0.0.1, so the OLD policy — one
// token bucket keyed on the remote address, burst 10, refill 0.5/s — is a
// single bucket for the whole public endpoint. The NEW policy keys on the
// identity the token proved.
func TestHandshakeStormBeforeAfter(t *testing.T) {
	const (
		clients      = 20
		storm        = 90 * time.Second
		retryEvery   = 200 * time.Millisecond
		proxiedAddr  = "127.0.0.1" // what cloudflared shows us for EVERY client
		attemptsEach = int(storm / retryEvery)
	)

	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	var clock time.Time
	now := func() time.Time { return clock }

	// --- BEFORE: one IP-keyed bucket, exactly the old AllowHandshake.
	old := newRateLimiter() // burst 10, refill 0.5/s
	old.now = now
	oldOK, oldRefused := 0, 0

	// --- AFTER: per-identity budget + concurrency caps.
	gate := newConnGate()
	gate.rl.now = now
	newOK, newRefused := 0, 0
	perDevice := make([]int, clients)

	for step := 0; step < attemptsEach; step++ {
		clock = base.Add(time.Duration(step) * retryEvery)
		for c := 0; c < clients; c++ {
			if old.allow("ws:" + proxiedAddr) {
				oldOK++
			} else {
				oldRefused++
			}

			who := Identity{Kind: "device", DeviceID: fmt.Sprintf("dev%02d", c)}
			release, ok, _ := gate.acquire(identityKey(who, proxiedAddr))
			if ok {
				newOK++
				perDevice[c]++
				release() // a reconnect storm drops the socket immediately
			} else {
				newRefused++
			}
		}
	}

	worst := perDevice[0]
	for _, n := range perDevice {
		if n < worst {
			worst = n
		}
	}
	t.Logf("BEFORE (IP-keyed, shared bucket): %d connected, %d refused (429) — %.2f per device over 90s",
		oldOK, oldRefused, float64(oldOK)/float64(clients))
	t.Logf("AFTER  (identity-keyed):          %d connected, %d refused (429) — worst device got %d",
		newOK, newRefused, worst)

	if oldOK > 60 {
		t.Fatalf("old policy should be crippled (~55 total), got %d", oldOK)
	}
	if newOK < 2000 {
		t.Fatalf("new policy too tight: %d successful connections", newOK)
	}
	if worst < 100 {
		t.Fatalf("worst-served device only got %d connections, expected >=100", worst)
	}
}

// TestFlappingDeviceDoesNotStarveAnother is the user-visible symptom: a phone on
// a bad link burning its whole budget must not stop the laptop connecting.
func TestFlappingDeviceDoesNotStarveAnother(t *testing.T) {
	gate := newConnGate()
	const proxied = "127.0.0.1"

	phone := identityKey(Identity{Kind: "device", DeviceID: "phone"}, proxied)
	laptop := identityKey(Identity{Kind: "device", DeviceID: "laptop"}, proxied)

	flaps, refused := 0, 0
	for i := 0; i < 500; i++ {
		release, ok, _ := gate.acquire(phone)
		if ok {
			flaps++
			release()
		} else {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("phone was never throttled; its own budget is not bounded")
	}

	// The laptop, arriving on the same apparent address, is untouched.
	for i := 0; i < HandshakeBurstPerIdentity; i++ {
		release, ok, why := gate.acquire(laptop)
		if !ok {
			t.Fatalf("laptop refused after %d connections (%s) — starved by the phone", i, why)
		}
		release()
	}
	t.Logf("phone: %d connected / %d refused; laptop: %d consecutive connections unaffected",
		flaps, refused, HandshakeBurstPerIdentity)
}

// TestConnGateConcurrencyCaps proves the backstop an authenticated attacker
// would otherwise walk past: a rate is not a cap on sockets held open.
func TestConnGateConcurrencyCaps(t *testing.T) {
	gate := newConnGate()
	// A rate bucket that never denies, so only the caps are under test.
	gate.rl = newRateLimiterWith(1e9, 1e9)

	key := identityKey(Identity{Kind: "device", DeviceID: "greedy"}, "127.0.0.1")
	var held []func()
	for i := 0; i < MaxConnsPerIdentity; i++ {
		release, ok, why := gate.acquire(key)
		if !ok {
			t.Fatalf("refused at %d below the per-identity cap: %s", i, why)
		}
		held = append(held, release)
	}
	if _, ok, why := gate.acquire(key); ok || why != "per-identity connection cap" {
		t.Fatalf("per-identity cap not enforced (ok=%v why=%q)", ok, why)
	}
	// Another device is unaffected by the first one's cap.
	other := identityKey(Identity{Kind: "device", DeviceID: "other"}, "127.0.0.1")
	release, ok, why := gate.acquire(other)
	if !ok {
		t.Fatalf("second device refused: %s", why)
	}
	release()

	// Releasing frees the slot, and double-release cannot underflow.
	held[0]()
	held[0]()
	if per, _ := gate.counts(key); per != MaxConnsPerIdentity-1 {
		t.Fatalf("release accounting wrong: %d", per)
	}
	for _, r := range held[1:] {
		r()
	}
	held[0]()
	if per, total := gate.counts(key); per != 0 || total != 0 {
		t.Fatalf("slots leaked: per=%d total=%d", per, total)
	}

	// Global backstop.
	var all []func()
	for i := 0; i < MaxConnsTotal; i++ {
		k := identityKey(Identity{Kind: "device", DeviceID: fmt.Sprintf("d%03d", i)}, "127.0.0.1")
		release, ok, why := gate.acquire(k)
		if !ok {
			t.Fatalf("refused at %d below the global cap: %s", i, why)
		}
		all = append(all, release)
	}
	k := identityKey(Identity{Kind: "device", DeviceID: "one-too-many"}, "127.0.0.1")
	if _, ok, why := gate.acquire(k); ok || why != "server connection cap" {
		t.Fatalf("global cap not enforced (ok=%v why=%q)", ok, why)
	}
	for _, r := range all {
		r()
	}
}

func TestConnGateConcurrentAcquireRelease(t *testing.T) {
	gate := newConnGate()
	gate.rl = newRateLimiterWith(1e9, 1e9)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := identityKey(Identity{Kind: "device", DeviceID: fmt.Sprintf("d%d", i%4)}, "127.0.0.1")
			for j := 0; j < 200; j++ {
				if release, ok, _ := gate.acquire(key); ok {
					release()
				}
			}
		}(i)
	}
	wg.Wait()
	if _, total := gate.counts("nobody"); total != 0 {
		t.Fatalf("connections leaked under concurrency: %d", total)
	}
}

// ------------------------------------------------- FIX 1b: bucket eviction

func TestRateLimiterEvictsIdleBuckets(t *testing.T) {
	l := newRateLimiter()
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	clock := base
	l.now = func() time.Time { return clock }

	for i := 0; i < 500; i++ {
		l.allow(fmt.Sprintf("auth:10.0.%d.%d", i/256, i%256))
	}
	if n := l.size(); n != 500 {
		t.Fatalf("expected 500 live buckets, got %d", n)
	}

	// One address keeps using it; the rest go idle past the TTL.
	clock = base.Add(l.idleTTL + time.Minute)
	l.allow("auth:10.0.0.0")
	if n := l.size(); n != 1 {
		t.Fatalf("idle buckets were not evicted: %d still tracked", n)
	}

	// An evicted bucket comes back full, which is what it would have refilled
	// to anyway — eviction hands nobody capacity they had not earned.
	for i := 0; i < rlBurst; i++ {
		if !l.allow("auth:10.0.9.9") {
			t.Fatalf("fresh bucket denied at attempt %d", i)
		}
	}
	if l.allow("auth:10.0.9.9") {
		t.Fatal("bucket did not run out after its burst")
	}
}

func TestRateLimiterBoundsKeyCount(t *testing.T) {
	l := newRateLimiter()
	clock := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { clock = clock.Add(time.Millisecond); return clock }

	// A flood of distinct addresses inside one GC interval: the map must still
	// be bounded, or an attacker allocates memory for the life of the process.
	for i := 0; i < rlMaxKeys*2; i++ {
		l.allow(fmt.Sprintf("auth:key-%d", i))
	}
	if n := l.size(); n > rlMaxKeys {
		t.Fatalf("bucket map grew past its ceiling: %d > %d", n, rlMaxKeys)
	}
	t.Logf("%d distinct keys seen, %d buckets retained", rlMaxKeys*2, l.size())
}

// TestAuthFailuresDoNotStarveAValidToken: behind a proxy every client shares one
// address, so a stranger's failures must never lock out a paired device.
func TestAuthFailuresDoNotStarveAValidToken(t *testing.T) {
	log := testLogger()
	a, err := NewAuth(t.TempDir(), log, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := pairDevice(t, a, "laptop")

	for i := 0; i < 200; i++ {
		if _, err := a.Authenticate("deadbeef", "127.0.0.1", "attacker"); err == nil {
			t.Fatal("a bogus token authenticated")
		}
	}
	if _, err := a.Authenticate(tok, "127.0.0.1", "laptop"); err != nil {
		t.Fatalf("valid device token refused after another client's failures: %v", err)
	}
}

// ------------------------------------------------------- FIX 2: /healthz

func TestHealthzUnauthenticatedLeaksNothing(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://herdr.example/healthz", nil)
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay a 200 liveness probe, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body: %v", err)
	}
	// The probe contract internal/expose relies on.
	if body["ok"] != true {
		t.Fatalf(`"ok":true missing: %s`, rec.Body.String())
	}
	for k := range body {
		if k != "ok" && k != "api" {
			t.Errorf("unauthenticated /healthz leaks %q = %v", k, body[k])
		}
	}
	for _, leaked := range []string{"sessions", "sessions_connected", "herdr_version",
		"herdr_protocol", "tree_rev", "web_ui", "upstream", "version"} {
		if _, ok := body[leaked]; ok {
			t.Errorf("%q must not be visible to an unauthenticated caller", leaked)
		}
	}
}

func TestHealthzAuthenticatedKeepsDetail(t *testing.T) {
	s := newTestServer(t)
	tok := pairDevice(t, s.auth, "laptop")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://herdr.example/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	s.routes().ServeHTTP(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("ok semantics changed for an authenticated caller: %s", rec.Body.String())
	}
	for _, want := range []string{"sessions", "sessions_connected", "tree_rev", "version", "web_ui"} {
		if _, ok := body[want]; !ok {
			t.Errorf("authenticated /healthz lost %q", want)
		}
	}
}

func TestHealthzLoopbackKeepsDetail(t *testing.T) {
	s := newTestServer(t)
	// What `status` and `doctor` do: straight at the loopback listener.
	s.local = localMode{
		enabled: true,
		port:    "21118",
		hosts:   []string{"127.0.0.1:21118", "localhost:21118"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:21118/healthz", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	s.routes().ServeHTTP(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body: %v", err)
	}
	if _, ok := body["version"]; !ok {
		t.Fatalf("loopback caller lost the detail doctor parses: %s", rec.Body.String())
	}

	// The same server reached THROUGH the tunnel (tunnel Host + CDN headers)
	// gets the minimal body, because the bypass is derived from the listener.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://herdr.example/healthz", nil)
	req.RemoteAddr = "127.0.0.1:54322" // cloudflared, on loopback
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	s.routes().ServeHTTP(rec, req)
	body = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["version"]; ok {
		t.Fatalf("tunnelled request got operational detail: %s", rec.Body.String())
	}
	if body["ok"] != true {
		t.Fatalf("tunnelled probe lost ok: %s", rec.Body.String())
	}
}

// --------------------------------------------------- FIX 3: pair expiry

func decodePair(t *testing.T, s *Server, code string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://herdr.example/v1/pair",
		strings.NewReader(`{"code":"`+code+`","name":"phone"}`))
	s.handlePair(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pair failed: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("pair body: %v", err)
	}
	return body
}

func expiryFields(t *testing.T, body map[string]any) (time.Time, float64) {
	t.Helper()
	at, ok := body["expires_at"].(string)
	if !ok {
		t.Fatalf("no expires_at in %v", body)
	}
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("expires_at %q: %v", at, err)
	}
	in, ok := body["expires_in"].(float64)
	if !ok {
		t.Fatalf("no expires_in in %v", body)
	}
	return ts, in
}

func TestPairExpiresInAgreesWithExpiresAt(t *testing.T) {
	s := newTestServer(t)
	code, _, err := s.auth.NewPairingCode("phone")
	if err != nil {
		t.Fatal(err)
	}
	body := decodePair(t, s, code)
	at, in := expiryFields(t, body)

	want := time.Until(at).Seconds()
	if diff := want - in; diff > 2 || diff < -2 {
		t.Fatalf("expires_in %v disagrees with expires_at %v (delta %.0fs)", in, at, diff)
	}
	if in < DeviceTTL.Seconds()-120 {
		t.Fatalf("a normal server should hand out the full device TTL, got %v", in)
	}
	// The device projection must not contradict the top-level pair either.
	dev, _ := body["device"].(map[string]any)
	if dev == nil {
		t.Fatal("no device in pair response")
	}
}

func TestPairExpiresInFollowsShareDeadline(t *testing.T) {
	s := newTestServer(t)
	deadline := time.Now().Add(1 * time.Hour)
	if err := s.auth.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	code, _, err := s.auth.NewPairingCode("phone")
	if err != nil {
		t.Fatal(err)
	}
	body := decodePair(t, s, code)
	at, in := expiryFields(t, body)

	if d := at.Sub(deadline); d > 2*time.Second || d < -2*time.Second {
		t.Fatalf("expires_at %v is not the share deadline %v", at, deadline)
	}
	if in > 3605 || in < 3500 {
		t.Fatalf("expires_in %v does not match a 1-hour share (expires_at %v)", in, at)
	}
	// The exact bug: a month-long expires_in next to an hour-long expires_at.
	if in >= DeviceTTL.Seconds() {
		t.Fatalf("expires_in still reports the device TTL (%v) on a 1-hour share", in)
	}

	// And the device projection agrees with both.
	dev := body["device"].(map[string]any)
	devAt, err := time.Parse(time.RFC3339, dev["expires_at"].(string))
	if err != nil {
		t.Fatalf("device.expires_at: %v", err)
	}
	if d := devAt.Sub(at); d > time.Second || d < -time.Second {
		t.Fatalf("device.expires_at %v != expires_at %v", devAt, at)
	}
}

// --------------------------------------------------- FIX 4: write_us

func TestMetricsPublishesWriteUs(t *testing.T) {
	s := newTestServer(t)
	tok := pairDevice(t, s.auth, "laptop")
	s.hub.Metrics().Write.Observe(3 * time.Millisecond)
	s.hub.Metrics().Output.Observe(4 * time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://herdr.example/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	s.handleMetrics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	w, ok := body["write_us"].(map[string]any)
	if !ok {
		t.Fatalf("write_us missing from /v1/metrics: %s", rec.Body.String())
	}
	out, _ := body["output_us"].(map[string]any)
	for k := range out {
		if _, ok := w[k]; !ok {
			t.Errorf("write_us has a different shape from output_us: missing %q", k)
		}
	}
	if w["p50"] == nil || w["count"] == nil {
		t.Fatalf("write_us is not a populated histogram: %v", w)
	}
}

func TestMetricsStillRequiresAuth(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://herdr.example/v1/metrics", nil)
	s.handleMetrics(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("metrics must stay authenticated, got %d", rec.Code)
	}
}
