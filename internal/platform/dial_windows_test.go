//go:build windows

package platform

import (
	"context"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// The naming rule is the whole port. Herdr exports a filesystem-shaped path in
// $HERDR_SOCKET_PATH on every platform and its own clients prepend `\\.\pipe\`
// to the WHOLE value, drive letter and all. Get this wrong and nothing connects.
func TestControlEndpoint(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{
			`C:\Users\me\AppData\Roaming\herdr\herdr.sock`,
			`\\.\pipe\C:\Users\me\AppData\Roaming\herdr\herdr.sock`,
		},
		{
			`C:\Users\me\AppData\Roaming\herdr\sessions\work\herdr.sock`,
			`\\.\pipe\C:\Users\me\AppData\Roaming\herdr\sessions\work\herdr.sock`,
		},
		// Already namespaced: never prefix twice.
		{`\\.\pipe\herdr-test`, `\\.\pipe\herdr-test`},
		{`//./pipe/herdr-test`, `\\.\pipe\herdr-test`},
		{``, ``},
	} {
		if got := ControlEndpoint(tc.in); got != tc.want {
			t.Errorf("ControlEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDialControlRoundTripOverAPipe proves the transport itself: a real named
// pipe, dialled through the same seam the Herdr client uses, with bytes going
// both ways. It does NOT prove Herdr's naming — TestControlEndpoint covers the
// naming, and only a live Herdr server proves the two together.
func TestDialControlRoundTripOverAPipe(t *testing.T) {
	const name = `\\.\pipe\herdr-expose-test-roundtrip`
	ln, err := winio.ListenPipe(name, nil)
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := c.Read(buf); err == nil {
			_, _ = c.Write([]byte("pong\n"))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialControl(ctx, name)
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := c.Read(buf); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(buf) != "pong\n" {
		t.Errorf("round trip got %q", buf)
	}
}

// The bare-name form must reach the same pipe, since that is the shape a hand
// -set HERDR_SOCKET_PATH would have.
func TestDialControlAcceptsABareName(t *testing.T) {
	const bare = `herdr-expose-test-bare`
	ln, err := winio.ListenPipe(`\\.\pipe\`+bare, nil)
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := DialControlTimeout(bare, 5*time.Second)
	if err != nil {
		t.Fatalf("DialControlTimeout(%q): %v", bare, err)
	}
	c.Close()
}
