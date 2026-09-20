//go:build !windows

package platform

import "net"

// PortBindable reports whether addr can be bound right now. Unix sets
// SO_REUSEADDR on listening sockets, so a port left in TIME_WAIT by a closed
// connection is still bindable and a plain Listen is the whole answer.
func PortBindable(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
