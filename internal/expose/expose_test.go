package expose

import (
	"context"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TWO RUNGS AND NO MORE. This is the test that would fail first if a tunnel
// ever grew back: whatever the config says, the answer is loopback or the LAN,
// and the bind address is one of exactly two values.
func TestOnlyTwoRungsExist(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    config.Expose
		mode config.Mode
		bind string
	}{
		{"empty config is loopback", config.Expose{}, ModeLocal, config.BindLoopback},
		{"lan binds every interface", config.Expose{LAN: true}, ModeLAN, config.BindAll},
	} {
		got := Resolve(tc.e, 21118, func(string) bool { return true })
		if got.Mode != tc.mode || got.Bind != tc.bind {
			t.Fatalf("%s: mode=%s bind=%s, want %s/%s", tc.name, got.Mode, got.Bind, tc.mode, tc.bind)
		}
		if got.Rung() > config.RungLAN {
			t.Fatalf("%s: resolved above the LAN, and there is no rung above it", tc.name)
		}
	}
}

// The floor holds when the binary cannot find anything it might once have
// shelled out to. There is nothing to find now, so a stub that answers "no" to
// everything must change nothing at all.
func TestNothingIsShelledOutToAnyMore(t *testing.T) {
	none := Resolve(config.Expose{LAN: true}, 21118, func(string) bool { return false })
	all := Resolve(config.Expose{LAN: true}, 21118, func(string) bool { return true })
	if none != all {
		t.Fatalf("a missing binary changed the answer:\n none=%+v\n  all=%+v", none, all)
	}
	if none.FellBack != "" {
		t.Fatalf("reported a fallback (%q) with nothing to fall back from", none.FellBack)
	}
}

func TestURLAndOriginsPerMode(t *testing.T) {
	// lan: the LAN IP, which must also be an allowed origin or the browser
	// refuses the WebSocket.
	lan := Resolve(config.Expose{LAN: true}, 21118, nil)
	if ip := PrimaryLANIP(); ip != "" {
		want := "http://" + ip + ":21118"
		if lan.URL != want {
			t.Fatalf("lan url = %s, want %s", lan.URL, want)
		}
		if !contains(lan.AllowedOrigins(nil), want) {
			t.Fatalf("lan origin missing: %v", lan.AllowedOrigins(nil))
		}
	}
	if !contains(lan.AllowedOrigins([]string{"https://pinned.example.com"}), "https://pinned.example.com") {
		t.Fatal("configured origins must be preserved")
	}
	if !contains(lan.AllowedOrigins(nil), "http://127.0.0.1:21118") {
		t.Fatal("loopback origin must always be allowed")
	}

	loc := Resolve(config.Expose{}, 21118, nil)
	if loc.URL != "http://127.0.0.1:21118" {
		t.Fatalf("local url = %s", loc.URL)
	}
	if !loc.SecureContext {
		t.Fatal("localhost is a secure context by definition")
	}
	if lan.SecureContext {
		t.Fatal("plain HTTP on a LAN IP is not a secure context")
	}
}

// Start and Stop are kept for callers that bracket a run with them. With no
// process to supervise they must be no-ops that never fail — including when
// Stop is called with nothing running, which is every call now.
func TestStartAndStopAreHarmlessNoOps(t *testing.T) {
	m := New(Options{Port: 21118, Expose: config.Expose{}})
	defer m.Close()

	st, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.Mode != string(ModeLocal) {
		t.Fatalf("status mode = %s", st.Mode)
	}
	for i := 0; i < 3; i++ {
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}
	}
}

// A DHCP lease change must not leave a stale URL in status.
func TestNetWatcherRefreshUpdatesURL(t *testing.T) {
	res := Resolution{Mode: ModeLAN, Bind: config.BindAll, Port: 21118, LANIP: "10.0.0.99",
		URL: "http://10.0.0.99:21118"}
	var seen Resolution
	w := newNetWatcher(res, time.Hour, func(r Resolution) { seen = r })
	defer w.close()

	got, changed := w.refresh()
	real := PrimaryLANIP()
	if real == "10.0.0.99" {
		t.Skip("this machine really is on 10.0.0.99")
	}
	if !changed {
		t.Fatal("a changed LAN ip was not detected")
	}
	if got.LANIP != real || seen.LANIP != real {
		t.Fatalf("refresh did not adopt the new ip: %+v", got)
	}
	want := "http://127.0.0.1:21118"
	if real != "" {
		want = "http://" + real + ":21118"
	}
	if got.URL != want {
		t.Fatalf("url after refresh = %s, want %s", got.URL, want)
	}
	if _, changed := w.refresh(); changed {
		t.Fatal("a second refresh with no change should report no change")
	}
}
