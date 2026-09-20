//go:build !windows

package platform

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestControlEndpointIsIdentityOnUnix(t *testing.T) {
	const p = "/Users/me/.config/herdr/herdr.sock"
	if got := ControlEndpoint(p); got != p {
		t.Errorf("ControlEndpoint(%q) = %q, want it untouched", p, got)
	}
}

func TestDialControlRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
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
	c, err := DialControl(ctx, sock)
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

	if _, err := DialControlTimeout(sock, time.Second); err != nil {
		t.Errorf("DialControlTimeout: %v", err)
	}
}
