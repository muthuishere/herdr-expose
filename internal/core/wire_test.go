package core

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

func TestServerFrameRoundTrip(t *testing.T) {
	payload := []byte("\x1b[2Jhello")
	b := encodeFrame(TypeFrame, 42, "w2:p1", payload)
	defer b.Release()

	h, got, err := DecodeServerFrame(b.B)
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != TypeFrame || h.Seq != 42 || h.Target != "w2:p1" {
		t.Fatalf("header wrong: %+v", h)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload %q want %q", got, payload)
	}
}

func TestGapFrameCarriesByteCount(t *testing.T) {
	b := encodeGap(7, "w2:p1", 123456)
	defer b.Release()
	h, payload, err := DecodeServerFrame(b.B)
	if err != nil || h.Type != TypeGap || len(payload) != 8 {
		t.Fatalf("bad gap frame: %+v %v %v", h, payload, err)
	}
	var want uint64 = 123456
	var got uint64
	for _, c := range payload {
		got = got<<8 | uint64(c)
	}
	if got != want {
		t.Fatalf("dropped=%d want %d", got, want)
	}
}

func TestClientInputFrameRoundTrip(t *testing.T) {
	// [type=16][tlen u16][target][raw bytes]
	target := "w2:p1"
	raw := []byte{0x1b, '[', 'A'}
	buf := make([]byte, 0, ClientHeaderLen+len(target)+len(raw))
	buf = append(buf, TypeInput, byte(len(target)>>8), byte(len(target)))
	buf = append(buf, target...)
	buf = append(buf, raw...)

	typ, tgt, payload, err := DecodeClientFrame(buf)
	if err != nil || typ != TypeInput || tgt != target || !bytes.Equal(payload, raw) {
		t.Fatalf("round trip failed: %v %q %v %v", typ, tgt, payload, err)
	}
}

func TestStampHeaderMatchesEncodeFrame(t *testing.T) {
	target := "w2:p1"
	hdr := HeaderLenFor(target)
	payload := []byte("abcdef")

	inPlace := upstream.GetBuf(hdr + len(payload))
	inPlace.B = append(inPlace.B[:hdr], payload...)
	stampHeader(inPlace.B, TypeFrame, 9, target)
	defer inPlace.Release()

	built := encodeFrame(TypeFrame, 9, target, payload)
	defer built.Release()

	if !bytes.Equal(inPlace.B, built.B) {
		t.Fatalf("in-place stamp %v != encodeFrame %v", inPlace.B, built.B)
	}
}

// drainForTest performs exactly the buffer swap run() does, synchronously.
func drainForTest(c *coalescer, target string) ([]*upstream.Buf, []time.Time, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pending[target]
	if p == nil {
		return nil, nil, 0
	}
	p.frames, p.spareFrames = p.spareFrames[:0], p.frames
	p.at, p.spareAt = p.spareAt[:0], p.at
	p.bytes = 0
	p.queued = false
	c.order = c.order[:0]
	c.bytes = 0
	return p.spareFrames, p.spareAt, p.hdrLen
}

// sinkRecorder counts what a connection would have written. The coalescer
// writes from its own goroutine while the test reads, so it is mutex-guarded.
type sinkRecorder struct {
	mu     sync.Mutex
	frames int
	bytes  int
	json   int
	last   []byte
}

func (s *sinkRecorder) SendBinary(b *upstream.Buf) {
	s.mu.Lock()
	s.frames++
	s.bytes += b.Len()
	s.last = append(s.last[:0], b.B...)
	s.mu.Unlock()
	b.Release()
}

func (s *sinkRecorder) SendJSON(string, any) {
	s.mu.Lock()
	s.json++
	s.mu.Unlock()
}

func (s *sinkRecorder) counts() (frames, bytes, jsonN int, last []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames, s.bytes, s.json, append([]byte(nil), s.last...)
}

// TestCoalescerNeverSleeps proves B3: the writer drains what is already queued
// and writes immediately. A frame must reach the sink without waiting for any
// accumulation tick.
func TestCoalescerNeverSleeps(t *testing.T) {
	rec := &sinkRecorder{}
	c := newCoalescer(rec)
	m := NewMetrics()
	go c.run(&seqCounter{}, nil, m)
	defer c.close()

	target := "w2:p1"
	hdr := HeaderLenFor(target)
	buf := upstream.GetBuf(hdr + 3)
	buf.B = append(buf.B[:hdr], 'a', 'b', 'c')

	start := time.Now()
	c.push(target, buf, hdr, false, start)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n, _, _, _ := rec.counts(); n > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	nFrames, _, _, last := rec.counts()
	if nFrames != 1 {
		t.Fatalf("frame did not arrive: %d", nFrames)
	}
	if el := time.Since(start); el > 10*time.Millisecond {
		t.Fatalf("coalescer added %v of latency; it must not sleep to accumulate", el)
	}
	h, payload, err := DecodeServerFrame(last)
	if err != nil || h.Target != target || string(payload) != "abc" {
		t.Fatalf("bad frame out: %+v %q %v", h, payload, err)
	}
}

// TestCoalescerMergesQueuedRun proves same-tick coalescing: several frames that
// are already queued collapse into one socket write.
func TestCoalescerMergesQueuedRun(t *testing.T) {
	rec := &sinkRecorder{}
	c := newCoalescer(rec)
	m := NewMetrics()
	target := "w2:p1"
	hdr := HeaderLenFor(target)

	for i := 0; i < 8; i++ {
		buf := upstream.GetBuf(hdr + 4)
		buf.B = append(buf.B[:hdr], 'a', 'b', 'c', 'd')
		c.push(target, buf, hdr, false, time.Now())
	}
	// Drain synchronously, exactly as run() would.
	frames, ats, hdr := drainForTest(c, target)
	c.writeRun(&seqCounter{}, target, frames, ats, hdr, m)

	nFrames, _, _, last := rec.counts()
	if nFrames != 1 {
		t.Fatalf("8 queued frames produced %d writes, want 1", nFrames)
	}
	_, payload, _ := DecodeServerFrame(last)
	if string(payload) != "abcdabcdabcdabcdabcdabcdabcdabcd" {
		t.Fatalf("merged payload wrong: %q", payload)
	}
	c.close()
}

// TestBackpressureDropsAndReportsGap proves the SPEC rule: losing output under
// load is acceptable, lying about it is not.
func TestBackpressureDropsAndReportsGap(t *testing.T) {
	rec := &sinkRecorder{}
	c := newCoalescer(rec)
	target := "w2:p1"
	hdr := HeaderLenFor(target)

	chunk := 64 << 10
	for i := 0; i < (MaxQueueBytes/chunk)+4; i++ {
		buf := upstream.GetBuf(hdr + chunk)
		buf.B = buf.B[:hdr+chunk]
		c.push(target, buf, hdr, false, time.Now())
	}
	c.mu.Lock()
	p := c.pending[target]
	c.mu.Unlock()
	if p == nil || p.dropped == 0 || !p.repaint {
		t.Fatalf("overflow did not record a gap: %+v", p)
	}
	if c.bytes > MaxQueueBytes {
		t.Fatalf("queue grew past the hard cap: %d", c.bytes)
	}
	c.close()
}

func TestSeenSetIsPerConnection(t *testing.T) {
	a, b := NewSeenSet(), NewSeenSet()
	const pane = "w2:p1"
	done := map[string]int64{pane: 3}

	if !a.Unseen(pane, 3) || !b.Unseen(pane, 3) {
		t.Fatal("both connections should start unseen")
	}
	a.MarkSeen(pane, 3)
	if a.Unseen(pane, 3) {
		t.Fatal("A marked seen, should be seen")
	}
	if !b.Unseen(pane, 3) {
		t.Fatal("A marking seen must not clear B's badge")
	}
	if len(b.Decorate(done)) != 1 {
		t.Fatal("B should still have one DONE badge")
	}
}

// BenchmarkFanoutFrame is the output-path harness: decode-side buffer to a
// stamped, shared wire frame. It must not allocate in steady state.
func BenchmarkFanoutFrame(b *testing.B) {
	rec := &sinkRecorder{}
	c := newCoalescer(rec)
	m := NewMetrics()
	target := "w2:p1"
	hdr := HeaderLenFor(target)
	seq := &seqCounter{}

	// warm the pools and the double buffers
	for i := 0; i < 64; i++ {
		buf := upstream.GetBuf(hdr + 4096)
		buf.B = buf.B[:hdr+4096]
		c.push(target, buf, hdr, false, time.Now())
		frames, ats, h := drainForTest(c, target)
		c.writeRun(seq, target, frames, ats, h, m)
	}

	b.ReportAllocs()
	b.SetBytes(4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := upstream.GetBuf(hdr + 4096)
		buf.B = buf.B[:hdr+4096]
		c.push(target, buf, hdr, false, time.Now())
		frames, ats, h := drainForTest(c, target)
		c.writeRun(seq, target, frames, ats, h, m)
	}
	b.StopTimer()
	c.close()
}

// BenchmarkDecodeClientFrame is the keystroke ingress path: a 3-byte header
// parse and two subslices. No JSON, no base64, no copy.
func BenchmarkDecodeClientFrame(b *testing.B) {
	target := "w2:p1"
	buf := make([]byte, 0, ClientHeaderLen+len(target)+3)
	buf = append(buf, TypeInput, 0, byte(len(target)))
	buf = append(buf, target...)
	buf = append(buf, 0x1b, '[', 'A')

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := DecodeClientFrame(buf); err != nil {
			b.Fatal(err)
		}
	}
}
