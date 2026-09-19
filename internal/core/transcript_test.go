package core

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[1;32;40mx\x1b[m", "x"},
		{"\x1b]0;a title\x07after", "after"},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"a\rb", "ab"},
		{"keep\nnewlines\n", "keep\nnewlines\n"},
		{"\x1b(Bplain", "plain"},
		{"\x1b[?25lhidden\x1b[?25h", "hidden"},
		// A truncated escape must not emit its own garbage.
		{"tail\x1b[", "tail"},
	}
	for _, c := range cases {
		if got := StripANSI(c.in); got != c.want {
			t.Errorf("StripANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseModeTranscript(t *testing.T) {
	if ParseMode("transcript") != ModeTranscript {
		t.Fatal("transcript must parse to ModeTranscript")
	}
	// An unknown mode is still `none`, not transcript: a client that misspells
	// a mode must get the cheapest one, never a poll it did not ask for.
	if ParseMode("transcipt") != ModeNone {
		t.Fatal("an unknown mode must fall back to none")
	}
}

// TestTranscriptNeverAttaches is the property AMENDMENTS 13 exists for.
//
// `herdr terminal session observe --cols --rows` is the only thing here that
// can change a pane's geometry, and startStream is the only place it is
// spawned. A transcript target must never reach it — that is what makes
// opening a pane on a phone non-destructive.
//
// Note what the test deliberately does NOT assert: that a `resize` is refused.
// It used to, and that was a deadlock — a client toggling transcript ->
// terminal sends `resize` before `viewport: live` (B2 requires that order), so
// the resize necessarily arrives while the server still believes the target is
// a transcript. Dropping it left the target with no geometry and `live`
// refused itself forever. Recording a number is harmless; attaching is not.
func TestTranscriptNeverAttaches(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := h.NewSession(ctx, &jsonSink{})
	defer sess.Close()

	const target = "s/w1:p1"
	sess.SetGeometry(target, 120, 40)
	sess.SetViewport(map[string]string{target: "transcript"})

	sess.mu.Lock()
	streams := len(sess.streams)
	sess.mu.Unlock()
	if streams != 0 {
		t.Fatalf("transcript mode started %d stream(s)", streams)
	}

	// Even asked directly, at a perfectly valid geometry.
	if _, err := sess.startStream(target, upstream.ModeObserve); err != ErrTranscriptStream {
		t.Fatalf("startStream on a transcript target: got %v, want ErrTranscriptStream", err)
	}

	// And a resize arriving mid-toggle is RECORDED, not dropped, so the
	// subsequent switch to live is not refused for want of geometry.
	sess.SetGeometry(target, 100, 30)
	if g := sess.Geometry(target); g.Cols != 100 || g.Rows != 30 {
		t.Fatalf("a resize during transcript must still be recorded, got %+v", g)
	}
}

// TestTranscriptRefusesRawInput: the key bar's raw path must not be able to
// attach a controlling terminal to a transcript target. A transcript
// connection records no geometry, so such a stream would attach at the 20x6
// floor — one tap would squeeze a real agent into a 20-column terminal.
func TestTranscriptRefusesRawInput(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := h.NewSession(ctx, &jsonSink{})
	defer sess.Close()

	const target = "s/w1:p1"
	sess.SetViewport(map[string]string{target: "transcript"})
	err := sess.Input(target, []byte("y"))
	if err == nil {
		t.Fatal("raw input to a transcript target must be refused")
	}
	if !strings.Contains(err.Error(), "agent.prompt") {
		t.Fatalf("the refusal must name the path that DOES work, got: %v", err)
	}
	sess.mu.Lock()
	n := len(sess.streams)
	sess.mu.Unlock()
	if n != 0 {
		t.Fatalf("refused input still started %d stream(s)", n)
	}
}

// TestTranscriptSendsOnlyOnChange: a 1Hz unconditional push is the
// summary-firehose mistake again (26 panes x a full screen every 0.65s was
// 206KB/s). A subscriber gets the text once, and then only when it changes.
func TestTranscriptSendsOnlyOnChange(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &countingSink{}
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "s/w1:p1"
	p := h.transcript
	p.subscribe(target, sess)

	deliver := func(text string) {
		// Drive the delivery half of poll() directly: the upstream read is what
		// a unit test cannot have, the change detection is what it must prove.
		p.deliver(target, TranscriptFrame{Target: target, Text: text}, text)
	}

	deliver("hello")
	deliver("hello")
	deliver("hello")
	if got := sink.count("transcript"); got != 1 {
		t.Fatalf("unchanged text sent %d times, want 1", got)
	}
	deliver("hello world")
	if got := sink.count("transcript"); got != 2 {
		t.Fatalf("changed text: got %d frames, want 2", got)
	}

	// A NEW subscriber must get the current text immediately even though
	// nothing changed — otherwise opening a pane shows an empty view until the
	// agent happens to move.
	sink2 := &countingSink{}
	sess2 := h.NewSession(ctx, sink2)
	defer sess2.Close()
	p.subscribe(target, sess2)
	deliver("hello world")
	if got := sink2.count("transcript"); got != 1 {
		t.Fatalf("new subscriber got %d frames, want 1", got)
	}
	if got := sink.count("transcript"); got != 2 {
		t.Fatalf("existing subscriber was re-sent unchanged text: %d", got)
	}
}

type countingSink struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *countingSink) SendBinary(b *upstream.Buf) { b.Release() }
func (c *countingSink) SendJSON(typ string, _ any) {
	c.mu.Lock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[typ]++
	c.mu.Unlock()
}
func (c *countingSink) count(typ string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[typ]
}
