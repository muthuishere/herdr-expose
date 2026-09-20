package core

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Hub owns everything shared between connections.
//
// LIVE streams ARE shared, for every target nobody has explicitly sized — hard
// rule #3, restored now that AMENDMENTS 14 / K4 has taken geometry off the
// attach path and removed B1's only justification for a stream per connection.
// See shared.go for the measurement and for what stays per-connection (seq,
// snapshot-on-attach, coalescer, seen). A target a client has fitted to its own
// window with `resize` still gets its own private stream at its own size, and
// so does every CONTROL stream.
//
// Every target it handles is SESSION-QUALIFIED (`<session>/<pane_id>`) and is
// routed to that session's socket. Shared streams and session-qualified targets
// compose: the stream key is the full target, so the same pane id in two
// sessions is two independent streams.
type Hub struct {
	store      *Store
	log        *slog.Logger
	summary    *summaryPoller
	transcript *transcriptPoller
	metrics    *Metrics

	// sharedMu guards shared and ctx. Held across the subprocess spawn in
	// attachShared, which is what makes "the first subscriber starts it, the
	// rest join it" true rather than racy.
	sharedMu sync.Mutex
	shared   map[string]*sharedStream
	// ctx is the HUB's lifetime, and it is what a shared stream is started on.
	// Starting one on the first subscriber's connection context would kill the
	// stream for everyone else the moment that one client went away.
	ctx context.Context

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
		summary:    newSummaryPoller(store, log),
		transcript: newTranscriptPoller(store, log),
		shared:     map[string]*sharedStream{},
		metrics:    NewMetrics()}
}

// Metrics exposes the latency instrument.
func (h *Hub) Metrics() *Metrics { return h.metrics }

// Store exposes the tree store.
func (h *Hub) Store() *Store { return h.store }

// Run drives the shared pollers. Summary and transcript are separate loops on
// separate periods: a tile refresh and a readable transcript are different
// products with different costs, and folding them into one ticker would make
// the cheaper one pay the other's rate.
func (h *Hub) Run(ctx context.Context) {
	// Record the hub's lifetime BEFORE anything can attach: it is the context a
	// shared upstream stream is spawned on.
	h.sharedMu.Lock()
	h.ctx = ctx
	h.sharedMu.Unlock()
	go h.transcript.run(ctx)
	go h.refreshTree(ctx)
	h.summary.run(ctx)
}

// TreeRefreshInterval is how often the tree is re-read while somebody is
// actually looking. See refreshTree.
const TreeRefreshInterval = 1500 * time.Millisecond

// refreshTree keeps the tree honest about agent state.
//
// Herdr pushes `pane.created/updated/closed/...` but NOT agent status: on 0.9.0
// `events.subscribe [{"type":"pane.agent_status_changed"}]` is refused with
// `missing field pane_id`, so there is no session-wide push for the one field
// the whole UI is about. An agent that finished therefore kept its `working`
// badge until some unrelated structural event happened to fire a resync.
//
// So we poll — but ONLY while a client is connected. With nobody looking this
// loop costs one atomic load every 1.5s and touches no socket, which matters:
// this daemon sits in front of a machine full of real sessions and must not
// generate traffic for an empty room. The call itself is `session.snapshot`,
// which is read-only.
func (h *Hub) refreshTree(ctx context.Context) {
	t := time.NewTicker(TreeRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if h.clients.Load() > 0 {
			h.store.ResyncAll()
		}
	}
}

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

	mu    sync.Mutex
	geom  map[string]Geometry
	modes map[string]Mode
	// explicit records the targets whose geometry the USER asked for, by
	// resizing deliberately. Everything else attaches at the PANE'S OWN size
	// and is never imposed on (see startStream).
	explicit map[string]bool
	// attached is the geometry a target's stream is actually running at, as
	// herdr reported it in the first frame. For a pane-matched attach this is
	// the pane's real size, learnt without touching it.
	attached map[string]Geometry
	streams  map[string]*liveStream
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
	// stream is this connection's OWN subprocess, and is nil when the
	// connection is instead a subscriber on a shared one (see shared below).
	// Nothing may Stop, Resize or write to a stream it does not own.
	stream *upstream.TerminalStream
	// shared is the fanout this connection is subscribed to, or nil for a
	// private stream. Only observe streams on targets with no explicit
	// geometry are ever shared.
	shared *sharedStream
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
		hub:      h,
		sink:     sink,
		log:      h.log,
		ctx:      ctx,
		Seen:     NewSeenSet(),
		geom:     map[string]Geometry{},
		modes:    map[string]Mode{},
		explicit: map[string]bool{},
		attached: map[string]Geometry{},
		streams:  map[string]*liveStream{},
	}
	s.co = newCoalescer(sink)
	s.co.started = true
	go s.co.run(&s.seq, s.requestRepaint, h.metrics)
	return s
}

// SetGeometry is the EXPLICIT resize path: the user has asked us to fit this
// pane to their browser window, which really does resize it for everyone
// looking at it, including the owner's laptop.
//
// It is no longer on the open-a-pane path. LOOKING MUST NOT TOUCH: a viewer
// that has not asked for a size gets startStream's pane-matched attach, which
// passes no --cols/--rows at all and therefore cannot move anything. Only a
// call to this function marks a target `explicit`, and only an explicit target
// ever has a geometry imposed on it.
func (s *Session) SetGeometry(target string, cols, rows int) {
	// Note what this does NOT do: refuse a resize because the target is
	// currently a transcript.
	//
	// It used to, and that was a deadlock. A client toggling transcript ->
	// terminal sends `resize` and then `viewport: live`, in that order (B2
	// requires it), so the resize necessarily arrives while the server still
	// believes the target is a transcript. Dropping it left the target with no
	// geometry, and `viewport: live` then refused itself forever.
	//
	// Recording a size costs nothing: geometry only reaches Herdr through
	// startStream, which refuses a transcript target outright. That is where
	// the no-resize property is enforced, because that is where it is real.
	g := Geometry{Cols: cols, Rows: rows}.Clamp()
	s.mu.Lock()
	prev := s.geom[target]
	wasExplicit := s.explicit[target]
	s.geom[target] = g
	s.explicit[target] = true
	ls := s.streams[target]
	mode := s.modes[target]
	s.mu.Unlock()

	if ls == nil || (wasExplicit && prev == g) {
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

// MatchPane gives a target's geometry BACK to the pane: the connection stops
// imposing a size and the stream is restarted with no --cols/--rows, so herdr
// attaches at whatever the pane is and tells us what that is.
//
// It is how a client undoes an explicit fit, and how every LIVE attach starts.
func (s *Session) MatchPane(target string) {
	s.mu.Lock()
	wasExplicit := s.explicit[target]
	delete(s.explicit, target)
	delete(s.geom, target)
	ls := s.streams[target]
	mode := s.modes[target]
	s.mu.Unlock()
	if !wasExplicit || ls == nil || mode != ModeLive {
		return
	}
	s.stopStream(target)
	s.mu.Lock()
	closed := s.closed
	mode = s.modes[target]
	s.mu.Unlock()
	if !closed && mode == ModeLive {
		if _, err := s.startStream(target, upstream.ModeObserve); err != nil {
			s.log.Warn("match-pane restart failed", "target", target, "err", err)
		}
	}
}

// AttachedGeometry is the size a target's stream is really running at, as herdr
// reported it. Zero until the first frame has arrived.
func (s *Session) AttachedGeometry(target string) Geometry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached[target]
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
	s.mu.Unlock()

	for target, mode := range want {
		if old[target] == mode && !s.needsReconcile(target, mode) {
			continue
		}
		switch mode {
		case ModeLive:
			s.hub.transcript.unsubscribe(target, s)
			// NO geometry requirement any more, and that reversal is the fix.
			//
			// B2 said "a terminal without a geometry message does not work", so
			// every client sent `resize` before `viewport: live` — which meant
			// merely OPENING a pane declared a size for somebody else's
			// terminal. A live attach now defaults to the pane's own size, and
			// a client that wants a different one has to ask for it.
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
		case ModeTranscript:
			// No startStream, and therefore no `herdr terminal session
			// observe --cols --rows` subprocess: the pane's own geometry is
			// never touched. That is the whole mode.
			s.stopStream(target)
			s.hub.summary.unsubscribe(target, s)
			s.hub.transcript.subscribe(target, s)
		case ModeSummary:
			s.stopStream(target)
			s.hub.transcript.unsubscribe(target, s)
			s.hub.summary.subscribe(target, s)
		default:
			s.stopStream(target)
			s.hub.summary.unsubscribe(target, s)
			s.hub.transcript.unsubscribe(target, s)
		}
	}
	for target := range old {
		if _, still := want[target]; !still {
			s.stopStream(target)
			s.hub.summary.unsubscribe(target, s)
			s.hub.transcript.unsubscribe(target, s)
		}
	}
}

// Input writes raw bytes to a target. This is the keystroke path: no JSON
// decode of the payload, no re-encode, no accumulation timer.
func (s *Session) Input(target string, data []byte) error {
	// TRANSCRIPT targets refuse raw input, and this is a safety property rather
	// than a policy. ensureControl would spawn a CONTROL stream, and a
	// transcript connection has deliberately recorded no geometry for the
	// target — so the size that stream would attach at is Clamp()'s floor,
	// 20x6. One key-bar tap would therefore squeeze a real agent into a
	// twenty-column terminal: the exact destruction transcript mode exists to
	// prevent, arriving through the back door. Answer keys and prompts in a
	// transcript go through the geometry-free `command` path
	// (agent.send_keys / agent.prompt) instead.
	s.mu.Lock()
	transcript := s.modes[target] == ModeTranscript
	s.mu.Unlock()
	if transcript {
		return ErrTranscriptInput
	}
	ls, err := s.ensureControl(target)
	if err != nil {
		return err
	}
	return ls.stream.SendInput(data)
}

// ErrTranscriptInput is returned when raw bytes are aimed at a target this
// connection is rendering as a transcript.
var ErrTranscriptInput = errors.New(
	"this pane is open in transcript view, which never attaches to the terminal: " +
		"send agent.prompt or agent.send_keys instead of raw input")

// ErrTranscriptStream is returned when anything tries to attach a terminal
// stream to a target this connection is rendering as a transcript.
var ErrTranscriptStream = errors.New(
	"transcript mode never attaches to the terminal, so it cannot resize the pane")

// Metrics is the shared latency instrument.
func (s *Session) Metrics() *Metrics { return s.hub.metrics }

// ensureControl upgrades a target's stream from observe to control on demand.
// Herdr allows one controller and unlimited observers; if the upgrade fails the
// caller gets the upstream error and the observe stream keeps running, which is
// exactly the one-controller/many-observer semantics we want to preserve.
//
// TAKING CONTROL MUST NOT ALSO RESIZE. `terminal session control --takeover` is
// the one call in this program that really does change the owner's pane, so the
// geometry it attaches at matters more here than anywhere else. startStream
// reuses the size herdr already told us the pane is (Session.attached), so the
// upgrade is control-only: same cols, same rows, nothing to SIGWINCH.
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

// ErrObserverScroll is returned when a connection that is only WATCHING a pane
// tries to scroll it upstream.
var ErrObserverScroll = errors.New(
	"scrolling a pane you are only watching would move the owner's own scrollback: " +
		"scroll your local buffer instead, or take control of the pane first")

// Scroll moves a target's viewport upstream. CONTROL ONLY.
//
// It used to fall back to `pane.scroll` for an observer, and that was the
// literal complaint — "you are scrolling actual herdr terminal". `pane.scroll`
// sets the pane's own `scroll.offset_from_bottom` for EVERYONE looking at it;
// measured on 0.9.0, `{"pane_id":"w1:p2","offset_from_bottom":20}` moves it and
// it stays moved. (The old call also passed `delta`, which 0.9.0 rejects with
// `missing field offset_from_bottom`, so it had never worked — it was a
// mutation waiting to be fixed into existence.)
//
// A viewer scrolls their OWN buffer; the client has 5000 lines of scrollback
// for exactly that. Only a controller — someone who has explicitly taken the
// pane — may move the shared viewport.
func (s *Session) Scroll(target string, delta int) error {
	s.mu.Lock()
	ls := s.streams[target]
	s.mu.Unlock()
	if ls == nil {
		return nil
	}
	if ls.mode != upstream.ModeControl {
		return ErrObserverScroll
	}
	return ls.stream.Scroll(delta)
}

func (s *Session) startStream(target string, mode upstream.TerminalMode) (*liveStream, error) {
	// THE no-resize enforcement point (SPEC AMENDMENTS 13 / J3), and it comes
	// FIRST — before routing, before any upstream lookup.
	//
	// `herdr terminal session observe --cols --rows` is the ONLY thing in this
	// program that can change a pane's geometry, and this is the only place it
	// is spawned. A transcript subscriber must never reach it — including
	// through Session.Input's control upgrade, which would attach at the
	// Clamp() floor and squeeze a real agent into a 20-column terminal.
	// Guarding here means the property holds no matter which path asks.
	s.mu.Lock()
	transcript := s.modes[target] == ModeTranscript
	s.mu.Unlock()
	if transcript {
		return nil, ErrTranscriptStream
	}

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
	// SHARED or PRIVATE, decided here and nowhere else.
	//
	// Shared is for exactly the case B1's objection no longer covers: an
	// OBSERVE stream on a target this connection has not explicitly sized, so
	// there is no --cols/--rows to be per-connection ABOUT. A control stream is
	// never shared (there is one controller), and neither is a target somebody
	// has fitted to their own window.
	shareable := mode == upstream.ModeObserve && !s.explicit[target]
	// THE GEOMETRY DECISION, and it is a one-liner on purpose.
	//
	// Unless the user EXPLICITLY asked us to fit this pane to their window, we
	// pass no geometry at all. Verified on herdr 0.9.0:
	//   - `terminal session observe <pane>`            -> attaches at the pane's
	//     own size, reports it as the first frame's width/height, and leaves the
	//     PTY alone (tput 120x40 before, during and after).
	//   - `terminal session control <pane> --cols 100 --rows 60 --takeover`
	//     -> PERMANENTLY resizes the pane (tput 120x40 -> 100x60,
	//     scroll.viewport_rows 40 -> 60, and it stays that way after we detach).
	// So a zero geometry is not a missing value; it is the only value that
	// cannot disturb the owner's terminal.
	g := s.geometryFor(target)
	hdr := HeaderLenFor(target)
	ls := &liveStream{mode: mode, hdrLen: hdr, target: target, startedAt: time.Now()}
	// Fresh attachment (or a restart after a takeover): this connection has no
	// trustworthy buffer for the target, so the next full repaint is a snapshot.
	ls.needSnap.Store(true)
	s.streams[target] = ls
	s.mu.Unlock()

	if shareable {
		ss, joined, err := s.hub.attachShared(s, target, paneID, socket, hdr, ls)
		if err != nil {
			s.mu.Lock()
			if s.streams[target] == ls {
				delete(s.streams, target)
			}
			s.mu.Unlock()
			s.sink.SendJSON("closed", map[string]any{"target": target, "reason": err.Error()})
			return nil, err
		}
		ls.shared = ss
		if joined {
			// A LATE JOINER, and this is the part that must not be skipped.
			// The subprocess is already running, so herdr sends it no attach
			// frame — without this the client would sit on an empty terminal
			// until the pane happened to move. It is owed exactly what a
			// private attach gave it: the pane's size, and a full screen.
			if g := ss.geometry(); g.Valid() {
				s.noteAttached(target, g)
			}
			s.requestRepaint(target)
		}
		return ls, nil
	}

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

// geometryFor is the geometry a stream for `target` should attach at. It must
// be called with s.mu held.
//
// Zero means "pass no --cols/--rows at all", and that is the default. See the
// block comment in startStream for the measurements behind it.
func (s *Session) geometryFor(target string) Geometry {
	if s.explicit[target] {
		return s.geom[target].Clamp()
	}
	if a := s.attached[target]; a.Valid() {
		// A control upgrade on a pane we are already observing REUSES the size
		// herdr told us the pane is, so taking control is not also a resize.
		return a
	}
	return Geometry{}
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
	if ls.shared != nil {
		// Leaving a shared stream is a detach, not a kill: the subprocess dies
		// only when the LAST subscriber goes (detach handles that, and waits).
		ls.shared.detach(s)
		return
	}
	if ls.stream != nil {
		ls.stream.Stop()
		ls.stream.Wait(2 * time.Second)
	}
}

// Repaint is a CLIENT-REQUESTED full repaint of a LIVE target.
//
// It exists for the browser's render-health watchdog (SPEC B8 is a list of ways
// a character grid can silently go out of alignment; the client is the only
// side that can measure that it happened). The client resets its emulator and
// needs a guaranteed full screen to repaint from — Herdr's own periodic `full`
// frames are not a guarantee, they are a coincidence with a period.
//
// It is deliberately a no-op for anything that is not LIVE: a transcript
// target has no emulator and no geometry to be out of alignment with, and a
// `none` target has no buffer to repair.
func (s *Session) Repaint(target string) {
	s.mu.Lock()
	live := s.modes[target] == ModeLive && !s.closed
	s.mu.Unlock()
	if !live {
		return
	}
	s.requestRepaint(target)
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
		if ls.shared != nil {
			// Refcounted: the pane keeps streaming for everyone else still
			// watching it, and stops only when this was the last viewer.
			ls.shared.detach(s)
			continue
		}
		if ls.stream != nil {
			ls.stream.Stop()
		}
	}
	s.hub.summary.unsubscribeAll(s)
	s.hub.transcript.unsubscribeAll(s)
	s.co.close()
}

// noteAttached records the size a stream is really running at and tells the
// client, once per change. The `geometry` frame carries `source`, so the UI can
// state truthfully whether the pane was matched or imposed upon.
func (s *Session) noteAttached(target string, g Geometry) {
	s.mu.Lock()
	if s.closed || s.attached[target] == g {
		s.mu.Unlock()
		return
	}
	s.attached[target] = g
	source := GeomPane
	if s.explicit[target] {
		source = GeomClient
	}
	s.mu.Unlock()
	s.sink.SendJSON("geometry", map[string]any{
		"target": target, "cols": g.Cols, "rows": g.Rows, "source": source,
	})
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
	// Herdr stamps every frame with the size it is rendering at. When we
	// attached WITHOUT --cols/--rows that number is the pane's OWN size, learnt
	// without having touched it — which is exactly what the client needs in
	// order to scale its rendering to the pane instead of the other way round.
	if f.Width > 0 && f.Height > 0 {
		h.sess.noteAttached(h.target, Geometry{Cols: f.Width, Rows: f.Height})
	}
	h.sess.deliverFrame(h.target, h.ls, f.Data, f.Off, h.hdrLen, f.Full, t0)
}

// deliverFrame is ONE connection's half of a terminal frame, and it is shared
// verbatim by the private and the fanned-out paths — which is the point. A
// client must not be able to tell from its data plane whether the subprocess
// behind it is serving it alone.
//
// It takes ownership of exactly one reference on buf.
func (s *Session) deliverFrame(target string, ls *liveStream, buf *upstream.Buf,
	off, hdrLen int, full bool, t0 time.Time) {
	// A stream that has stayed up counts as recovered: forget the backoff
	// history so a target that dies again hours later restarts promptly. The
	// check is a wall-clock compare and an atomic load, so it costs nothing on
	// the per-frame hot path.
	if ls != nil && !ls.healthy.Load() && !ls.startedAt.IsZero() &&
		time.Since(ls.startedAt) >= StreamHealthyAfter && ls.healthy.CompareAndSwap(false, true) {
		s.clearRestarts(target)
	}
	s.hub.metrics.CountFrame(buf.Len() - off)
	if off != hdrLen {
		// Defensive: reservation mismatch means we cannot stamp in place.
		defer buf.Release()
		s.sink.SendBinary(encodeFrame(TypeFrame, s.seq.next(), target, buf.B[off:]))
		return
	}
	// type 2 means "you cannot trust your buffer, reset and repaint". That is
	// true only when this CONNECTION just attached, lost bytes to a gap, or had
	// its stream restarted — NOT on Herdr's periodic `full` repaints, which are
	// self-contained and paint cleanly into a live buffer. needSnap is per
	// CONNECTION, so on a shared stream one client's reset never resets another.
	if full && ls != nil && ls.needSnap.CompareAndSwap(true, false) {
		stampHeader(buf.B, TypeSnapshot, s.seq.next(), target)
		s.sink.SendBinary(buf)
		s.hub.metrics.Output.Since(t0)
		return
	}
	// A full repaint still SUPERSEDES anything buffered for this target: there
	// is no point writing deltas the repaint is about to overwrite.
	s.co.push(target, buf, hdrLen, full, t0)
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
