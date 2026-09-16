package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"testing"
)

// realFrames are verbatim NDJSON lines captured from
// `herdr terminal session observe <pane> --cols 100 --rows 30` on Herdr 0.9.0.
// Shape: {"type":"terminal.frame","seq":N,"bytes":"<b64>","encoding":"ansi",
//
//	"full":bool,"width":100,"height":30}
func realLine(seq int, full bool, payload string) []byte {
	rec := map[string]any{
		"type": "terminal.frame", "seq": seq,
		"bytes":    base64.StdEncoding.EncodeToString([]byte(payload)),
		"encoding": "ansi", "full": full, "width": 100, "height": 30,
	}
	b, _ := json.Marshal(rec)
	return b
}

type capture struct {
	frames []TerminalFrame
	closed string
}

func (c *capture) OnFrame(f TerminalFrame) { c.frames = append(c.frames, f) }
func (c *capture) OnClosed(reason string)  { c.closed = reason }

func TestHandleLineDecodesRealFrames(t *testing.T) {
	c := &capture{}
	ts := NewTerminalStream("w2:p1", ModeObserve, 100, 30, c, slog.Default())
	ts.Reserve = 13

	payload := "\x1b[?2026h\x1b[2J\x1b[1;1H hello pane "
	for i, full := range []bool{true, false, false} {
		if reason, closed := ts.handleLine(realLine(i+1, full, payload)); closed {
			t.Fatalf("unexpected close: %s", reason)
		}
	}
	if len(c.frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(c.frames))
	}
	if !c.frames[0].Full || c.frames[1].Full {
		t.Fatalf("full flags wrong: %v %v", c.frames[0].Full, c.frames[1].Full)
	}
	for i, f := range c.frames {
		if f.Off != 13 {
			t.Fatalf("frame %d: Off=%d want 13 (header must be reserved in place)", i, f.Off)
		}
		if got := string(f.Data.B[f.Off:]); got != payload {
			t.Fatalf("frame %d: payload %q want %q", i, got, payload)
		}
		if f.Width != 100 || f.Height != 30 || f.UpSeq != uint64(i+1) {
			t.Fatalf("frame %d: geometry/seq wrong: %+v", i, f)
		}
		f.Data.Release()
	}
}

func TestHandleLineTerminalClosed(t *testing.T) {
	c := &capture{}
	ts := NewTerminalStream("w2:p1", ModeObserve, 100, 30, c, slog.Default())
	line := []byte(`{"type":"terminal.closed","reason":"pane exited"}`)
	reason, closed := ts.handleLine(line)
	if !closed || reason != "pane exited" {
		t.Fatalf("got (%q,%v), want (\"pane exited\",true)", reason, closed)
	}
}

// TestFrameDecodeIsAllocationFree proves the steady-state budget from SPEC A3:
// the base64 fast path slices the payload out of the raw line and decodes it
// into a pooled buffer, so a terminal frame costs no heap allocation.
func TestFrameDecodeIsAllocationFree(t *testing.T) {
	c := &capture{}
	ts := NewTerminalStream("w2:p1", ModeObserve, 100, 30, c, slog.Default())
	ts.Reserve = 13
	line := realLine(1, false, string(bytes.Repeat([]byte("x"), 4096)))

	// warm the pool
	for i := 0; i < 64; i++ {
		ts.handleLine(line)
		for _, f := range c.frames {
			f.Data.Release()
		}
		c.frames = c.frames[:0]
	}
	avg := testing.AllocsPerRun(200, func() {
		ts.handleLine(line)
		f := c.frames[len(c.frames)-1]
		c.frames = c.frames[:0]
		f.Data.Release()
	})
	if avg > 0 {
		t.Fatalf("decode allocated %.1f times per frame, want 0", avg)
	}
}

func BenchmarkHandleLine(b *testing.B) {
	c := &capture{}
	ts := NewTerminalStream("w2:p1", ModeObserve, 100, 30, c, slog.Default())
	ts.Reserve = 13
	line := realLine(1, false, string(bytes.Repeat([]byte("x"), 8192)))
	b.ReportAllocs()
	b.SetBytes(8192)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts.handleLine(line)
		f := c.frames[len(c.frames)-1]
		c.frames = c.frames[:0]
		f.Data.Release()
	}
}
