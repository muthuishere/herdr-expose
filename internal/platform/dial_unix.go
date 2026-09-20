//go:build !windows

package platform

import (
	"context"
	"net"
	"time"
)

// ControlEndpoint maps $HERDR_SOCKET_PATH to the address this platform dials.
// On Unix it is already a filesystem path to an AF_UNIX socket.
func ControlEndpoint(path string) string { return path }

// DialControl connects to a Herdr control socket.
func DialControl(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// DialControlTimeout is the liveness probe used by session discovery.
func DialControlTimeout(path string, d time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, d)
}
