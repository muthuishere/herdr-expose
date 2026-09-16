package core

import (
	"sync"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Mode is what the server has decided to send for a target.
type Mode string

const (
	// ModeLive is the focused pane: its own upstream stream, full output.
	ModeLive Mode = "live"
	// ModeSummary is visible-but-unfocused: a deduplicated pane.read poll.
	ModeSummary Mode = "summary"
	// ModeNone is offscreen: state changes only, no output at all.
	ModeNone Mode = "none"
)

// ParseMode maps a client-declared render mode onto a server mode.
// Clients declare what they RENDER; the server decides what it SENDS.
func ParseMode(s string) Mode {
	switch s {
	case "live":
		return ModeLive
	case "summary":
		return ModeSummary
	default:
		return ModeNone
	}
}

// Geometry floors. A terminal smaller than this is not usable and Herdr will
// reflow real agent output into nonsense.
const (
	MinCols = 20
	MinRows = 6
)

// Geometry is a per-connection terminal size. It is per-connection and never
// shared: a phone at 40 columns must not resize the laptop looking at the same
// pane (herdr 0.9.0 #3526).
type Geometry struct {
	Cols int
	Rows int
}

// Clamp applies the floors.
func (g Geometry) Clamp() Geometry {
	if g.Cols < MinCols {
		g.Cols = MinCols
	}
	if g.Rows < MinRows {
		g.Rows = MinRows
	}
	return g
}

// Valid reports whether geometry was ever set.
func (g Geometry) Valid() bool { return g.Cols > 0 && g.Rows > 0 }

// Output-plane tuning.
const (
	// MaxWriteBytes is the hard flush size for one coalesced socket write.
	MaxWriteBytes = 64 << 10
	// MaxQueueBytes is the per-connection hard cap. Past it we drop a target's
	// buffered output, report the gap, and repaint. We never grow the buffer
	// and never block the hub.
	MaxQueueBytes = 4 << 20
)

// ClientSink is what a connection must implement to receive output.
// SendBinary takes ownership of exactly one reference on b.
type ClientSink interface {
	SendBinary(b *upstream.Buf)
	SendJSON(typ string, data any)
}

// pendingTarget is the not-yet-written output for one target.
//
// frames/at are DOUBLE BUFFERED against spareFrames/spareAt. A drain swaps the
// two and hands the full one to the writer; the producer keeps appending to the
// (already allocated) spare. After warmup neither side ever grows a slice, so a
// terminal frame costs no allocation anywhere on the fanout path.
type pendingTarget struct {
	frames      []*upstream.Buf
	at          []time.Time
	spareFrames []*upstream.Buf
	spareAt     []time.Time
	bytes       int
	dropped     uint64
	repaint     bool
	queued      bool
	hdrLen      int
}

// coalescer batches output per connection.
//
// It coalesces SAME-TICK only: the writer drains whatever is already queued and
// writes once. It never sleeps to accumulate, because a 16ms accumulation timer
// would add up to 16ms to every keystroke echo and blow the <5ms budget.
type coalescer struct {
	sink ClientSink

	mu      sync.Mutex
	pending map[string]*pendingTarget
	order   []string
	bytes   int
	closed  bool

	sig     chan struct{}
	done    chan struct{}
	started bool
}

func newCoalescer(sink ClientSink) *coalescer {
	return &coalescer{
		sink:    sink,
		pending: make(map[string]*pendingTarget),
		sig:     make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// push queues one decoded frame. It takes ownership of buf's reference and
// never blocks: on overflow it drops that target's buffer and records the gap.
func (c *coalescer) push(target string, buf *upstream.Buf, hdrLen int, full bool, at time.Time) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		buf.Release()
		return
	}
	p := c.pending[target]
	if p == nil {
		p = &pendingTarget{hdrLen: hdrLen}
		c.pending[target] = p
	}
	if !p.queued {
		p.queued = true
		c.order = append(c.order, target)
	}
	if full {
		// A full repaint supersedes everything buffered for this target.
		for _, f := range p.frames {
			c.bytes -= f.Len()
			f.Release()
		}
		p.frames = p.frames[:0]
		p.at = p.at[:0]
		p.bytes = 0
		p.dropped = 0
		p.repaint = false
	}
	if c.bytes+buf.Len() > MaxQueueBytes {
		// Backpressure: the client cannot keep up. Losing output is acceptable;
		// lying about it is not.
		for _, f := range p.frames {
			c.bytes -= f.Len()
			p.dropped += uint64(f.Len() - p.hdrLen)
			f.Release()
		}
		p.frames = p.frames[:0]
		p.at = p.at[:0]
		p.bytes = 0
		p.dropped += uint64(buf.Len() - hdrLen)
		p.repaint = true
		buf.Release()
		c.mu.Unlock()
		c.wake()
		return
	}
	p.frames = append(p.frames, buf)
	p.at = append(p.at, at)
	p.bytes += buf.Len()
	c.bytes += buf.Len()
	c.mu.Unlock()
	c.wake()
}

// markGap records a synthetic gap (e.g. an upstream stream restart).
func (c *coalescer) markGap(target string, dropped uint64, hdrLen int) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	p := c.pending[target]
	if p == nil {
		p = &pendingTarget{hdrLen: hdrLen}
		c.pending[target] = p
	}
	if !p.queued {
		p.queued = true
		c.order = append(c.order, target)
	}
	p.dropped += dropped
	p.repaint = true
	c.mu.Unlock()
	c.wake()
}

func (c *coalescer) wake() {
	select {
	case c.sig <- struct{}{}:
	default:
	}
}

// gapReporter is called when a target needs a repaint after a gap.
type gapReporter func(target string)

// run is the single writer for this connection.
func (c *coalescer) run(seq *seqCounter, repaint gapReporter, m *Metrics) {
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
	defer close(c.done)

	// Reused across drains so the drain loop itself never allocates.
	var order []string
	for range c.sig {
		for {
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			if len(c.order) == 0 {
				c.mu.Unlock()
				break
			}
			order = append(order[:0], c.order...)
			c.order = c.order[:0]
			c.bytes = 0
			for _, target := range order {
				p := c.pending[target]
				if p == nil {
					continue
				}
				// Swap the producer's buffers for the spares. The writer then
				// owns the full slices until the next drain, which is this same
				// goroutine, so no copy and no race.
				p.frames, p.spareFrames = p.spareFrames[:0], p.frames
				p.at, p.spareAt = p.spareAt[:0], p.at
				p.bytes = 0
				p.queued = false
			}
			c.mu.Unlock()

			for _, target := range order {
				c.mu.Lock()
				p := c.pending[target]
				var (
					frames  []*upstream.Buf
					ats     []time.Time
					dropped uint64
					needRe  bool
					hdrLen  int
				)
				if p != nil {
					frames, ats = p.spareFrames, p.spareAt
					dropped, needRe, hdrLen = p.dropped, p.repaint, p.hdrLen
					p.dropped, p.repaint = 0, false
				}
				c.mu.Unlock()
				if p == nil {
					continue
				}
				if dropped > 0 {
					c.sink.SendBinary(encodeGap(seq.next(), target, dropped))
				}
				c.writeRun(seq, target, frames, ats, hdrLen, m)
				if needRe && repaint != nil {
					repaint(target)
				}
			}
		}
	}
}

// writeRun emits a target's queued payloads, coalescing adjacent frames into at
// most MaxWriteBytes per socket write.
//
// The single-frame case is the common one and is ZERO COPY: the payload was
// base64-decoded into a pooled buffer that already reserved header space, so we
// only stamp the header in place before handing the same bytes to the socket.
func (c *coalescer) writeRun(seq *seqCounter, target string, frames []*upstream.Buf, ats []time.Time, hdrLen int, m *Metrics) {
	i := 0
	for i < len(frames) {
		f := frames[i]
		if i == len(frames)-1 || f.Len()-hdrLen >= MaxWriteBytes {
			stampHeader(f.B, TypeFrame, seq.next(), target)
			c.sink.SendBinary(f)
			m.Output.Since(ats[i])
			frames[i] = nil
			i++
			continue
		}
		total := f.Len() - hdrLen
		j := i + 1
		for j < len(frames) && total+(frames[j].Len()-hdrLen) <= MaxWriteBytes {
			total += frames[j].Len() - hdrLen
			j++
		}
		if j == i+1 {
			stampHeader(f.B, TypeFrame, seq.next(), target)
			c.sink.SendBinary(f)
			m.Output.Since(ats[i])
			frames[i] = nil
			i++
			continue
		}
		out := upstream.GetBuf(hdrLen + total)
		out.B = out.B[:hdrLen]
		for k := i; k < j; k++ {
			out.B = append(out.B, frames[k].B[hdrLen:]...)
			frames[k].Release()
			frames[k] = nil
		}
		stampHeader(out.B, TypeFrame, seq.next(), target)
		c.sink.SendBinary(out)
		m.Output.Since(ats[i])
		i = j
	}
}

func (c *coalescer) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for _, p := range c.pending {
		for _, f := range p.frames {
			if f != nil {
				f.Release()
			}
		}
		for _, f := range p.spareFrames {
			if f != nil {
				f.Release()
			}
		}
	}
	c.pending = nil
	c.order = nil
	started := c.started
	close(c.sig)
	c.mu.Unlock()
	if started {
		<-c.done
	}
}

// seqCounter hands out monotonic data-plane sequence numbers.
// Resume was deleted, so seq exists purely for ordering and debugging.
type seqCounter struct {
	mu sync.Mutex
	n  uint64
}

func (s *seqCounter) next() uint64 {
	s.mu.Lock()
	s.n++
	v := s.n
	s.mu.Unlock()
	return v
}
