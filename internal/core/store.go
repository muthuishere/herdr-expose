package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// SessionState is one Herdr session inside the tree.
//
// The tree is sessions -> workspaces -> tabs -> panes. `Name` is the id: it is
// stable across server restarts, unlike pane ids, which are re-minted every
// time a session comes back up.
type SessionState struct {
	Name      string             `json:"name"`
	Socket    string             `json:"socket"`
	Running   bool               `json:"running"`
	Connected bool               `json:"connected"`
	Origin    bool               `json:"origin"`
	Default   bool               `json:"default"`
	Version   string             `json:"herdr_version"`
	Protocol  int                `json:"herdr_protocol"`
	Err       string             `json:"error,omitempty"`
	Snapshot  *upstream.Snapshot `json:"snapshot"`
}

// Tree is an immutable snapshot of EVERY session. Readers never mutate it; the
// store replaces the pointer wholesale under the writer goroutine.
type Tree struct {
	Rev uint64 `json:"rev"`
	// Connected is true when at least one session is connected.
	Connected bool `json:"connected"`
	// Version/Protocol come from the focused session's handshake.
	Version  string `json:"herdr_version"`
	Protocol int    `json:"herdr_protocol"`
	// FocusedSession is the session a client should land on: the origin session
	// when it is up, else the first connected one. The UI is free to ignore it —
	// the origin session gets no other privileges.
	FocusedSession string          `json:"focused_session"`
	Sessions       []*SessionState `json:"sessions"`
	// DoneSeq is the authoritative idle-transition counter per pane, keyed by
	// the SESSION-QUALIFIED target. A connection derives its own DONE badges
	// from it via its SeenSet; the store deliberately holds no global seen state.
	DoneSeq map[string]int64 `json:"done_seq"`
	// ChangedAt is when each pane's agent status last changed (same keying).
	ChangedAt map[string]time.Time `json:"changed_at"`
	At        time.Time            `json:"at"`
}

// Session returns one session's state by name.
func (t *Tree) Session(name string) *SessionState {
	for _, s := range t.Sessions {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// ErrNoSession means a target named a session this server does not have.
var ErrNoSession = errors.New("core: unknown herdr session")

// Store owns the session trees. A single writer goroutine applies upstream
// events and resyncs; everyone else reads an immutable *Tree via an atomic
// pointer, so there are no mutexes and no torn reads on the read path.
//
// One upstream CLIENT and one event subscription per RUNNING session. A session
// that dies takes down only its own worker: its entry stays in the tree marked
// disconnected, and its EventStream reconnects with backoff on its own.
type Store struct {
	log *slog.Logger

	cur atomic.Pointer[Tree]
	rev atomic.Uint64

	// work is the single-writer mailbox.
	work chan func(*mutable)

	subMu sync.Mutex
	subs  map[int]chan struct{}
	subID int

	originName   string
	originSocket string

	reg *upstream.Registry

	// mu guards workers, which is read from every connection goroutine to route
	// a target to its session's client.
	mu      sync.Mutex
	workers map[string]*sessionWorker
	ctx     context.Context
}

// sessionWorker is one session's upstream attachment.
type sessionWorker struct {
	info   upstream.Session
	client *upstream.Client
	cancel context.CancelFunc
	resync chan struct{}
}

// mutable is the writer goroutine's private state.
type mutable struct {
	sessions  map[string]*SessionState
	doneSeq   map[string]int64
	lastState map[string]string
	changedAt map[string]time.Time
}

// NewStore builds an empty multi-session store. Sessions are discovered at
// runtime; nothing is bound to $HERDR_SOCKET_PATH except the "origin" label.
func NewStore(log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	name, socket := upstream.OriginSession()
	s := &Store{
		log:          log,
		work:         make(chan func(*mutable), 512),
		subs:         make(map[int]chan struct{}),
		workers:      map[string]*sessionWorker{},
		originName:   name,
		originSocket: socket,
	}
	s.cur.Store(&Tree{Rev: 0, Sessions: []*SessionState{},
		DoneSeq: map[string]int64{}, ChangedAt: map[string]time.Time{}, At: time.Now()})
	s.reg = upstream.NewRegistry(log, s.onDiscovery)
	return s
}

// Run starts the single writer goroutine and the session registry.
// It blocks until ctx is cancelled.
func (s *Store) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	m := &mutable{
		sessions:  map[string]*SessionState{},
		doneSeq:   map[string]int64{},
		lastState: map[string]string{},
		changedAt: map[string]time.Time{},
	}

	go s.reg.Run(ctx)

	for {
		select {
		case <-ctx.Done():
			s.stopAllWorkers()
			return
		case fn := <-s.work:
			fn(m)
		}
	}
}

// onDiscovery is the registry callback: hand the listing to the single writer.
func (s *Store) onDiscovery(list []upstream.Session) {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil {
		return // not running yet
	}
	s.post(func(m *mutable) { s.reconcile(ctx, m, list) })
}

// Registry exposes the discovery poller, so a test can swap its source.
func (s *Store) Registry() *upstream.Registry { return s.reg }

// SetRegistryInterval overrides the discovery cadence (tests).
func (s *Store) SetRegistryInterval(d time.Duration) {
	if s.reg != nil {
		s.reg.SetInterval(d)
	}
}

// reconcile applies a discovery listing: start workers for sessions that came
// up, stop workers for sessions that went away, and keep an entry for a session
// that died so the UI can show it as disconnected rather than silently losing it.
func (s *Store) reconcile(ctx context.Context, m *mutable, list []upstream.Session) {
	seen := map[string]bool{}
	for _, info := range list {
		seen[info.Name] = true
		st := m.sessions[info.Name]
		if st == nil {
			if !info.Running {
				continue // never attached, not running: not worth an entry
			}
			st = &SessionState{Name: info.Name}
			m.sessions[info.Name] = st
		}
		st.Socket = info.SocketPath
		st.Running = info.Running
		st.Default = info.Default
		st.Origin = info.SocketPath == s.originSocket ||
			(s.originName != "" && info.Name == s.originName)

		if info.Running {
			s.ensureWorker(ctx, info)
		} else if s.stopWorker(info.Name) {
			st.Connected = false
			st.Snapshot = nil
			s.forgetSession(m, info.Name)
		}
	}
	// A session that vanished from the listing entirely (its directory was
	// removed) is dropped.
	for name := range m.sessions {
		if seen[name] {
			continue
		}
		s.stopWorker(name)
		delete(m.sessions, name)
		s.forgetSession(m, name)
	}
	s.publish(m)
}

// forgetSession drops derived per-pane state for a session that went away.
func (s *Store) forgetSession(m *mutable, name string) {
	prefix := name + TargetSep
	for k := range m.doneSeq {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(m.doneSeq, k)
			delete(m.lastState, k)
			delete(m.changedAt, k)
		}
	}
}

func (s *Store) ensureWorker(ctx context.Context, info upstream.Session) {
	s.mu.Lock()
	if w := s.workers[info.Name]; w != nil {
		if w.info.SocketPath == info.SocketPath {
			s.mu.Unlock()
			return
		}
		// The socket moved under us: restart the attachment.
		w.cancel()
		delete(s.workers, info.Name)
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &sessionWorker{
		info:   info,
		client: upstream.NewSessionClient(info),
		cancel: cancel,
		resync: make(chan struct{}, 1),
	}
	s.workers[info.Name] = w
	s.mu.Unlock()

	s.log.Info("attaching to herdr session", "session", info.Name, "socket", info.SocketPath)
	go s.runWorker(wctx, w)
}

func (s *Store) stopWorker(name string) bool {
	s.mu.Lock()
	w := s.workers[name]
	delete(s.workers, name)
	s.mu.Unlock()
	if w == nil {
		return false
	}
	s.log.Info("detaching from herdr session", "session", name)
	w.cancel()
	return true
}

func (s *Store) stopAllWorkers() {
	s.mu.Lock()
	ws := s.workers
	s.workers = map[string]*sessionWorker{}
	s.mu.Unlock()
	for _, w := range ws {
		w.cancel()
	}
}

// runWorker holds one session's event subscription and resync loop. Its
// EventStream owns the reconnect backoff, so a session that dies retries on its
// own without touching any other session.
func (s *Store) runWorker(ctx context.Context, w *sessionWorker) {
	if pong, err := w.client.Ping(ctx); err == nil {
		v, p := pong.Version, pong.Protocol
		s.post(func(m *mutable) {
			if st := m.sessions[w.info.Name]; st != nil {
				st.Version, st.Protocol = v, p
			}
		})
	}
	go s.resyncLoop(ctx, w)
	sink := &workerSink{store: s, w: w}
	upstream.NewEventStream(w.client, sink, s.log.With("session", w.info.Name), nil).Run(ctx)
}

// resyncLoop serialises snapshot fetches for ONE session and debounces bursts.
func (s *Store) resyncLoop(ctx context.Context, w *sessionWorker) {
	const debounce = 25 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.resync:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(debounce):
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		snap, err := w.client.SessionSnapshot(rctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("store: resync failed", "session", w.info.Name, "err", err)
			}
			continue
		}
		s.post(func(m *mutable) {
			st := m.sessions[w.info.Name]
			if st == nil {
				return
			}
			st.Connected = true
			st.Err = ""
			s.applySnapshot(m, w.info.Name, snap)
			s.publish(m)
		})
	}
}

// Tree returns the current immutable tree.
func (s *Store) Tree() *Tree { return s.cur.Load() }

// HerdrVersion reports the focused session's upstream handshake.
func (s *Store) HerdrVersion() (string, int) {
	t := s.cur.Load()
	return t.Version, t.Protocol
}

// Sessions lists the sessions this store is attached to, running first.
func (s *Store) Sessions() []string {
	t := s.cur.Load()
	out := make([]string, 0, len(t.Sessions))
	for _, st := range t.Sessions {
		out = append(out, st.Name)
	}
	return out
}

// DefaultSession is the session used when a client names none: the origin
// session when it is up, else the first connected one.
func (s *Store) DefaultSession() string { return s.cur.Load().FocusedSession }

// Client returns the RPC client for one session, or nil.
func (s *Store) Client(session string) *upstream.Client {
	if session == "" {
		session = s.DefaultSession()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.workers[session]; w != nil {
		return w.client
	}
	return nil
}

// Socket returns a session's socket path, or "".
func (s *Store) Socket(session string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.workers[session]; w != nil {
		return w.info.SocketPath
	}
	return ""
}

// Resolve maps a wire target onto the session that owns it, that session's
// client and the bare Herdr id to send upstream. This is the ONE place a
// `<session>/<pane>` target is taken apart.
func (s *Store) Resolve(target string) (session string, c *upstream.Client, id string, err error) {
	session, id = SplitTarget(target)
	if session == "" {
		session = s.DefaultSession()
	}
	c = s.Client(session)
	if c == nil {
		return session, nil, id, ErrNoSession
	}
	return session, c, id, nil
}

// Watch returns a channel that is signalled (coalesced) on every tree change,
// plus a cancel func.
func (s *Store) Watch() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	s.subID++
	id := s.subID
	s.subs[id] = ch
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		delete(s.subs, id)
		s.subMu.Unlock()
	}
}

func (s *Store) notify() {
	s.subMu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default: // already pending; watchers read the latest tree anyway
		}
	}
	s.subMu.Unlock()
}

// post enqueues work for the writer goroutine, dropping it if the mailbox is
// full rather than blocking an upstream reader.
func (s *Store) post(fn func(*mutable)) {
	select {
	case s.work <- fn:
	default:
		s.log.Warn("store: writer mailbox full, dropping update")
	}
}

func (s *Store) publish(m *mutable) {
	done := make(map[string]int64, len(m.doneSeq))
	for k, v := range m.doneSeq {
		done[k] = v
	}
	changed := make(map[string]time.Time, len(m.changedAt))
	for k, v := range m.changedAt {
		changed[k] = v
	}
	names := make([]string, 0, len(m.sessions))
	for name := range m.sessions {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := m.sessions[names[i]], m.sessions[names[j]]
		if a.Connected != b.Connected {
			return a.Connected
		}
		if a.Running != b.Running {
			return a.Running
		}
		return a.Name < b.Name
	})

	sessions := make([]*SessionState, 0, len(names))
	anyConn := false
	focused := ""
	for _, name := range names {
		st := *m.sessions[name] // copy: the tree must be immutable
		sessions = append(sessions, &st)
		if st.Connected {
			anyConn = true
			if focused == "" || (st.Origin && m.sessions[focused] != nil && !m.sessions[focused].Origin) {
				focused = st.Name
			}
		}
	}
	t := &Tree{
		Rev:            s.rev.Add(1),
		Connected:      anyConn,
		FocusedSession: focused,
		Sessions:       sessions,
		DoneSeq:        done,
		ChangedAt:      changed,
		At:             time.Now(),
	}
	if f := t.Session(focused); f != nil {
		t.Version, t.Protocol = f.Version, f.Protocol
	}
	s.cur.Store(t)
	s.notify()
}

// applySnapshot recomputes derived state for ONE session from a fresh tree.
// Every derived key is SESSION-QUALIFIED, because pane ids repeat across
// sessions (they all start at w1:p1).
func (s *Store) applySnapshot(m *mutable, session string, snap *upstream.Snapshot) {
	st := m.sessions[session]
	if st == nil {
		return
	}
	st.Snapshot = snap
	live := make(map[string]struct{}, len(snap.Panes))
	for i := range snap.Panes {
		p := &snap.Panes[i]
		key := JoinTarget(session, p.PaneID)
		live[key] = struct{}{}
		prev := m.lastState[key]
		if prev != p.AgentStatus {
			m.lastState[key] = p.AgentStatus
			m.changedAt[key] = time.Now()
			// A transition INTO an idle-ish state is what creates a DONE badge.
			if isSettled(p.AgentStatus) && prev != "" {
				m.doneSeq[key]++
			}
		}
		// Focus in Herdr itself clears the badge for everyone: the human
		// looked at it on the desktop.
		if p.Focused {
			m.doneSeq[key] = 0
		}
	}
	prefix := session + TargetSep
	for id := range m.doneSeq {
		if len(id) <= len(prefix) || id[:len(prefix)] != prefix {
			continue
		}
		if _, ok := live[id]; !ok {
			delete(m.doneSeq, id)
			delete(m.lastState, id)
			delete(m.changedAt, id)
		}
	}
}

func isSettled(status string) bool {
	switch status {
	case "idle", "done", "blocked", "waiting", "exited":
		return true
	}
	return false
}

// workerSink adapts one session's event stream to the store's writer.
type workerSink struct {
	store *Store
	w     *sessionWorker
}

// Resync is called right after events.subscribe is confirmed, never before:
// 0.9.0 does not replay retained history, so snapshot-then-subscribe would
// silently lose everything in the gap. It is per SESSION, exactly as before —
// multi-session just runs one of these per socket.
func (ws *workerSink) Resync() {
	select {
	case ws.w.resync <- struct{}{}:
	default: // one is already queued; it will observe the latest state
	}
}

// OnEvent folds one upstream event in. Herdr's events carry partial data, so
// anything structural triggers a cheap full resync of THAT session rather than
// a hand-written merge that can silently diverge from upstream.
func (ws *workerSink) OnEvent(ev upstream.Event) {
	ws.store.log.Debug("upstream event", "session", ws.w.info.Name, "event", ev.Event)
	ws.Resync()
}

// OnUpstreamState records one session's connectivity. Client connections stay
// open across an upstream outage; they just see that session as disconnected,
// and every other session keeps streaming.
func (ws *workerSink) OnUpstreamState(connected bool, err error) {
	s := ws.store
	name := ws.w.info.Name
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.post(func(m *mutable) {
		st := m.sessions[name]
		if st == nil {
			return
		}
		if st.Connected == connected && st.Err == msg {
			return
		}
		st.Connected = connected
		st.Err = msg
		if !connected {
			st.Snapshot = nil
		}
		s.publish(m)
	})
}

// TreeJSON renders the tree for the control plane.
func (t *Tree) TreeJSON() json.RawMessage {
	b, err := json.Marshal(t)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
