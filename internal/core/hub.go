package core

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Hub owns everything shared between connections.
//
// Note what it deliberately does NOT share: LIVE terminal streams. Each
// connection gets its own `herdr terminal session` subprocess with its own
// geometry, because a shared stream would let a phone at 40 columns resize the
// laptop watching the same pane. Only SUMMARY reads are deduplicated, since
// pane.read is geometry-free.
//
// Every target it handles is SESSION-QUALIFIED (`<session>/<pane_id>`) and is
// routed to that session's socket. Per-connection streams and session-qualified
// targets compose: the stream key is the full target, so the same pane id in two
// sessions is two independent streams.
type Hub struct {
	store   *Store
	log     *slog.Logger
	summary *summaryPoller
	metrics *Metrics

	// clients counts live websocket connections, for /v1/metrics and for the
	// "clients" line in the log on connect/disconnect.
	clients atomic.Int64
}

// ConnectedClients is the number of live client connections.
func (h *Hub) ConnectedClients() int64 { return h.clients.Load() }

// NewHub builds a hub over a store.
func NewHub(store *Store, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{store: store, log: log,
		summary: newSummaryPoller(store, log), metrics: NewMetrics()}
}

// Metrics exposes the latency instrument.
func (h *Hub) Metrics() *Metrics { return h.metrics }

// Store exposes the tree store.
func (h *Hub) Store() *Store { return h.store }

// Run drives the shared summary poller.
func (h *Hub) Run(ctx context.Context) { h.summary.run(ctx) }

// Session is one client connection's view of the world.
type Session struct {
	hub  *Hub
	sink ClientSink
	log  *slog.Logger
	ctx  context.Context

	co   *coalescer
	seq  seqCounter
	Seen *SeenSet

	mu      sync.Mutex
	geom    map[string]Geometry
	modes   map[string]Mode
	streams map[string]*liveStream
	// lastLive is the most recent target this connection asked to render live.
	// It is what a `command` frame with no explicit session defaults to.
	lastLive string
	closed   bool
}

type liveStream struct {
	stream *upstream.TerminalStream
	mode   upstream.TerminalMode
	hdrLen int
	target string
	// needSnap means THIS connection's buffer cannot be trusted, so the next
	// full repaint must go out as a type-2 snapshot. Set on attach, after a
	// gap, and after a stream restart — never merely because Herdr sent one of
	// its periodic `full` frames. A full repaint carries its own clear/home
	// sequences, so writing it into a live buffer is seamless; marking every one
	// of them a snapshot told the client to reset several times a second, which
	// is what the flicker was.
	needSnap atomic.Bool
}

// NewSession creates a per-connection session.
func (h *Hub) NewSession(ctx context.Context, sink ClientSink) *Session {
	h.clients.Add(1)
	s := &Session{
		hub:     h,
		sink:    sink,
		log:     h.log,
		ctx:     ctx,
		Seen:    NewSeenSet(),
		geom:    map[string]Geometry{},
		modes:   map[string]Mode{},
		streams: map[string]*liveStream{},
	}
	s.co = newCoalescer(sink)
	s.co.started = true
	go s.co.run(&s.seq, s.requestRepaint, h.metrics)
	return s
}

// SetGeometry records this connection's size for a target and resizes an
// already-running stream. Geometry must be set before the first LIVE frame.
func (s *Session) SetGeometry(target string, cols, rows int) {
	g := Geometry{Cols: cols, Rows: rows}.Clamp()
	s.mu.Lock()
	prev := s.geom[target]
	s.geom[target] = g
	ls := s.streams[target]
	mode := s.modes[target]
	s.mu.Unlock()

	if ls != nil && prev != g {
		if err := ls.stream.Resize(g.Cols, g.Rows); err != nil {
			// Observers have no stdin: restart at the new size instead.
			s.stopStream(target)
			if mode == ModeLive {
				s.startStream(target, upstream.ModeObserve)
			}
		}
	}
}

// Geometry returns the connection's geometry for a target.
func (s *Session) Geometry(target string) Geometry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.geom[target]
}

// FocusedSession is the Herdr session this connection is currently working in:
// the session of the last target it rendered live, else the store's default.
func (s *Session) FocusedSession() string {
	s.mu.Lock()
	last := s.lastLive
	s.mu.Unlock()
	if name := SessionOf(last); name != "" {
		return name
	}
	return s.hub.store.DefaultSession()
}

// SetViewport applies a client's declared render modes. The server decides what
// it actually sends; clients never request LIVE directly, they declare that they
// are rendering a full terminal and the server grants it.
func (s *Session) SetViewport(decl map[string]string) {
	want := make(map[string]Mode, len(decl))
	for target, m := range decl {
		want[target] = ParseMode(m)
	}

	s.mu.Lock()
	old := s.modes
	s.modes = want
	geoms := make(map[string]Geometry, len(s.geom))
	for k, v := range s.geom {
		geoms[k] = v
	}
	s.mu.Unlock()

	for target, mode := range want {
		if old[target] == mode {
			continue
		}
		switch mode {
		case ModeLive:
			if !geoms[target].Valid() {
				s.sink.SendJSON("error", map[string]any{
					"target": target,
					"code":   "no_geometry",
					"detail": "send a resize message for this target before requesting live",
				})
				continue
			}
			s.hub.summary.unsubscribe(target, s)
			// lastLive is only updated once the target actually attached: it
			// is what a `command` with no explicit session defaults to, and a
			// REFUSED target (out of scope, or gone) must not become this
			// connection's implicit session.
			if _, err := s.startStream(target, upstream.ModeObserve); err == nil {
				s.mu.Lock()
				s.lastLive = target
				s.mu.Unlock()
			}
		case ModeSummary:
			s.stopStream(target)
			s.hub.summary.subscribe(target, s)
		default:
			s.stopStream(target)
			s.hub.summary.unsubscribe(target, s)
		}
	}
	for target := range old {
		if _, still := want[target]; !still {
			s.stopStream(target)
			s.hub.summary.unsubscribe(target, s)
		}
	}
}

// Input writes raw bytes to a target. This is the keystroke path: no JSON
// decode of the payload, no re-encode, no accumulation timer.
func (s *Session) Input(target string, data []byte) error {
	ls, err := s.ensureControl(target)
	if err != nil {
		return err
	}
	return ls.stream.SendInput(data)
}

// Metrics is the shared latency instrument.
func (s *Session) Metrics() *Metrics { return s.hub.metrics }

// ensureControl upgrades a target's stream from observe to control on demand.
// Herdr allows one controller and unlimited observers; if the upgrade fails the
// caller gets the upstream error and the observe stream keeps running, which is
// exactly the one-controller/many-observer semantics we want to preserve.
func (s *Session) ensureControl(target string) (*liveStream, error) {
	s.mu.Lock()
	ls := s.streams[target]
	if ls != nil && ls.mode == upstream.ModeControl {
		s.mu.Unlock()
		return ls, nil
	}
	s.mu.Unlock()
	// Tear the observer down and WAIT for it: Herdr permits one attached client
	// per terminal, so a controller started before the observer has exited is
	// rejected with "already has an attached client". Takeover is also set, so
	// even a lost race resolves in our favour rather than wedging the pane.
	s.stopStream(target)
	return s.startStream(target, upstream.ModeControl)
}

// Scroll moves a target's viewport upstream.
func (s *Session) Scroll(target string, delta int) error {
	s.mu.Lock()
	ls := s.streams[target]
	s.mu.Unlock()
	if ls == nil {
		return nil
	}
	if ls.mode != upstream.ModeControl {
		_, c, id, err := s.hub.store.Resolve(target)
		if err != nil {
			return err
		}
		_, err = c.Call(s.ctx, "pane.scroll", map[string]any{"pane_id": id, "delta": delta})
		return err
	}
	return ls.stream.Scroll(delta)
}

func (s *Session) startStream(target string, mode upstream.TerminalMode) (*liveStream, error) {
	// Route by session BEFORE spawning: the herdr CLI selects its server from
	// $HERDR_SOCKET_PATH, and a bare pane id would otherwise resolve against
	// whichever session launched us.
	_, _, paneID, err := s.hub.store.Resolve(target)
	if err != nil {
		s.sink.SendJSON("closed", map[string]any{"target": target, "reason": err.Error()})
		return nil, err
	}
	socket := s.hub.store.Socket(SessionOf(target))
	if socket == "" {
		socket = s.hub.store.Socket(s.hub.store.DefaultSession())
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, context.Canceled
	}
	if ls := s.streams[target]; ls != nil {
		s.mu.Unlock()
		return ls, nil
	}
	g := s.geom[target].Clamp()
	hdr := HeaderLenFor(target)
	ls := &liveStream{mode: mode, hdrLen: hdr, target: target}
	// Fresh attachment (or a restart after a takeover): this connection has no
	// trustworthy buffer for the target, so the next full repaint is a snapshot.
	ls.needSnap.Store(true)
	s.streams[target] = ls
	s.mu.Unlock()

	h := &streamHandler{sess: s, target: target, hdrLen: hdr, ls: ls}
	ts := upstream.NewTerminalStream(paneID, mode, g.Cols, g.Rows, h, s.log)
	ts.Socket = socket
	ts.Reserve = hdr
	ts.Takeover = mode == upstream.ModeControl
	ls.stream = ts
	if err := ts.Start(s.ctx); err != nil {
		s.mu.Lock()
		delete(s.streams, target)
		s.mu.Unlock()
		s.sink.SendJSON("closed", map[string]any{"target": target, "reason": err.Error()})
		return nil, err
	}
	return ls, nil
}

func (s *Session) stopStream(target string) {
	s.mu.Lock()
	ls := s.streams[target]
	delete(s.streams, target)
	s.mu.Unlock()
	if ls != nil && ls.stream != nil {
		ls.stream.Stop()
		ls.stream.Wait(2 * time.Second)
	}
}

// requestRepaint fetches a fresh full repaint after a gap. Resume/replay was
// deleted on purpose: repainting is cheaper and cannot be subtly wrong.
func (s *Session) requestRepaint(target string) {
	// Bytes were actually dropped, so this connection's buffer is now
	// untrustworthy: the next full repaint must also be flagged as a snapshot.
	s.mu.Lock()
	ls := s.streams[target]
	s.mu.Unlock()
	if ls != nil {
		ls.needSnap.Store(true)
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		_, c, id, err := s.hub.store.Resolve(target)
		if err != nil {
			return
		}
		text, err := c.PaneRead(ctx, id, "visible", "ansi", 0)
		if err != nil {
			s.log.Debug("repaint read failed", "target", target, "err", err)
			return
		}
		s.sink.SendBinary(encodeFrame(TypeSnapshot, s.seq.next(), target, []byte(text)))
	}()
}

// Close tears down every stream owned by this connection.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.hub.clients.Add(-1)
	streams := s.streams
	s.streams = map[string]*liveStream{}
	s.mu.Unlock()
	for _, ls := range streams {
		if ls.stream != nil {
			ls.stream.Stop()
		}
	}
	s.hub.summary.unsubscribeAll(s)
	s.co.close()
}

// streamHandler bridges an upstream terminal stream into this session's
// coalescer. It runs on the subprocess reader goroutine and must never block.
type streamHandler struct {
	sess   *Session
	target string
	hdrLen int
	ls     *liveStream
}

func (h *streamHandler) OnFrame(f upstream.TerminalFrame) {
	t0 := time.Now()
	h.sess.hub.metrics.CountFrame(f.Data.Len() - f.Off)
	if f.Off != h.hdrLen {
		// Defensive: reservation mismatch means we cannot stamp in place.
		defer f.Data.Release()
		h.sess.sink.SendBinary(encodeFrame(TypeFrame, h.sess.seq.next(), h.target, f.Data.B[f.Off:]))
		return
	}
	// type 2 means "you cannot trust your buffer, reset and repaint". That is
	// true only when this CONNECTION just attached, lost bytes to a gap, or had
	// its stream restarted — NOT on Herdr's periodic `full` repaints, which are
	// self-contained and paint cleanly into a live buffer.
	if f.Full && h.ls != nil && h.ls.needSnap.CompareAndSwap(true, false) {
		stampHeader(f.Data.B, TypeSnapshot, h.sess.seq.next(), h.target)
		h.sess.sink.SendBinary(f.Data)
		h.sess.hub.metrics.Output.Since(t0)
		return
	}
	// A full repaint still SUPERSEDES anything buffered for this target: there
	// is no point writing deltas the repaint is about to overwrite.
	h.sess.co.push(h.target, f.Data, h.hdrLen, f.Full, t0)
}

func (h *streamHandler) OnClosed(reason string) {
	h.sess.mu.Lock()
	delete(h.sess.streams, h.target)
	closed := h.sess.closed
	h.sess.mu.Unlock()
	if closed {
		return
	}
	h.sess.sink.SendJSON("closed", map[string]any{"target": h.target, "reason": reason})
}

// summaryPoller deduplicates SUMMARY reads across connections. pane.read has no
// geometry, so one poll serves every connection watching that tile. Targets are
// session-qualified and each poll is routed to its own session's socket.
type summaryPoller struct {
	store *Store
	log   *slog.Logger

	mu   sync.Mutex
	subs map[string]map[*Session]struct{}
	wake chan struct{}
}

// SummaryInterval is the SUMMARY tile refresh period (1-2Hz).
const SummaryInterval = 700 * time.Millisecond

func newSummaryPoller(store *Store, log *slog.Logger) *summaryPoller {
	return &summaryPoller{store: store, log: log,
		subs: map[string]map[*Session]struct{}{}, wake: make(chan struct{}, 1)}
}

func (p *summaryPoller) subscribe(target string, s *Session) {
	p.mu.Lock()
	m := p.subs[target]
	if m == nil {
		m = map[*Session]struct{}{}
		p.subs[target] = m
	}
	m[s] = struct{}{}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *summaryPoller) unsubscribe(target string, s *Session) {
	p.mu.Lock()
	if m := p.subs[target]; m != nil {
		delete(m, s)
		if len(m) == 0 {
			delete(p.subs, target)
		}
	}
	p.mu.Unlock()
}

func (p *summaryPoller) unsubscribeAll(s *Session) {
	p.mu.Lock()
	for target, m := range p.subs {
		delete(m, s)
		if len(m) == 0 {
			delete(p.subs, target)
		}
	}
	p.mu.Unlock()
}

func (p *summaryPoller) run(ctx context.Context) {
	t := time.NewTicker(SummaryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-t.C:
		}
		p.mu.Lock()
		targets := make([]string, 0, len(p.subs))
		for target := range p.subs {
			targets = append(targets, target)
		}
		p.mu.Unlock()

		for _, target := range targets {
			_, c, id, err := p.store.Resolve(target)
			if err != nil {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			text, err := c.PaneRead(rctx, id, "visible", "ansi", 0)
			cancel()
			if err != nil {
				continue
			}
			p.mu.Lock()
			sessions := make([]*Session, 0, len(p.subs[target]))
			for s := range p.subs[target] {
				sessions = append(sessions, s)
			}
			p.mu.Unlock()
			if len(sessions) == 0 {
				continue
			}
			// One encode, shared by every subscriber: no per-client copy.
			for _, s := range sessions {
				s.sink.SendBinary(encodeFrame(TypeSnapshot, s.seq.next(), target, []byte(text)))
			}
		}
	}
}
