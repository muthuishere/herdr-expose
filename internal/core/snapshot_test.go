package core

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

type capSink struct {
	mu    sync.Mutex
	types []byte
}

func (c *capSink) SendBinary(b *upstream.Buf) {
	h, _, err := DecodeServerFrame(b.B)
	c.mu.Lock()
	if err == nil {
		c.types = append(c.types, h.Type)
	}
	c.mu.Unlock()
	b.Release()
}
func (c *capSink) SendJSON(string, any) {}

func (c *capSink) seen() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.types...)
}

func waitFrames(t *testing.T, sink *capSink, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.seen()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("only %d frames arrived, want %d", len(sink.seen()), n)
}

func fullFrame(target string, payload string) upstream.TerminalFrame {
	hdr := HeaderLenFor(target)
	b := upstream.GetBuf(hdr + len(payload))
	b.B = append(b.B[:hdr], payload...)
	return upstream.TerminalFrame{Data: b, Off: hdr, Full: true, Width: 80, Height: 24}
}

// type 2 must mean "your buffer cannot be trusted". Herdr sends a `full` repaint
// periodically on a completely idle pane; mapping every one of those to type 2
// told the client to reset several times a second, which is what made the
// terminal flicker. Only the FIRST full frame after attaching is a snapshot.
func TestOnlyFirstFullFrameIsASnapshot(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &capSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "alpha/w1:p1"
	ls := &liveStream{hdrLen: HeaderLenFor(target), target: target}
	ls.needSnap.Store(true) // set on attach
	sh := &streamHandler{sess: sess, target: target, hdrLen: HeaderLenFor(target), ls: ls}

	// Spaced out so the coalescer drains between them: back-to-back repaints
	// legitimately collapse into one write, and that is not what is under test.
	for i := 0; i < 4; i++ {
		sh.OnFrame(fullFrame(target, "\x1b[2J\x1b[Hidle repaint"))
		waitFrames(t, sink, i+1)
	}
	got := sink.seen()
	if len(got) != 4 {
		t.Fatalf("got %d frames, want 4: %v", len(got), got)
	}
	if got[0] != TypeSnapshot {
		t.Fatalf("first frame after attach must be a snapshot, got type %d", got[0])
	}
	for i, typ := range got[1:] {
		if typ != TypeFrame {
			t.Fatalf("periodic full repaint %d was sent as type %d, want %d (frame)",
				i+1, typ, TypeFrame)
		}
	}

	// After a gap, the client's buffer is untrustworthy again.
	ls.needSnap.Store(true)
	sh.OnFrame(fullFrame(target, "\x1b[2J\x1b[Hafter gap"))
	waitFrames(t, sink, 5)
	got = sink.seen()
	if got[len(got)-1] != TypeSnapshot {
		t.Fatalf("first full frame after a gap must be a snapshot, got type %d", got[len(got)-1])
	}
}
