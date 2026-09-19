package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// The log is a FILE by default, not stderr: a forked, setsid daemon's stderr
// goes nowhere anybody can find, and that is the problem this solves.
func TestDefaultLogIsAFileInTheStateDir(t *testing.T) {
	cfg := config.Defaults()
	state := t.TempDir()
	got := logFilePath(cfg, state)
	want := filepath.Join(state, config.DefaultLogFileName)
	if got != want {
		t.Fatalf("default log path = %q, want %q", got, want)
	}
	// An explicit path overrides.
	cfg.Log.File = filepath.Join(state, "elsewhere.log")
	if logFilePath(cfg, state) != cfg.Log.File {
		t.Fatalf("explicit log.file ignored")
	}
}

// An unbounded log on a daemon designed to run for weeks is a disk-filling
// bug, so rotation is measured, not assumed.
func TestRotationBoundsTheLogAndKeepsN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	// 1 MB cap, keep 2.
	w, err := newRotatingFile(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("y"), 64*1024)
	line[len(line)-1] = '\n'
	for i := 0; i < 80; i++ { // ~5 MB: several rotations
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var total int64
	for _, name := range []string{"x.log", "x.log.1", "x.log.2"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
			total += st.Size()
			if st.Size() > 1<<20 {
				t.Fatalf("%s is %d bytes, over the 1MB cap", name, st.Size())
			}
		}
	}
	// keep = 2 means at most live + 2 rotated. A third must have been dropped.
	if _, err := os.Stat(filepath.Join(dir, "x.log.3")); err == nil {
		t.Fatal("x.log.3 exists; keep = 2 was not enforced")
	}
	if total > 3<<20 {
		t.Fatalf("total on disk %d bytes; rotation is not bounding anything", total)
	}
	// And the whole point: 5MB written, far less retained.
	if total >= 5<<20 {
		t.Fatalf("nothing was actually rotated away: %d bytes", total)
	}
}

// Rotation must not lose the current line, and the log must still be readable
// after a rotation.
func TestRotationKeepsWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	w, err := newRotatingFile(path, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 40; i++ {
		if _, err := fmt.Fprintf(w, "%s line %d\n", strings.Repeat("z", 60*1024), i); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "line 39") {
		t.Fatal("the most recent line is not in the live log file")
	}
}

// TERMINAL OUTPUT AND CREDENTIALS NEVER REACH THE LOG, at any level including
// debug. Enforced in code, because the log runs for weeks and gets pasted into
// issues.
func TestPaneBytesAndSecretsNeverReachTheLog(t *testing.T) {
	var buf bytes.Buffer
	h := newSafeHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log := slog.New(h)

	const screen = "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI customer=4111111111111111"
	log.Debug("frame", "bytes", screen)
	log.Debug("frame", "frame", []byte(screen))
	log.Debug("read pane", "output", screen)
	log.Debug("blob under an innocent name", "thing", []byte(screen))
	log.Debug("snapshot", "screen", screen)
	log.Info("pairing", "code", "8H2K4Q")
	log.Info("auth", "token", "abcdefghijklmnop")
	log.Debug("nested", slog.Group("pane", slog.String("text", screen)))

	out := buf.String()
	if strings.Contains(out, "wJalrXUtnFEMI") || strings.Contains(out, "4111111111111111") {
		t.Fatalf("terminal content reached the log:\n%s", out)
	}
	if strings.Contains(out, "8H2K4Q") || strings.Contains(out, "abcdefghijklmnop") {
		t.Fatalf("a credential reached the log:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Fatalf("values were dropped without saying why:\n%s", out)
	}
	// The line itself still has to be useful.
	for _, want := range []string{"frame", "read pane", "pairing", "snapshot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the event %q was lost along with its payload:\n%s", want, out)
		}
	}

	// Ordinary operational attributes are untouched — the filter must not make
	// the log useless.
	buf.Reset()
	log.Info("share created", "share", "a1b2c3", "session", "main", "mode", "lan", "port", 21118)
	out = buf.String()
	for _, want := range []string{"a1b2c3", "main", "lan", "21118"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a useful attribute was scrubbed: want %q in\n%s", want, out)
		}
	}
}

// A secret VALUE must not slip through inside somebody else's error string,
// which is exactly how tokens leak in practice.
func TestSecretValuesAreScrubbedFromFreeText(t *testing.T) {
	t.Setenv("CLOUDFLARE_ALLPURPOSE_TOKEN", "TOKEN-VALUE-THAT-MUST-NOT-APPEAR")
	var buf bytes.Buffer
	log := slog.New(newSafeHandler(slog.NewTextHandler(&buf, nil)))
	log.Warn("cloudflare api GET /zones failed with Authorization: Bearer TOKEN-VALUE-THAT-MUST-NOT-APPEAR",
		"err", fmt.Errorf("bad auth for TOKEN-VALUE-THAT-MUST-NOT-APPEAR"))
	out := buf.String()
	if strings.Contains(out, "TOKEN-VALUE-THAT-MUST-NOT-APPEAR") {
		t.Fatalf("the Cloudflare token leaked through free text:\n%s", out)
	}
	if !strings.Contains(out, "redacted") {
		t.Fatalf("scrub left no trace of what it removed:\n%s", out)
	}
}

// A very long attribute is a payload, not metadata.
func TestLongValuesAreTruncated(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(newSafeHandler(slog.NewTextHandler(&buf, nil)))
	log.Info("big", "note", strings.Repeat("q", maxAttrLen*4))
	out := buf.String()
	if !strings.Contains(out, "truncated") {
		t.Fatalf("a %d-char attribute was written whole:\n%.200s", maxAttrLen*4, out)
	}
	if len(out) > maxAttrLen*2 {
		t.Fatalf("truncation did not bound the line: %d bytes", len(out))
	}
}

// buildLogger honours format and level, and writes where logFilePath says.
func TestBuildLoggerWritesJSONAtTheConfiguredLevel(t *testing.T) {
	state := t.TempDir()
	cfg := config.Defaults()
	cfg.Log.Format = "json"
	cfg.Log.Level = "warn"
	path := logFilePath(cfg, state)

	log, closer, err := buildLogger(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("should not appear")
	log.Info("should not appear either")
	log.Warn("server stopped", "mode", "local")
	closer.Close()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	if strings.Contains(out, "should not appear") {
		t.Fatalf("level not honoured:\n%s", out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("format=json did not produce JSON:\n%s", out)
	}
	if !strings.Contains(out, "server stopped") {
		t.Fatalf("the warn line is missing:\n%s", out)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("log perms = %v, want 0600", st.Mode().Perm())
	}
}

func TestSafeHandlerEnabledDelegates(t *testing.T) {
	inner := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := newSafeHandler(inner)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("safeHandler ignored the inner level")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("safeHandler suppressed an error")
	}
	// WithAttrs must sanitize too, or a logger built with .With("token", x)
	// leaks on every line it ever writes.
	var buf bytes.Buffer
	l := slog.New(newSafeHandler(slog.NewTextHandler(&buf, nil))).With("token", "leak-me-please")
	l.Info("hello")
	if strings.Contains(buf.String(), "leak-me-please") {
		t.Fatalf("WithAttrs bypassed the filter:\n%s", buf.String())
	}
}
