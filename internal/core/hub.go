package core

import (
	"context"
	"log/slog"
	"strconv"
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

// Upstream supervision (SPEC B7: supervision is a layer, not a hope).
//
// A LIVE target whose `herdr terminal session observe` subprocess exits is the
// flagship failure: the pane is still there, the websocket is still open, and
// the client sits forever believing it is watching a terminal that stopped
// existing. Restart it, tell the client to repaint, and if it truly cannot come
// back say so with a `closed` — never go silent.
// They are vars, not consts, only so tests can shrink them; nothing at runtime
// writes them.
var (
	// RestartBackoffMin is the delay before the first supervised restart.
	RestartBackoffMin = 250 * time.Millisecond
	// RestartBackoffMax caps the exponential backoff.
	RestartBackoffMax = 10 * time.Second
	// MaxRestarts is how many consecutive failures we tolerate before telling
	// the client the target is gone.
	MaxRestarts = 8
	// StreamHealthyAfter is how long a restarted stream must survive before the
	// backoff counter is forgiven.
	StreamHealthyAfter = 30 * time.Second
)

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
	// restarting/restartCount drive supervised upstream restarts (see
	// superviseRestart). Both are guarded by mu.
	restarting   map[string]bool
	restartCount map[string]int
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
	// stopped marks a DELIBERATE teardown (viewport change, geometry restart,
	// control upgrade, connection close). Without it the supervisor cannot tell
	// "we killed it" from "it died", and every normal viewport change would
	// look like an upstream failure worth restarting.
	stopped atomic.Bool
	// startedAt/healthy drive the supervisor's backoff reset: a stream only
	// counts as recovered once it has stayed up for StreamHealthyAfter, not
	// merely because it emitted its attach snapshot before dying again.
	startedAt time.Time
	healthy   atomic.Bool
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

	if ls == nil || prev == g {
		return
	}
	if ls.mode == upstream.ModeControl {
		if err := ls.stream.Resize(g.Cols, g.Rows); err == nil {
			return
		}
	}
	// `herdr terminal session observe` takes no stdin — verified on 0.9.0: a
	// terminal.resize written to an observer is ignored, and the CLI exposes no
	// other way to change geometry. So an observer is restarted at the new size.
	// stopStream marks the teardown deliberate, so the supervisor does not see
	// a failure and the client is not told the target closed; needSnap on the
	// fresh stream turns its first full frame into a snapshot, so the repaint
	// is seamless.
	s.stopStream(target)
	s.mu.Lock()
	closed := s.closed
	mode = s.modes[target]
	s.mu.Unlock()
	if !closed && mode == ModeLive {
		if _, err := s.startStream(target, upstream.ModeObserve); err != nil {
			s.log.Warn("resize restart failed", "target", target, "err", err)
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

// needsReconcile reports whether a target whose declared mode did NOT change
// still needs work. Viewport is a RECONCILIATION, not a diff: a target can be
// ModeLive in `modes` with no entry in `streams` because its upstream
// subprocess died, and a client re-sending the same viewport is precisely how
// it asks us to fix that. Treating an unchanged mode as a no-op made that state
// unrecoverable for the life of the connection.
func (s *Session) needsReconcile(target string, mode Mode) bool {
	if mode != ModeLive {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, running := s.streams[target]
	return !running
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
		if old[target] == mode && !s.needsReconcile(target, mode) {
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
	ls := &liveStream{mode: mode, hdrLen: hdr, target: target, startedAt: time.Now()}
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
	if ls == nil {
		return
	}
	// Mark BEFORE killing: OnClosed runs on the subprocess reader goroutine and
	// must be able to tell a deliberate teardown from an upstream death.
	ls.stopped.Store(true)
	if ls.stream != nil {
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
		ls.stopped.Store(true)
		if ls.stream != nil {
			ls.stream.Stop()
		}
	}
	s.hub.summary.unsubscribeAll(s)
	s.co.close()
}

// clearRestarts forgets a target's restart history after a successful frame.
func (s *Session) clearRestarts(target string) {
	s.mu.Lock()
	if len(s.restartCount) > 0 {
		delete(s.restartCount, target)
	}
	s.mu.Unlock()
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
	// A stream that has stayed up counts as recovered: forget the backoff
	// history so a target that dies again hours later restarts promptly. The
	// check is a wall-clock compare and an atomic load, so it costs nothing on
	// the per-frame hot path.
	if h.ls != nil && !h.ls.healthy.Load() && !h.ls.startedAt.IsZero() &&
		time.Since(h.ls.startedAt) >= StreamHealthyAfter && h.ls.healthy.CompareAndSwap(false, true) {
		h.sess.clearRestarts(h.target)
	}
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
	s := h.sess
	s.mu.Lock()
	// Only retire OURSELVES. A supervised restart may already have installed a
	// newer stream for this target; deleting it here would orphan a live
	// subprocess and lose the client's output for good.
	if cur := s.streams[h.target]; cur == h.ls {
		delete(s.streams, h.target)
	}
	closed := s.closed
	mode := s.modes[h.target]
	s.mu.Unlock()
	if closed {
		return
	}
	if h.ls != nil && h.ls.stopped.Load() {
		// We asked for this (viewport change, resize restart, control upgrade).
		// It is not a failure and the client must not be told the pane died.
		return
	}
	if mode == ModeLive {
		// The target is still supposed to be LIVE, so this is an upstream
		// failure, not a client decision. Restart it.
		s.superviseRestart(h.target, reason)
		return
	}
	s.sink.SendJSON("closed", map[string]any{"target": h.target, "reason": reason})
}

// superviseRestart brings a LIVE target's upstream stream back after it died,
// with exponential backoff, and tells the client to repaint. If it cannot come
// back it emits `closed` with a real reason: a client is NEVER left believing
// it is live on a dead target.
//
// The backoff counter deliberately survives a successful restart and is only
// cleared once a stream has stayed up for StreamHealthyAfter. Otherwise a
// stream that dies five seconds after every attach would restart at the minimum
// delay forever, and 26 targets doing that is a subprocess storm, not recovery.
func (s *Session) superviseRestart(target, reason string) {
	s.mu.Lock()
	if s.closed || s.modes[target] != ModeLive {
		s.mu.Unlock()
		return
	}
	if _, running := s.streams[target]; running {
		s.mu.Unlock()
		return
	}
	if s.restarting == nil {
		s.restarting = map[string]bool{}
	}
	if s.restarting[target] {
		s.mu.Unlock()
		return
	}
	s.restarting[target] = true
	prior := s.restartCount[target]
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.restarting, target)
			s.mu.Unlock()
		}()
		lastErr := reason
		for attempt := prior + 1; attempt <= MaxRestarts; attempt++ {
			backoff := backoffFor(attempt)
			s.log.Warn("upstream stream died; restarting", "target", target,
				"reason", lastErr, "attempt", attempt, "in", backoff)
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(backoff):
			}

			s.mu.Lock()
			if s.closed || s.modes[target] != ModeLive {
				s.mu.Unlock()
				return
			}
			if _, running := s.streams[target]; running {
				s.mu.Unlock()
				return
			}
			if s.restartCount == nil {
				s.restartCount = map[string]int{}
			}
			s.restartCount[target] = attempt
			s.mu.Unlock()

			ls, err := s.startStream(target, upstream.ModeObserve)
			if err != nil {
				// startStream already told the client with `closed`; keep
				// trying rather than going quiet.
				lastErr = err.Error()
				continue
			}
			_ = ls
			// The client's buffer describes a stream that no longer exists.
			// Say so with a gap, then repaint. startStream has already set
			// needSnap, so Herdr's own next full frame lands as a snapshot too.
			s.sink.SendBinary(encodeGap(s.seq.next(), target, 0))
			s.requestRepaint(target)
			return
		}
		s.mu.Lock()
		delete(s.restartCount, target)
		s.mu.Unlock()
		s.sink.SendJSON("closed", map[string]any{"target": target,
			"reason": "upstream stream could not be kept alive after " +
				strconv.Itoa(MaxRestarts) + " restarts: " + lastErr})
	}()
}

// backoffFor is exponential from RestartBackoffMin, capped at RestartBackoffMax.
func backoffFor(attempt int) time.Duration {
	d := RestartBackoffMin
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= RestartBackoffMax {
			return RestartBackoffMax
		}
	}
	return d
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
