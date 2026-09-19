package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// TerminalMode is observe (read-only) or control (one holder, can write).
type TerminalMode string

const (
	ModeObserve TerminalMode = "observe"
	ModeControl TerminalMode = "control"
)

// TerminalFrame is one decoded frame from a terminal stream.
//
// Data is a pooled buffer holding RAW ANSI bytes: base64 is decoded exactly
// here, once, and never re-encoded anywhere downstream. The receiver owns one
// reference and must Release it.
type TerminalFrame struct {
	Data *Buf
	// Off is the number of reserved bytes at the front of Data.B. The raw ANSI
	// payload is Data.B[Off:]. The reservation lets the wire header be written
	// in place, so a frame travels from base64 decode to the socket with zero
	// copies and zero allocations.
	Off    int
	Full   bool
	Width  int
	Height int
	UpSeq  uint64
}

// TerminalHandler receives stream events. OnFrame owns f.Data's reference.
type TerminalHandler interface {
	OnFrame(f TerminalFrame)
	OnClosed(reason string)
}

// TerminalStream wraps `herdr terminal session observe|control <target>`.
//
// There is NO terminal.* method on the socket API in 0.9.0; this subprocess is
// the only streaming primitive. Control accepts NDJSON on stdin.
type TerminalStream struct {
	Target string
	// Socket is the Herdr session socket this stream belongs to. The `herdr`
	// CLI picks its server from $HERDR_SOCKET_PATH, so a multi-session server
	// MUST set it per subprocess: without it every stream would attach to
	// whichever session launched the plugin, and pane ids (which all start at
	// w1:p1) would silently resolve against the wrong tree.
	Socket string
	Mode   TerminalMode
	// Cols/Rows are the geometry to ATTACH AT. Zero means "do not pass
	// --cols/--rows at all", which makes herdr attach at the pane's OWN
	// current size and report it back in the first frame's width/height.
	//
	// That is the non-destructive default and it is the whole point: verified
	// on 0.9.0, `terminal session observe` never changes a pane's PTY, while
	// `terminal session control --cols --rows` changes it permanently. Passing
	// nothing means we cannot impose a size even by accident, and the first
	// frame tells us the size we must render at.
	Cols     int
	Rows     int
	Takeover bool
	// Reserve is the header space left in front of every decoded payload.
	Reserve int

	log     *slog.Logger
	handler TerminalHandler

	mu      sync.Mutex
	done    chan struct{}
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	closed  bool
	inbuf   []byte
	started time.Time
}

// NewTerminalStream builds (but does not start) a stream.
func NewTerminalStream(target string, mode TerminalMode, cols, rows int, h TerminalHandler, log *slog.Logger) *TerminalStream {
	if log == nil {
		log = slog.Default()
	}
	// cols/rows of zero are PRESERVED, not defaulted: they mean "attach at the
	// pane's own size". Defaulting them to 120x32 was how a browser silently
	// imposed a geometry on somebody else's terminal.
	return &TerminalStream{Target: target, Mode: mode, Cols: cols, Rows: rows, handler: h, log: log,
		inbuf: make([]byte, 0, 1024), done: make(chan struct{})}
}

// Start spawns the subprocess and pumps frames until it exits or ctx is done.
// It returns once the process is running; Wait blocks for teardown.
func (t *TerminalStream) Start(ctx context.Context) error {
	args := []string{"terminal", "session", string(t.Mode), t.Target}
	if t.Cols > 0 && t.Rows > 0 {
		args = append(args, "--cols", strconv.Itoa(t.Cols), "--rows", strconv.Itoa(t.Rows))
	}
	if t.Mode == ModeControl && t.Takeover {
		args = append(args, "--takeover")
	}
	bin, err := ResolveHerdrBin()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, args...)
	if t.Socket != "" {
		cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH="+t.Socket)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if t.Mode == ModeControl {
		sin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		t.stdin = sin
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("upstream: start %s %s: %w", t.Mode, t.Target, err)
	}
	t.mu.Lock()
	t.cmd = cmd
	t.started = time.Now()
	t.mu.Unlock()

	go func() {
		<-ctx.Done()
		t.Stop()
	}()

	go func() {
		reason := t.pump(stdout)
		// Reap deterministically: close stdin, wait, never leave a zombie.
		t.mu.Lock()
		if t.stdin != nil {
			_ = t.stdin.Close()
			t.stdin = nil
		}
		t.mu.Unlock()
		_ = cmd.Wait()
		if reason == "" {
			if s := bytes.TrimSpace(stderr.Bytes()); len(s) > 0 {
				reason = string(s)
			} else {
				reason = "stream ended"
			}
		}
		// done closes AFTER the handler has been told, so Wait is a real
		// barrier: once it returns, no OnClosed for this stream can still be in
		// flight. Closing it first let a caller tear a stream down, start a
		// replacement, and then have the OLD stream's late OnClosed delete the
		// NEW one from the session — a running subprocess nobody owned any
		// more, and a client told its still-live target had closed.
		t.handler.OnClosed(reason)
		close(t.done)
	}()
	return nil
}

// pump reads NDJSON records, decoding base64 directly into pooled buffers.
// Returns the close reason if terminal.closed was seen.
func (t *TerminalStream) pump(r io.ReadCloser) string {
	defer r.Close()
	br := bufio.NewReaderSize(r, 1<<18)
	for {
		line, err := readLine(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.log.Debug("terminal stream read", "target", t.Target, "err", err)
			}
			return ""
		}
		if len(line) == 0 {
			continue
		}
		if reason, ok := t.handleLine(line); ok {
			return reason
		}
	}
}

var (
	kBytes  = []byte(`"bytes":"`)
	kSeq    = []byte(`"seq":`)
	kFullT  = []byte(`"full":true`)
	kWidth  = []byte(`"width":`)
	kHeight = []byte(`"height":`)
	kClosed = []byte(`terminal.closed`)
)

// handleLine parses one NDJSON record. The fast path avoids allocating a Go
// string for the (large) base64 payload: base64 never contains a quote or a
// backslash, so the value region can be sliced out of the raw line directly and
// decoded straight into a pooled buffer.
func (t *TerminalStream) handleLine(line []byte) (reason string, closed bool) {
	if bytes.Contains(line, kClosed) {
		var rec TerminalRecord
		if err := json.Unmarshal(line, &rec); err == nil && rec.Type == "terminal.closed" {
			if rec.Reason == "" {
				rec.Reason = "terminal.closed"
			}
			return rec.Reason, true
		}
	}
	i := bytes.Index(line, kBytes)
	if i < 0 {
		return "", false
	}
	start := i + len(kBytes)
	end := bytes.IndexByte(line[start:], '"')
	if end < 0 {
		return "", false
	}
	enc := line[start : start+end]

	off := t.Reserve
	buf := GetBuf(off + base64.StdEncoding.DecodedLen(len(enc)))
	dst := buf.B[:cap(buf.B)]
	n, err := base64.StdEncoding.Decode(dst[off:], enc)
	if err != nil {
		buf.Release()
		t.log.Debug("terminal frame: bad base64", "target", t.Target, "err", err)
		return "", false
	}
	buf.B = dst[:off+n]

	t.handler.OnFrame(TerminalFrame{
		Data:   buf,
		Off:    off,
		Full:   bytes.Contains(line, kFullT),
		Width:  scanInt(line, kWidth),
		Height: scanInt(line, kHeight),
		UpSeq:  uint64(scanInt(line, kSeq)),
	})
	return "", false
}

func scanInt(line, key []byte) int {
	i := bytes.Index(line, key)
	if i < 0 {
		return 0
	}
	j := i + len(key)
	n := 0
	for j < len(line) && line[j] >= '0' && line[j] <= '9' {
		n = n*10 + int(line[j]-'0')
		j++
	}
	return n
}

// Send writes one NDJSON control record on stdin (control mode only).
func (t *TerminalStream) Send(rec any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stdin == nil {
		return errors.New("upstream: stream is not a controller")
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	t.inbuf = append(t.inbuf[:0], b...)
	t.inbuf = append(t.inbuf, '\n')
	_, err = t.stdin.Write(t.inbuf)
	return err
}

// SendInput writes raw keystrokes upstream. This is the keystroke hot path:
// one small JSON envelope, no intermediate copies of the payload.
func (t *TerminalStream) SendInput(data []byte) error {
	return t.Send(map[string]any{"type": "terminal.input", "bytes": base64.StdEncoding.EncodeToString(data)})
}

// Resize asks the upstream terminal to change geometry.
func (t *TerminalStream) Resize(cols, rows int) error {
	t.mu.Lock()
	t.Cols, t.Rows = cols, rows
	t.mu.Unlock()
	return t.Send(map[string]any{"type": "terminal.resize", "cols": cols, "rows": rows})
}

// Scroll moves the viewport.
func (t *TerminalStream) Scroll(delta int) error {
	return t.Send(map[string]any{"type": "terminal.scroll", "delta": delta})
}

// Release gives up control without killing the pane.
func (t *TerminalStream) Release() error {
	return t.Send(map[string]any{"type": "terminal.release"})
}

// Wait blocks until the subprocess has been reaped AND its OnClosed has
// returned, or the timeout elapses.
//
// This matters when upgrading observe -> control on the same terminal: Herdr
// allows ONE attached client, so starting the controller before the observer
// has actually exited fails with "already has an attached client".
func (t *TerminalStream) Wait(timeout time.Duration) bool {
	t.mu.Lock()
	started := t.cmd != nil
	t.mu.Unlock()
	if !started {
		return true
	}
	select {
	case <-t.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Stop tears the subprocess down. Idempotent.
func (t *TerminalStream) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.cmd == nil || t.cmd.Process == nil {
		t.closed = true
		return
	}
	t.closed = true
	if t.stdin != nil {
		_ = t.stdin.Close()
		t.stdin = nil
	}
	_ = t.cmd.Process.Kill()
}
