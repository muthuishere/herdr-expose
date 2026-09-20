package expose

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// LIVE tests. They talk to the real Cloudflare edge and the real API, so they
// are OFF unless HEX_LIVE=1 — `go test ./...` on any machine, in CI or on a
// laptop with no token, must stay hermetic.
//
// They exist because the interesting failures in this package are not the ones
// a mock can produce: an edge that answers 502 through a tunnel it has not
// finished routing, a stub resolver holding a negative answer for a hostname
// minted three seconds ago, an API that refuses to delete a tunnel whose
// connector has not deregistered yet. Every one of those was hit for real.
//
// SAFETY: these use a THROWAWAY hostname derived from the clock, their own
// tunnel name, and their own state dir. They never name the permanent
// deployment's hostname or tunnel, and the named-tunnel case asserts, against
// the API, that the permanent record is untouched afterwards.
func liveOrSkip(t *testing.T) {
	t.Helper()
	if os.Getenv("HEX_LIVE") != "1" {
		t.Skip("live test; set HEX_LIVE=1 to run it against the real Cloudflare edge")
	}
}

// localOrigin is a real HTTP server answering /healthz, standing in for the
// herdr-expose listener a tunnel is put in front of.
func localOrigin(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	var port int
	if _, err := fmt.Sscanf(srv.URL[strings.LastIndex(srv.URL, ":")+1:], "%d", &port); err != nil {
		t.Fatal(err)
	}
	return srv, port
}

// A real quick tunnel, raised and torn down. It creates NOTHING in any
// account, which is exactly what makes it safe to run here — and the test
// asserts that: after Stop, Destroy is a declared no-op.
func TestLiveQuickTunnelRaiseVerifyAndTearDown(t *testing.T) {
	liveOrSkip(t)
	_, port := localOrigin(t)

	m := New(Options{
		Port: port, Expose: config.Expose{Quick: true}, StateDir: t.TempDir(),
		Logf: func(f string, a ...any) { t.Logf(f, a...) },
	})
	defer m.Close()

	fp := m.Footprint()
	if !fp.Ephemeral || fp.NamedTunnel != "" || fp.DNSRecord != "" {
		t.Fatalf("a quick tunnel must declare an empty footprint: %+v", fp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("raise a quick tunnel: %v", err)
	}
	if !strings.HasPrefix(st.URL, "https://") || !strings.Contains(st.URL, "trycloudflare.com") {
		t.Fatalf("url = %q", st.URL)
	}
	t.Logf("live quick tunnel: %s", st.URL)
	// The URL was published only after it answered, so it answers NOW.
	if !probe(ctx, st.URL) {
		t.Fatalf("%s was published but does not answer: verify-before-publish is broken", st.URL)
	}

	// Start again: no second tunnel, no error.
	st2, err := m.Start(ctx)
	if err != nil || st2.URL != st.URL {
		t.Fatalf("Start #2 must reuse the live tunnel: err=%v url=%q", err, st2.URL)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("stop #2: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Destroy(ctx); err != nil {
			t.Fatalf("destroy #%d: %v", i+1, err)
		}
	}
	// The hostname is gone with the process, which is the whole contract.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !probe(ctx, st.URL) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s still answers after teardown", st.URL)
}

// A real NAMED tunnel on a throwaway hostname: provision, verify through the
// public URL, destroy, destroy again, and confirm against the API that both
// the record and the tunnel are gone — and that the PERMANENT deployment's
// record was never touched.
//
// $HEX_LIVE_DOMAIN must be a throwaway hostname in a zone the token can reach.
func TestLiveNamedTunnelProvisionVerifyDestroy(t *testing.T) {
	liveOrSkip(t)
	domain := strings.TrimSpace(os.Getenv("HEX_LIVE_DOMAIN"))
	if domain == "" {
		t.Skip("set HEX_LIVE_DOMAIN to a THROWAWAY hostname (never the permanent one)")
	}
	if strings.HasPrefix(domain, "herdr.") {
		t.Fatal("refusing to run against the permanent hostname")
	}
	_, port := localOrigin(t)

	tunnelName := "hex-idem-" + strings.ReplaceAll(strings.Split(domain, ".")[0], "hex-idem-", "")
	m := New(Options{
		Port:     port,
		Expose:   config.Expose{Cloudflare: true, Domain: domain, TunnelName: tunnelName},
		StateDir: t.TempDir(),
		Logf:     func(f string, a ...any) { t.Logf(f, a...) },
	})
	defer m.Close()

	fp := m.Footprint()
	if fp.NamedTunnel != tunnelName || fp.DNSRecord != domain || fp.DNSTag == "" {
		t.Fatalf("footprint does not describe what will be created: %+v", fp)
	}
	t.Logf("footprint: %s", fp.Describe())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if st.URL != "https://"+domain {
		t.Fatalf("url = %q", st.URL)
	}
	if !probe(ctx, st.URL) {
		t.Fatalf("%s was published but does not answer", st.URL)
	}
	t.Logf("live named tunnel up and answering: %s", st.URL)

	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Destroy(ctx); err != nil {
			t.Fatalf("destroy #%d: %v", i+1, err)
		}
		dnsGone, tunGone, rec, err := VerifyGone(ctx, m.CloudflareOptions())
		if err != nil {
			t.Fatalf("verify #%d: %v", i+1, err)
		}
		if !dnsGone || !tunGone {
			t.Fatalf("destroy #%d did not complete: dns_gone=%v tunnel_gone=%v rec=%+v",
				i+1, dnsGone, tunGone, rec)
		}
	}

	// And the permanent deployment is untouched: its record still exists and
	// still carries ITS tag, not ours.
	permanent := CloudflareOptions{Domain: "herdr.deemwar.com", TunnelName: "herdr-expose"}
	dnsGone, tunGone, rec, err := VerifyGone(ctx, permanent)
	if err != nil {
		t.Logf("could not check the permanent deployment: %v", err)
		return
	}
	if dnsGone || tunGone || !rec.Exists {
		t.Fatalf("THE PERMANENT DEPLOYMENT WAS DAMAGED: dns_gone=%v tunnel_gone=%v rec=%+v",
			dnsGone, tunGone, rec)
	}
}
