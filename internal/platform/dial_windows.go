//go:build windows

package platform

import (
	"context"
	"net"
	"strings"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// pipePrefix is the only namespace a local named pipe can live in.
const pipePrefix = `\\.\pipe\`

// ControlEndpoint maps $HERDR_SOCKET_PATH to a dialable named-pipe path.
//
// This is NOT a guess. Herdr sets the same variable to the same
// filesystem-shaped value on every platform — `session.rs` builds
// `<config_dir>/herdr.sock` (or `<config_dir>/sessions/<name>/herdr.sock`) with
// no Windows branch, and `integration/env.rs` exports that PathBuf verbatim.
// The Windows-ness lives entirely in the CLIENT, and Herdr's own bundled
// integrations spell it one way, in five separate files:
//
//	const socketPath = process.env.HERDR_SOCKET_PATH;
//	const socketEndpoint =
//	  process.platform === "win32" ? `\\\\.\\pipe\\${socketPath}` : socketPath;
//
// i.e. `\\.\pipe\` + the WHOLE value, drive letter and all. The server agrees:
// `ipc.rs` hands the same string to `interprocess`'s GenericNamespaced, which
// prepends the identical prefix. So the live pipe is genuinely named
//
//	\\.\pipe\C:\Users\me\AppData\Roaming\herdr\herdr.sock
//
// which looks wrong and is correct — a pipe name is an opaque string in the
// pipe namespace, and `:` and `\` after the prefix are legal in it.
//
// The one thing we tolerate beyond that is a value that ALREADY names the pipe
// namespace, so that a future Herdr which exports the fully-qualified form, or
// an operator who sets the variable by hand, does not get it prefixed twice.
func ControlEndpoint(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, pipePrefix) || strings.HasPrefix(path, `\\?\pipe\`) ||
		strings.HasPrefix(path, `//./pipe/`) {
		return strings.ReplaceAll(path, "/", `\`)
	}
	return pipePrefix + path
}

// DialControl connects to a Herdr control pipe.
func DialControl(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, ControlEndpoint(path))
}

// DialControlTimeout is the liveness probe used by session discovery.
func DialControlTimeout(path string, d time.Duration) (net.Conn, error) {
	return winio.DialPipe(ControlEndpoint(path), &d)
}
