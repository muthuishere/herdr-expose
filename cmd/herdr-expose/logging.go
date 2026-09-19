package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// Logging exists to answer one question after the fact: WHAT DID THE DAEMON DO?
//
// `herdr-expose daemon` forks, setsids and returns. Its stderr therefore goes
// to whatever the parent happened to leave open — which is nowhere anybody can
// find. So the log is a real FILE by default, in the state dir, written by the
// daemon and read back by `herdr-expose logs` through the SAME resolver, so it
// can never land somewhere nothing reads. A share writes its own file inside
// its own state dir, so a share's whole lifetime is auditable on its own and
// the record disappears with the share at teardown.
//
// Two things must NEVER reach it, at any level including debug:
//
//  1. TERMINAL OUTPUT. Pane bytes are the contents of the user's screen and can
//     contain anything — a password typed at a prompt, a customer record,
//     unreleased source. A debug line that dumps a frame turns the log into a
//     copy of everything the user did.
//  2. CREDENTIALS. The Cloudflare API token, device tokens and pairing codes
//     are logged as ids and hashes, never as values.
//
// Both are enforced by the handler below — in code, not by convention — because
// the log survives for weeks and gets pasted into issues.

// logFilePath is where the daemon writes and what `herdr-expose logs` reads.
// An explicit [log] file wins; otherwise it is the default name in the state
// dir. One function, both callers, by construction.
func logFilePath(cfg *config.Config, state string) string {
	if f := strings.TrimSpace(cfg.Log.File); f != "" {
		return expandHome(f)
	}
	return filepath.Join(state, config.DefaultLogFileName)
}

// shareLogPath is a share's own log, inside its own state dir, so that
// revoking the share takes its log with it.
func shareLogPath(shareDir string) string {
	return filepath.Join(shareDir, "share.log")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// newLogger is the stderr logger the one-shot commands use, before any config
// has necessarily been read.
func newLogger() *slog.Logger {
	lvl := slog.LevelInfo
	if os.Getenv("HERDR_EXPOSE_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	return slog.New(newSafeHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
}

// buildLogger applies the [log] table and returns the logger plus a closer.
//
// $HERDR_EXPOSE_DEBUG still wins over the configured level: when you are
// debugging you are standing at the machine, and having to edit a config file
// first is the friction that makes people give up and add print statements.
func buildLogger(cfg *config.Config, path string) (*slog.Logger, io.Closer, error) {
	lvl := cfg.Log.SlogLevel()
	if os.Getenv("HERDR_EXPOSE_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: lvl}

	w, err := newRotatingFile(path, cfg.Log.MaxSizeMB, cfg.Log.Keep)
	if err != nil {
		return nil, nil, err
	}

	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(cfg.Log.Format), "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(newSafeHandler(h)), w, nil
}

// --- rotation ---------------------------------------------------------------

// rotatingFile is a size-rotating log file with no external dependency.
//
// An unbounded log on a process designed to run for weeks is a disk-filling
// bug, and reaching for a rotation library for eighty lines of code is how a
// small tool acquires a supply chain.
type rotatingFile struct {
	mu      sync.Mutex
	path    string
	maxSize int64 // bytes; 0 disables rotation
	keep    int
	f       *os.File
	size    int64
}

func newRotatingFile(path string, maxSizeMB, keep int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("log file %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("log file %s: %w", path, err)
	}
	r := &rotatingFile{path: path, maxSize: int64(maxSizeMB) * 1024 * 1024, keep: keep, f: f}
	if st, err := f.Stat(); err == nil {
		r.size = st.Size()
	}
	return r, nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Rotate BEFORE the write that would cross the line, so a record is never
	// split across two files.
	if r.maxSize > 0 && r.size+int64(len(p)) > r.maxSize {
		if err := r.rotate(); err != nil {
			// A rotation failure must not lose the line.
			fmt.Fprintf(os.Stderr, "herdr-expose: log rotation failed: %v\n", err)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate shifts path -> path.1 -> path.2 ... and drops anything past keep.
func (r *rotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	if r.keep <= 0 {
		// Rotation with nothing retained means truncate: still bounded, and
		// still never unbounded growth.
		_ = os.Remove(r.path)
	} else {
		_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
		for i := r.keep - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
		}
		_ = os.Rename(r.path, r.path+".1")
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	r.f, r.size = f, 0
	return nil
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// --- the safety filter ------------------------------------------------------

// payloadAttrKeys name attributes that carry, or plausibly carry, terminal
// content or a credential. The VALUE of such an attribute is never written;
// the key is kept so the line still says what happened.
var payloadAttrKeys = map[string]bool{
	// terminal payload
	"bytes": true, "pane_bytes": true, "frame": true, "frames": true,
	"data": true, "payload": true, "body": true,
	"output": true, "stdout": true, "stderr": true,
	"text": true, "line": true, "lines": true, "content": true,
	"input": true, "keys": true, "keystrokes": true,
	"snapshot": true, "screen": true, "buffer": true, "scrollback": true,
	// credentials
	"token": true, "secret": true, "password": true, "passwd": true,
	"authorization": true, "auth_token": true, "api_key": true, "apikey": true,
	"code": true, "pairing_code": true, "credential": true, "credentials": true,
	"header": true, "headers": true, "cookie": true,
}

// safeValue is what replaces a refused value. It names the reason, so a
// missing value in a log line is never mistaken for a bug.
const safeValue = "[withheld: terminal output and credentials are never logged]"

// safeHandler scrubs every record on its way to the real handler:
//
//   - a value under a payload/credential key is replaced, never written;
//   - ANY []byte value is replaced whatever it is called — that is what a
//     frame looks like;
//   - every string is bounded, because a value that long is a payload;
//   - known secret VALUES (the Cloudflare and ngrok tokens) are
//     scrubbed out of free text, the same trick internal/expose already plays,
//     so a third-party error string cannot smuggle one through.
type safeHandler struct{ inner slog.Handler }

func newSafeHandler(inner slog.Handler) slog.Handler { return &safeHandler{inner: inner} }

func (h *safeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *safeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &safeHandler{inner: h.inner.WithAttrs(sanitizeAttrs(as))}
}

func (h *safeHandler) WithGroup(name string) slog.Handler {
	return &safeHandler{inner: h.inner.WithGroup(name)}
}

func (h *safeHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, scrubText(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(sanitizeAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func sanitizeAttrs(as []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(as))
	for _, a := range as {
		out = append(out, sanitizeAttr(a))
	}
	return out
}

func sanitizeAttr(a slog.Attr) slog.Attr {
	if payloadAttrKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, safeValue)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(sanitizeAttrs(v.Group())...)}
	case slog.KindString:
		return slog.String(a.Key, scrubText(truncate(v.String())))
	case slog.KindAny:
		if _, isBytes := v.Any().([]byte); isBytes {
			// A []byte is what a frame looks like, whatever the key says.
			return slog.String(a.Key, safeValue)
		}
		if err, isErr := v.Any().(error); isErr && err != nil {
			return slog.String(a.Key, scrubText(truncate(err.Error())))
		}
	}
	return a
}

// maxAttrLen bounds a logged string. Log attributes are metadata; something
// longer than this is a payload, and payloads are not logged.
const maxAttrLen = 512

func truncate(s string) string {
	if len(s) <= maxAttrLen {
		return s
	}
	return s[:maxAttrLen] + "…[truncated]"
}

// secretEnvNames are variables whose VALUES must never appear in a log line,
// even inside somebody else's error string.
var secretEnvNames = []string{
	"CLOUDFLARE_ALLPURPOSE_TOKEN", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_TUNNEL_TOKEN",
	"NGROK_AUTHTOKEN",
}

func scrubText(s string) string {
	for _, env := range secretEnvNames {
		if v := strings.TrimSpace(os.Getenv(env)); len(v) >= 8 && strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[redacted $"+env+"]")
		}
	}
	return s
}

// auditLogger is the logger the one-shot CLI verbs use to append to the MAIN
// daemon log: share create / extend / revoke, pairing, panic.
//
// Those verbs run in their OWN process, not in the daemon, so without this
// their effects would be invisible in the log that explains the daemon's
// behaviour — "the tunnel went away at 03:12" with no record of the `revoke`
// that did it. Falls back to stderr when the log cannot be opened; an audit
// line is never worth failing a command over.
func auditLogger() (*slog.Logger, io.Closer) {
	state, err := StateDir()
	if err != nil {
		return newLogger(), nil
	}
	cfg, err := config.Load()
	if err != nil {
		return newLogger(), nil
	}
	l, closer, err := buildLogger(cfg, logFilePath(cfg, state))
	if err != nil {
		return newLogger(), nil
	}
	return l, closer
}

// shareLogger is a share's OWN log, inside the share's state dir. A share's
// lifetime is then auditable on its own, and the record is destroyed with the
// share at teardown rather than lingering in the machine-wide log.
func shareLogger(shareDir, id string) (*slog.Logger, io.Closer) {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
	}
	l, closer, err := buildLogger(cfg, shareLogPath(shareDir))
	if err != nil {
		return newLogger().With("share", id), nil
	}
	return l.With("share", id), closer
}
