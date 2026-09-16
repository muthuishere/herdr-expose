package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Tree is an immutable snapshot of the session. Readers never mutate it; the
// store replaces the pointer wholesale under the writer goroutine.
type Tree struct {
	Rev       uint64             `json:"rev"`
	Connected bool               `json:"connected"`
	Version   string             `json:"herdr_version"`
	Protocol  int                `json:"herdr_protocol"`
	Snapshot  *upstream.Snapshot `json:"snapshot"`
	// DoneSeq is the authoritative idle-transition counter per pane. A
	// connection derives its own DONE badges from it via its SeenSet; the store
	// deliberately holds no global seen state.
	DoneSeq map[string]int64 `json:"done_seq"`
	// ChangedAt is when each pane's agent status last changed. Herdr exposes a
	// state_change_seq but no wall-clock, and the UI wants "blocked for 4m".
	ChangedAt map[string]time.Time `json:"changed_at"`
	At        time.Time            `json:"at"`
}

// Store owns the session tree. A single writer goroutine applies upstream
// events and resyncs; everyone else reads an immutable *Tree via an atomic
// pointer, so there are no mutexes and no torn reads on the read path.
type Store struct {
	client *upstream.Client
	log    *slog.Logger

	cur atomic.Pointer[Tree]
	rev atomic.Uint64

	// work is the single-writer mailbox.
	work chan func(*mutable)

	subMu sync.Mutex
	subs  map[int]chan struct{}
	subID int

	herdrVersion  string
	herdrProtocol int

	// resyncReq coalesces snapshot fetches: Herdr can emit a burst of events
	// for one user action, and one snapshot answers all of them.
	resyncReq chan struct{}
}

// mutable is the writer goroutine's private state.
type mutable struct {
	snap      *upstream.Snapshot
	connected bool
	doneSeq   map[string]int64
	lastState map[string]string
	changedAt map[string]time.Time
}

// NewStore builds a store bound to an upstream client.
func NewStore(c *upstream.Client, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		client:    c,
		log:       log,
		work:      make(chan func(*mutable), 256),
		subs:      make(map[int]chan struct{}),
		resyncReq: make(chan struct{}, 1),
	}
	s.cur.Store(&Tree{Rev: 0, DoneSeq: map[string]int64{},
		ChangedAt: map[string]time.Time{}, At: time.Now()})
	return s
}

// Run starts the single writer goroutine and the upstream event stream.
// It blocks until ctx is cancelled.
func (s *Store) Run(ctx context.Context) {
	m := &mutable{doneSeq: map[string]int64{}, lastState: map[string]string{},
		changedAt: map[string]time.Time{}}

	if pong, err := s.client.Ping(ctx); err == nil {
		s.herdrVersion, s.herdrProtocol = pong.Version, pong.Protocol
	} else {
		s.log.Warn("upstream ping failed at startup", "err", err)
	}

	es := upstream.NewEventStream(s.client, (*storeSink)(s), s.log, nil)
	go es.Run(ctx)
	go s.resyncLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case fn := <-s.work:
			fn(m)
		}
	}
}

// resyncLoop serialises snapshot fetches and debounces bursts.
func (s *Store) resyncLoop(ctx context.Context) {
	const debounce = 25 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.resyncReq:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(debounce):
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		snap, err := s.client.SessionSnapshot(rctx)
		cancel()
		if err != nil {
			s.log.Warn("store: resync failed", "err", err)
			continue
		}
		s.post(func(m *mutable) {
			m.connected = true
			s.applySnapshot(m, snap)
			s.publish(m)
		})
	}
}

// Tree returns the current immutable tree.
func (s *Store) Tree() *Tree { return s.cur.Load() }

// HerdrVersion reports the upstream version handshake.
func (s *Store) HerdrVersion() (string, int) { return s.herdrVersion, s.herdrProtocol }

// Client exposes the RPC client for command passthrough.
func (s *Store) Client() *upstream.Client { return s.client }

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
	t := &Tree{
		Rev:       s.rev.Add(1),
		Connected: m.connected,
		Version:   s.herdrVersion,
		Protocol:  s.herdrProtocol,
		Snapshot:  m.snap,
		DoneSeq:   done,
		ChangedAt: changed,
		At:        time.Now(),
	}
	s.cur.Store(t)
	s.notify()
}

// applySnapshot recomputes derived state from a fresh tree.
func (s *Store) applySnapshot(m *mutable, snap *upstream.Snapshot) {
	m.snap = snap
	live := make(map[string]struct{}, len(snap.Panes))
	for i := range snap.Panes {
		p := &snap.Panes[i]
		live[p.PaneID] = struct{}{}
		prev := m.lastState[p.PaneID]
		if prev != p.AgentStatus {
			m.lastState[p.PaneID] = p.AgentStatus
			m.changedAt[p.PaneID] = time.Now()
			// A transition INTO an idle-ish state is what creates a DONE badge.
			if isSettled(p.AgentStatus) && prev != "" {
				m.doneSeq[p.PaneID]++
			}
		}
		// Focus in Herdr itself clears the badge for everyone: the human
		// looked at it on the desktop.
		if p.Focused {
			m.doneSeq[p.PaneID] = 0
		}
	}
	for id := range m.doneSeq {
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

// storeSink adapts Store to upstream.EventSink without exporting the methods.
type storeSink Store

// Resync is called right after events.subscribe is confirmed, never before:
// 0.9.0 does not replay retained history, so snapshot-then-subscribe would
// silently lose everything in the gap.
func (ss *storeSink) Resync() {
	s := (*Store)(ss)
	select {
	case s.resyncReq <- struct{}{}:
	default: // one is already queued; it will observe the latest state
	}
}

// OnEvent folds one upstream event in. Herdr's events carry partial data, so
// anything structural triggers a cheap full resync rather than a hand-written
// merge that can silently diverge from upstream.
func (ss *storeSink) OnEvent(ev upstream.Event) {
	s := (*Store)(ss)
	s.log.Debug("upstream event", "event", ev.Event)
	// Herdr events carry partial payloads. Rather than hand-merge (and slowly
	// diverge from upstream), request a debounced full snapshot: it is one
	// socket round trip and it cannot be wrong.
	ss.Resync()
}

// OnUpstreamState records connectivity. Client connections stay open across an
// upstream outage; they just see connected:false in the tree.
func (ss *storeSink) OnUpstreamState(connected bool, err error) {
	s := (*Store)(ss)
	s.post(func(m *mutable) {
		if m.connected == connected {
			return
		}
		m.connected = connected
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
