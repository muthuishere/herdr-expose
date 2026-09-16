package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoSocket is returned when HERDR_SOCKET_PATH is unset.
var ErrNoSocket = errors.New("upstream: HERDR_SOCKET_PATH is not set")

// SocketPath returns the configured Herdr socket path.
func SocketPath() string { return os.Getenv("HERDR_SOCKET_PATH") }

// binCandidates are the directories a launchd/systemd unit will NOT have on
// PATH. Units start with a minimal PATH, so a bare "herdr" fails exec and the
// browser sees an endless reconnect loop. Resolve absolutely, up front, and
// fail with a real error instead.
var binCandidates = []string{
	"~/.local/bin", "~/.cargo/bin", "~/bin",
	"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin",
}

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// ResolveHerdrBin returns an absolute, executable path to the herdr binary.
// Resolved once per process and cached: $HERDR_BIN_PATH, then PATH, then the
// well-known install dirs.
func ResolveHerdrBin() (string, error) {
	binOnce.Do(func() { binPath, binErr = resolveHerdrBin() })
	return binPath, binErr
}

func resolveHerdrBin() (string, error) {
	if p := os.Getenv("HERDR_BIN_PATH"); p != "" {
		if abs, err := filepath.Abs(p); err == nil && isExec(abs) {
			return abs, nil
		}
		return "", fmt.Errorf("upstream: HERDR_BIN_PATH=%q is not an executable file", p)
	}
	if p, err := exec.LookPath("herdr"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
	}
	home, _ := os.UserHomeDir()
	for _, dir := range binCandidates {
		if strings.HasPrefix(dir, "~/") {
			if home == "" {
				continue
			}
			dir = filepath.Join(home, dir[2:])
		}
		cand := filepath.Join(dir, "herdr")
		if isExec(cand) {
			return cand, nil
		}
	}
	return "", errors.New("upstream: cannot find the herdr binary; set $HERDR_BIN_PATH to its absolute path")
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// BinPath returns the resolved herdr binary, or "herdr" if resolution failed
// (callers that care use ResolveHerdrBin and surface the error).
func BinPath() string {
	if p, err := ResolveHerdrBin(); err == nil {
		return p
	}
	return "herdr"
}

// Client talks to the Herdr control socket.
//
// IMPORTANT (verified live against 0.9.0): the socket serves exactly ONE
// request per connection and then closes. Pipelining a second request on the
// same connection yields a broken pipe. So every RPC dials a fresh connection.
// Only events.subscribe keeps a connection open, and that lives in events.go.
type Client struct {
	sock    string
	session string
	timeout time.Duration
	ids     atomic.Uint64
}

// NewClient builds a client for the given socket path ("" => $HERDR_SOCKET_PATH).
func NewClient(sock string) *Client {
	if sock == "" {
		sock = SocketPath()
	}
	return &Client{sock: sock, session: SessionNameForSocket(sock), timeout: 10 * time.Second}
}

// NewSessionClient builds a client bound to one discovered session.
func NewSessionClient(s Session) *Client {
	return &Client{sock: s.SocketPath, session: s.Name, timeout: 10 * time.Second}
}

// Socket returns the socket path in use.
func (c *Client) Socket() string { return c.sock }

// Session returns the name of the Herdr session this client talks to.
func (c *Client) Session() string { return c.session }

func (c *Client) nextID() string {
	return fmt.Sprintf("x%d", c.ids.Add(1))
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c.sock == "" {
		return nil, ErrNoSocket
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.sock)
}

// Call issues one request and returns the raw `result` payload.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("upstream: dial %s: %w", c.sock, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req := Request{ID: c.nextID(), Method: method, Params: params}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("upstream: write %s: %w", method, err)
	}

	br := bufio.NewReaderSize(conn, 1<<16)
	raw, err := readLine(br)
	if err != nil {
		return nil, fmt.Errorf("upstream: read %s: %w", method, err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("upstream: decode %s: %w", method, err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// CallInto issues one request and unmarshals `result` into out.
func (c *Client) CallInto(ctx context.Context, method string, params any, out any) error {
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Ping returns Herdr's version/protocol handshake.
func (c *Client) Ping(ctx context.Context) (*Pong, error) {
	raw, err := c.Call(ctx, "ping", map[string]any{})
	if err != nil {
		return nil, err
	}
	var p Pong
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	p.Raw = raw
	return &p, nil
}

// SessionSnapshot fetches the whole tree in one call.
func (c *Client) SessionSnapshot(ctx context.Context) (*Snapshot, error) {
	var res sessionSnapshotResult
	if err := c.CallInto(ctx, "session.snapshot", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return &res.Snapshot, nil
}

// PaneRead reads a pane's buffer (used for SUMMARY tiles and agent Q&A).
func (c *Client) PaneRead(ctx context.Context, paneID, source, format string, lines int) (string, error) {
	params := map[string]any{
		"pane_id":    paneID,
		"source":     source,
		"format":     format,
		"strip_ansi": format != "ansi",
	}
	if lines > 0 {
		params["lines"] = lines
	}
	var res PaneReadResult
	if err := c.CallInto(ctx, "pane.read", params, &res); err != nil {
		return "", err
	}
	return res.Read.Text, nil
}

// readLine reads one newline-terminated JSON document, without a size cap that
// would truncate a large session.snapshot.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, more, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if !more {
			return out, nil
		}
	}
}
