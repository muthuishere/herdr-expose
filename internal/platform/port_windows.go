//go:build windows

package platform

import (
	"net"
	"time"
)

// PortBindable reports whether addr is free for us to serve on.
//
// "Can I bind it?" is the right question on Unix and the WRONG one here. Go
// deliberately does not set SO_REUSEADDR on Windows (there it allows one
// process to steal another's live socket, which is not what the option means on
// Unix). So after a share has served even one browser request, the port sits in
// TIME_WAIT and a fresh Listen fails — while nothing whatsoever is serving.
//
// Measured: `share revoke --all` tore a LAN share down perfectly, then reported
// "port STILL BOUND" and exited non-zero with "1 of 1 share(s) did NOT tear
// down completely", purely because a browser had just connected to it.
//
// What the callers actually mean is "is anything SERVING on this address". So
// if the bind fails, ask that directly: a connection that is refused means no
// listener, whatever TIME_WAIT thinks.
func PortBindable(addr string) bool {
	if ln, err := net.Listen("tcp", addr); err == nil {
		_ = ln.Close()
		return true
	}
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return true // nothing is accepting: the port is ours for all practical purposes
	}
	_ = c.Close()
	return false
}
