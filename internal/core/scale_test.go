package core

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// An IDLE tree must produce NO frames, however often it is re-read.
//
// K6 re-reads the tree every 1.5s while anyone is looking, and ResyncAll
// queues that read per SESSION — so twelve sessions meant twelve publishes a
// tick, each bumping Rev and waking every connection. Measured on an idle
// 12-session bed: 44 tree frames in 12s, 12.5KB each, ONE distinct payload
// among them, and `rev` the only field that ever differed. This is the
// regression test for that: repeated publishes of identical content are not
// revisions and wake nobody.
func TestIdleTreePublishesNothing(t *testing.T) {
	s := NewStore(quietLog())
	// No discovery: this test is about publish, and it must never go looking
	// for the real Herdr sessions on the machine it runs on.
	empty := &fakeLister{}
	s.Registry().List = empty.List
	s.SetRegistryInterval(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	ch, unwatch := s.Watch()
	defer unwatch()

	// One real change, so there is something to be idle ABOUT.
	s.post(func(m *mutable) {
		m.sessions["alpha"] = &SessionState{Name: "alpha", Connected: true, Running: true}
		s.publish(m)
	})
	waitUntil(t, func() bool { return s.Tree().Session("alpha") != nil })
	// Drain anything the startup discovery left pending.
	for draining := true; draining; {
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
			draining = false
		}
	}
	rev := s.Tree().Rev
	if rev == 0 {
		t.Fatal("a published tree must carry a revision")
	}
	baseSuppressed := s.SuppressedPublishes()

	// Now the idle ticks: several sweeps' worth of resyncs that change nothing.
	const ticks = 12
	for i := 0; i < ticks; i++ {
		done := make(chan struct{})
		s.post(func(m *mutable) { s.publish(m); close(done) })
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("writer goroutine wedged")
		}
	}

	select {
	case <-ch:
		t.Fatal("an idle tree woke a watcher: the broadcast is not change-detected")
	case <-time.After(150 * time.Millisecond):
	}
	if got := s.Tree().Rev; got != rev {
		t.Fatalf("rev moved on an idle tree: %d -> %d", rev, got)
	}
	if got := s.SuppressedPublishes() - baseSuppressed; got != ticks {
		t.Fatalf("suppressed %d idle publishes, want %d", got, ticks)
	}

	// And a REAL change still gets through immediately, with no added latency
	// and no timer: suppression must not become silence.
	s.post(func(m *mutable) {
		m.sessions["beta"] = &SessionState{Name: "beta", Connected: true, Running: true}
		s.publish(m)
	})
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("a real change after an idle stretch never reached a watcher")
	}
	if got := s.Tree().Rev; got <= rev {
		t.Fatalf("a real change did not bump rev (%d -> %d)", rev, got)
	}
}

// The transcript sweep must not be serial: its period was N x the cost of one
// read, which is what took the DEFAULT view from ~1s at one pane to ~10s at
// twelve (measured, with an 0.78s read).
func TestTranscriptSweepIsBoundedAndConcurrent(t *testing.T) {
	const (
		n       = 12
		workers = 4
		cost    = 40 * time.Millisecond
	)
	items := make([]string, n)
	for i := range items {
		items[i] = string(rune('a' + i))
	}

	var live, peak atomic.Int64
	var done atomic.Int64
	start := time.Now()
	forEachBounded(context.Background(), items, workers, func(context.Context, string) {
		cur := live.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(cost)
		live.Add(-1)
		done.Add(1)
	})
	elapsed := time.Since(start)

	if got := done.Load(); got != n {
		t.Fatalf("swept %d targets, want %d", got, n)
	}
	if got := peak.Load(); got > workers {
		t.Fatalf("%d reads in flight at once, bound is %d", got, workers)
	}
	if got := peak.Load(); got < 2 {
		t.Fatalf("the sweep never ran two reads at once (peak %d): it is still serial", got)
	}
	// Serial would be n*cost. Bounded is ceil(n/workers)*cost, with slack for
	// a loaded CI box.
	if want := time.Duration(n) * cost; elapsed >= want {
		t.Fatalf("sweep took %v, which is no better than the serial %v", elapsed, want)
	}
	// forEachBounded must be a BARRIER: nothing may still be running.
	if got := live.Load(); got != 0 {
		t.Fatalf("%d reads still in flight after the sweep returned", got)
	}
}

// Two connections watching ONE pane share the subprocess, and share nothing
// else. Each keeps its own seq and its own snapshot-on-attach.
func TestSharedStreamFanoutKeepsPerConnectionState(t *testing.T) {
	const target = "alpha/w1:p1"
	hub := NewHub(NewStore(quietLog()), quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sinkA, sinkB := &frameSink{}, &frameSink{}
	a := hub.NewSession(ctx, sinkA)
	b := hub.NewSession(ctx, sinkB)
	defer a.Close()
	defer b.Close()

	hdr := HeaderLenFor(target)
	ss := &sharedStream{hub: hub, target: target, hdrLen: hdr,
		startedAt: time.Now(), subs: map[*Session]*liveStream{}}

	lsA := &liveStream{mode: upstream.ModeObserve, hdrLen: hdr, target: target,
		startedAt: time.Now(), shared: ss}
	lsA.needSnap.Store(true)
	a.mu.Lock()
	a.streams[target] = lsA
	a.mu.Unlock()
	if !ss.attach(a, lsA) {
		t.Fatal("could not attach the first subscriber")
	}

	// A attaches first and its first full frame is its SNAPSHOT.
	ss.OnFrame(fullFrame(target, "screen-one"))
	waitUntil(t, func() bool { return len(sinkA.frames()) == 1 })
	if got := sinkA.frames()[0].typ; got != TypeSnapshot {
		t.Fatalf("first frame for A was type %d, want snapshot", got)
	}

	// B joins a stream that is ALREADY RUNNING, at a different time.
	lsB := &liveStream{mode: upstream.ModeObserve, hdrLen: hdr, target: target,
		startedAt: time.Now(), shared: ss}
	lsB.needSnap.Store(true)
	b.mu.Lock()
	b.streams[target] = lsB
	b.mu.Unlock()
	if !ss.attach(b, lsB) {
		t.Fatal("could not attach the second subscriber")
	}

	ss.OnFrame(fullFrame(target, "screen-two"))
	waitUntil(t, func() bool { return len(sinkA.frames()) == 2 && len(sinkB.frames()) == 1 })

	// B's FIRST frame is a snapshot of its own, even though the stream had been
	// running: a late joiner with an empty emulator must be told to repaint.
	if got := sinkB.frames()[0].typ; got != TypeSnapshot {
		t.Fatalf("B's first frame was type %d, want its own snapshot", got)
	}
	// A is mid-stream and must NOT be reset by B arriving.
	if got := sinkA.frames()[1].typ; got != TypeFrame {
		t.Fatalf("A was reset to type %d by a second viewer attaching", got)
	}
	// Same bytes to both, and each stamped with ITS OWN sequence.
	if pa, pb := sinkA.frames()[1].payload, sinkB.frames()[0].payload; pa != pb {
		t.Fatalf("fanout delivered different payloads: %q vs %q", pa, pb)
	}
	if sinkB.frames()[0].seq != 1 {
		t.Fatalf("B's first frame carried seq %d, want a sequence of its own starting at 1",
			sinkB.frames()[0].seq)
	}
	if sinkA.frames()[1].seq != 2 {
		t.Fatalf("A's second frame carried seq %d, want 2", sinkA.frames()[1].seq)
	}

	// Refcount: one leaver does not take the stream down, the last one does.
	ss.detach(a)
	ss.mu.Lock()
	dead, n := ss.dead, len(ss.subs)
	ss.mu.Unlock()
	if dead || n != 1 {
		t.Fatalf("one viewer leaving retired a stream another is still watching (dead=%v subs=%d)", dead, n)
	}
	ss.detach(b)
	ss.mu.Lock()
	dead = ss.dead
	ss.mu.Unlock()
	if !dead {
		t.Fatal("the last viewer left and the stream stayed up: an empty room must cost nothing")
	}
	if ss.attach(a, lsA) {
		t.Fatal("a retired stream accepted a new subscriber")
	}
}

/* -------------------------------------------------------------- test sinks */

type capturedFrame struct {
	typ     byte
	seq     uint64
	payload string
}

type frameSink struct {
	mu   sync.Mutex
	got  []capturedFrame
	json []string
}

func (f *frameSink) SendBinary(b *upstream.Buf) {
	h, payload, err := DecodeServerFrame(b.B)
	f.mu.Lock()
	if err == nil {
		f.got = append(f.got, capturedFrame{typ: h.Type, seq: h.Seq, payload: string(payload)})
	}
	f.mu.Unlock()
	b.Release()
}

func (f *frameSink) SendJSON(typ string, _ any) {
	f.mu.Lock()
	f.json = append(f.json, typ)
	f.mu.Unlock()
}

func (f *frameSink) frames() []capturedFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedFrame(nil), f.got...)
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
