package core

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// ONE observe subprocess per target, fanned out — hard rule #3, restored.
//
// SPEC B1 replaced hard rule #3 with a stream per CONNECTION, and its whole
// justification was geometry: "a shared stream means a phone at 40 cols resizes
// the laptop looking at the same pane". AMENDMENTS 14 / K4 then withdrew
// geometry from the attach path entirely — a LIVE attach passes no
// --cols/--rows at all, and the e2e suite proves resizeSent=0. With no geometry
// on the wire there is nothing per-connection about the SUBPROCESS any more,
// and paying for one per viewer is paying for nothing.
//
// MEASURED on an isolated 12-session bed, 16 clients watching ONE pane:
// 16 subprocesses, 138.4MB of subprocess RSS, 209.5MB total.
//
// What stays per-connection, because it is genuinely per-connection:
//   - seq (Session.seq), so every client's data plane is its own sequence;
//   - needSnap and the snapshot-on-attach, so a client that joins a stream
//     that has been running for an hour still gets a full screen of its own;
//   - the coalescer and its backpressure, so one stalled phone cannot stall
//     anyone else;
//   - SeenSet / DONE badges (SPEC B-seen), which never leave the connection.
//
// A target a client has EXPLICITLY sized is not shared: SetGeometry marks it
// `explicit`, and an explicit target gets its own private stream at its own
// size, exactly as before. So does every CONTROL stream — there is one
// controller by construction.
type sharedStream struct {
	hub    *Hub
	target string
	hdrLen int
	stream *upstream.TerminalStream

	// stopped marks a DELIBERATE teardown (the last subscriber left), so
	// OnClosed can tell "we killed it" from "it died".
	stopped   atomic.Bool
	startedAt time.Time

	mu sync.Mutex
	// dead is set once this stream can take no further subscribers. A joiner
	// that loses the race with a teardown is told so and creates a fresh one,
	// rather than attaching to a subprocess that is already exiting.
	dead bool
	subs map[*Session]*liveStream
	// geom is the size herdr reported for the pane. A late joiner is told it
	// immediately, because it will not see an attach frame of its own.
	geom Geometry
}

type sharedSub struct {
	sess *Session
	ls   *liveStream
}

// attach adds a subscriber. It reports false when this stream is already on its
// way out, which means the caller must start a new one.
func (ss *sharedStream) attach(s *Session, ls *liveStream) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.dead {
		return false
	}
	ss.subs[s] = ls
	return true
}

// detach removes a subscriber and, when it was the last one, tears the
// subprocess down. An empty room must generate no upstream traffic.
func (ss *sharedStream) detach(s *Session) {
	ss.mu.Lock()
	delete(ss.subs, s)
	last := len(ss.subs) == 0
	if last {
		ss.dead = true
	}
	ss.mu.Unlock()
	if !last {
		return
	}
	ss.hub.dropShared(ss)
	ss.stopped.Store(true)
	if ss.stream != nil {
		ss.stream.Stop()
		ss.stream.Wait(2 * time.Second)
	}
}

// geometry is the size herdr reported, or the zero Geometry before the first
// frame has arrived.
func (ss *sharedStream) geometry() Geometry {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.geom
}

// OnFrame fans one upstream frame out to every subscriber.
//
// The LAST subscriber is handed the pooled buffer the decoder produced; the
// others get a copy, because each one stamps its OWN seq into the header in
// place. One viewer — the overwhelmingly common case — therefore still costs
// zero copies, exactly as the per-connection stream did.
func (ss *sharedStream) OnFrame(f upstream.TerminalFrame) {
	t0 := time.Now()
	g := Geometry{Cols: f.Width, Rows: f.Height}
	ss.mu.Lock()
	if g.Valid() {
		ss.geom = g
	}
	subs := make([]sharedSub, 0, len(ss.subs))
	for sess, ls := range ss.subs {
		subs = append(subs, sharedSub{sess: sess, ls: ls})
	}
	ss.mu.Unlock()

	if len(subs) == 0 {
		f.Data.Release()
		return
	}
	// A stream that has stayed up counts as recovered, once, for the stream —
	// each connection then forgets its own restart history.
	for i, sub := range subs {
		if g.Valid() {
			sub.sess.noteAttached(ss.target, g)
		}
		buf := f.Data
		if i < len(subs)-1 {
			buf = upstream.BufOf(f.Data.B)
		}
		sub.sess.deliverFrame(ss.target, sub.ls, buf, f.Off, ss.hdrLen, f.Full, t0)
	}
}

// OnClosed retires the stream and tells every subscriber, each on its own
// terms: a deliberate teardown is silent, an upstream death is supervised.
func (ss *sharedStream) OnClosed(reason string) {
	ss.hub.dropShared(ss)
	ss.mu.Lock()
	ss.dead = true
	subs := make([]sharedSub, 0, len(ss.subs))
	for sess, ls := range ss.subs {
		subs = append(subs, sharedSub{sess: sess, ls: ls})
	}
	ss.subs = map[*Session]*liveStream{}
	ss.mu.Unlock()

	deliberate := ss.stopped.Load()
	for _, sub := range subs {
		sub.sess.onSharedClosed(ss.target, sub.ls, reason, deliberate)
	}
}

// dropShared forgets a stream, but only if it is still the CURRENT one for its
// target: a restart may already have installed its successor.
func (h *Hub) dropShared(ss *sharedStream) {
	h.sharedMu.Lock()
	if h.shared[ss.target] == ss {
		delete(h.shared, ss.target)
	}
	h.sharedMu.Unlock()
}

// attachShared joins a connection to the one observe stream for `target`,
// starting it when nobody has yet.
//
// joined is true when the stream was ALREADY running, which means herdr will
// send this connection no attach frame and the caller owes it a snapshot and a
// geometry frame of its own.
//
// The subprocess is started on the HUB's context, never on the first
// subscriber's: the stream outlives whoever happened to open it first, and is
// torn down by refcount in detach instead.
func (h *Hub) attachShared(s *Session, target, paneID, socket string, hdr int, ls *liveStream) (*sharedStream, bool, error) {
	h.sharedMu.Lock()
	defer h.sharedMu.Unlock()
	if cur := h.shared[target]; cur != nil && cur.attach(s, ls) {
		return cur, true, nil
	}
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ss := &sharedStream{
		hub: h, target: target, hdrLen: hdr, startedAt: time.Now(),
		subs: map[*Session]*liveStream{s: ls},
	}
	// NO --cols/--rows, by construction: this stream only ever serves targets
	// nobody has explicitly sized, so there is no size to impose and none can
	// be introduced later without leaving the shared path (see SetGeometry).
	ts := upstream.NewTerminalStream(paneID, upstream.ModeObserve, 0, 0, ss, h.log)
	ts.Socket = socket
	ts.Reserve = hdr
	ss.stream = ts
	if err := ts.Start(ctx); err != nil {
		return nil, false, err
	}
	h.shared[target] = ss
	return ss, false, nil
}

// SharedStreams is how many upstream observe subprocesses the hub is running.
// It is published on /v1/metrics: "one stream per target" is a claim, and a
// claim about load has to be measurable from outside.
func (h *Hub) SharedStreams() int {
	h.sharedMu.Lock()
	defer h.sharedMu.Unlock()
	return len(h.shared)
}

// onSharedClosed is a connection's half of a shared stream going away. It
// mirrors streamHandler.OnClosed exactly, because the client must not be able
// to tell which kind of stream it was watching.
func (s *Session) onSharedClosed(target string, ls *liveStream, reason string, deliberate bool) {
	s.mu.Lock()
	// Only retire OURSELVES: a supervised restart may already have installed a
	// newer stream for this target on this connection.
	if cur := s.streams[target]; cur == ls {
		delete(s.streams, target)
	}
	closed := s.closed
	mode := s.modes[target]
	s.mu.Unlock()
	if closed {
		return
	}
	if deliberate || (ls != nil && ls.stopped.Load()) {
		return
	}
	if mode == ModeLive {
		s.superviseRestart(target, reason)
		return
	}
	s.sink.SendJSON("closed", map[string]any{"target": target, "reason": reason})
}
