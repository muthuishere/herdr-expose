package core

import (
	"context"
	"log/slog"
	"sync"
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
type Hub struct {
	store   *Store
	log     *slog.Logger
	summary *summaryPoller
	metrics *Metrics
}

// NewHub builds a hub over a store.
func NewHub(store *Store, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{store: store, log: log,
		summary: newSummaryPoller(store.Client(), log), metrics: NewMetrics()}
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
	closed  bool
}

type liveStream struct {
	stream *upstream.TerminalStream
	mode   upstream.TerminalMode
	hdrLen int
	target string
}

// NewSession creates a per-connection session.
func (h *Hub) NewSession(ctx context.Context, sink ClientSink) *Session {
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
			s.startStream(target, upstream.ModeObserve)
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
		_, err := s.hub.store.Client().Call(s.ctx, "pane.scroll",
			map[string]any{"pane_id": target, "delta": delta})
		return err
	}
	return ls.stream.Scroll(delta)
}

func (s *Session) startStream(target string, mode upstream.TerminalMode) (*liveStream, error) {
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
	s.streams[target] = ls
	s.mu.Unlock()

	h := &streamHandler{sess: s, target: target, hdrLen: hdr}
	ts := upstream.NewTerminalStream(target, mode, g.Cols, g.Rows, h, s.log)
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
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		text, err := s.hub.store.Client().PaneRead(ctx, target, "visible", "ansi", 0)
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
	if f.Full {
		stampHeader(f.Data.B, TypeSnapshot, h.sess.seq.next(), h.target)
		h.sess.sink.SendBinary(f.Data)
		h.sess.hub.metrics.Output.Since(t0)
		return
	}
	h.sess.co.push(h.target, f.Data, h.hdrLen, false, t0)
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
// geometry, so one poll serves every connection watching that tile.
type summaryPoller struct {
	client *upstream.Client
	log    *slog.Logger

	mu   sync.Mutex
	subs map[string]map[*Session]struct{}
	wake chan struct{}
}

// SummaryInterval is the SUMMARY tile refresh period (1-2Hz).
const SummaryInterval = 700 * time.Millisecond

func newSummaryPoller(c *upstream.Client, log *slog.Logger) *summaryPoller {
	return &summaryPoller{client: c, log: log,
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
			rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			text, err := p.client.PaneRead(rctx, target, "visible", "ansi", 0)
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
