package core

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// ADR 0014 called this out in writing: "pooled buffers mean ownership rules ...
// this is the main source of subtle bugs in this design and is worth a
// race-detector test". These are those tests. Run them with -race.
//
// The bug they pin: coalescer.run swapped the producer's frame slices under the
// lock and then released the lock while writeRun still held the swapped slice.
// A concurrent Session.Close -> coalescer.close walked BOTH slices and released
// the same buffers, so a pooled Buf went back to its sync.Pool twice and could
// be handed to two owners at once — one viewer's terminal bytes inside another
// viewer's frame. The reported panic (Buf released more times than retained)
// was the lucky outcome, not the bad one.

// slowSink makes the write window wide enough that close() lands inside it.
type slowSink struct {
	delay time.Duration
	mu    sync.Mutex
	n     int
}

func (s *slowSink) SendBinary(b *upstream.Buf) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	b.Release()
}

func (s *slowSink) SendJSON(string, any) {}

func testFrame(target string, n int) *upstream.Buf {
	hdr := HeaderLenFor(target)
	b := upstream.GetBuf(hdr + n)
	b.B = b.B[:hdr+n]
	for i := hdr; i < len(b.B); i++ {
		b.B[i] = 'x'
	}
	return b
}

// TestCoalescerCloseDuringDrain closes a connection while its writer is
// mid-drain. Before the fix this reliably panicked with
// "upstream: Buf released more times than retained" (or tripped -race on the
// frames slice), because close() released buffers writeRun still owned.
func TestCoalescerCloseDuringDrain(t *testing.T) {
	const target = "alpha/w1:p1"
	hdr := HeaderLenFor(target)
	for round := 0; round < 12; round++ {
		sink := &slowSink{delay: 2 * time.Millisecond}
		c := newCoalescer(sink)
		c.started = true
		var seq seqCounter
		go c.run(&seq, nil, NewMetrics())

		// Frames OVER MaxWriteBytes, so writeRun emits them one at a time and
		// the drain is demonstrably still running when close() arrives. Small
		// frames coalesce into a single write and close the window the bug
		// lives in.
		for i := 0; i < 12; i++ {
			c.push(target, testFrame(target, MaxWriteBytes+1), hdr, false, time.Now())
		}
		// Keep producing while the close races the drain: this is the real
		// shape of the bug (a client disconnecting mid-stream).
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 12; i++ {
				c.push(target, testFrame(target, MaxWriteBytes+1), hdr, false, time.Now())
				time.Sleep(200 * time.Microsecond)
			}
		}()
		time.Sleep(3 * time.Millisecond)
		c.close()
		<-done
	}
}

// TestCoalescerCloseWaitsForWriter is the invariant the fix rests on: once
// close() returns, no writeRun can still be holding a buffer, so releasing the
// remainder is safe.
func TestCoalescerCloseWaitsForWriter(t *testing.T) {
	const target = "alpha/w1:p1"
	hdr := HeaderLenFor(target)
	sink := &slowSink{delay: time.Millisecond}
	c := newCoalescer(sink)
	c.started = true
	var seq seqCounter
	go c.run(&seq, nil, NewMetrics())
	for i := 0; i < 8; i++ {
		c.push(target, testFrame(target, MaxWriteBytes+1), hdr, false, time.Now())
	}
	c.close()
	select {
	case <-c.done:
	default:
		t.Fatal("close() returned while the writer goroutine was still running")
	}
}

// TestSessionCloseMidDrain walks the exact stack from the production panic:
// serve.handleStream -> Session.Close -> coalescer.close, racing the frames a
// stream handler is still pushing.
func TestSessionCloseMidDrain(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	for round := 0; round < 20; round++ {
		sink := &slowSink{delay: time.Millisecond}
		ctx, cancel := context.WithCancel(context.Background())
		sess := h.NewSession(ctx, sink)

		const target = "alpha/w1:p1"
		hdr := HeaderLenFor(target)
		ls := &liveStream{hdrLen: hdr, target: target}
		sh := &streamHandler{sess: sess, target: target, hdrLen: hdr, ls: ls}

		var wg sync.WaitGroup
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 12; i++ {
					sh.OnFrame(upstream.TerminalFrame{
						Data: testFrame(target, MaxWriteBytes+1), Off: hdr, Width: 80, Height: 24,
					})
					time.Sleep(200 * time.Microsecond)
				}
			}()
		}
		time.Sleep(time.Millisecond)
		sess.Close()
		wg.Wait()
		cancel()
	}
}

// TestViewportIsReconciledNotDiffed pins the second half of the silent-death
// bug: a target can be ModeLive in `modes` with no entry in `streams` (its
// upstream subprocess died), and re-sending the SAME viewport must be able to
// fix that. While SetViewport diffed against the old modes it was a no-op, so
// the client had no recovery path at all for the life of the connection.
func TestViewportIsReconciledNotDiffed(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &capSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "alpha/w1:p1"
	sess.mu.Lock()
	sess.modes[target] = ModeLive
	sess.mu.Unlock()

	if !sess.needsReconcile(target, ModeLive) {
		t.Fatal("a LIVE target with no stream must be reconciled, not skipped")
	}
	sess.mu.Lock()
	sess.streams[target] = &liveStream{hdrLen: HeaderLenFor(target), target: target}
	sess.mu.Unlock()
	if sess.needsReconcile(target, ModeLive) {
		t.Fatal("a LIVE target that already has a stream must not be restarted")
	}
	if sess.needsReconcile(target, ModeSummary) || sess.needsReconcile(target, ModeNone) {
		t.Fatal("only LIVE owns an upstream stream")
	}
}

// TestDeliberateStopIsNotAFailure: a viewport change, a geometry restart and a
// control upgrade all tear a stream down on purpose. Reporting those to the
// client as `closed`, or restarting them as if upstream had failed, is how a
// working pane looks broken.
func TestDeliberateStopIsNotAFailure(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &jsonSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "alpha/w1:p1"
	ls := &liveStream{hdrLen: HeaderLenFor(target), target: target}
	ls.stopped.Store(true)
	sh := &streamHandler{sess: sess, target: target, hdrLen: HeaderLenFor(target), ls: ls}
	sh.OnClosed("stream ended")
	if got := sink.kinds(); len(got) != 0 {
		t.Fatalf("a deliberate teardown told the client %v", got)
	}

	// An UNEXPECTED exit on a target nobody asked to be live is a real close.
	other := &liveStream{hdrLen: HeaderLenFor(target), target: target}
	sh2 := &streamHandler{sess: sess, target: target, hdrLen: HeaderLenFor(target), ls: other}
	sh2.OnClosed("terminal.closed")
	if got := sink.kinds(); len(got) != 1 || got[0] != "closed" {
		t.Fatalf("an upstream death must reach the client, got %v", got)
	}
}

// TestOnClosedOnlyRetiresItself: a late OnClosed from a stream we already
// replaced must not delete the replacement. That is what orphaned a running
// subprocess — frames still flowing from a stream the session no longer knew
// about, with modes[target] stuck on LIVE and no way back.
func TestOnClosedOnlyRetiresItself(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &jsonSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "alpha/w1:p1"
	old := &liveStream{hdrLen: HeaderLenFor(target), target: target}
	old.stopped.Store(true)
	fresh := &liveStream{hdrLen: HeaderLenFor(target), target: target}
	sess.mu.Lock()
	sess.modes[target] = ModeLive
	sess.streams[target] = fresh
	sess.mu.Unlock()

	(&streamHandler{sess: sess, target: target, hdrLen: HeaderLenFor(target), ls: old}).
		OnClosed("stream ended")

	sess.mu.Lock()
	cur := sess.streams[target]
	sess.mu.Unlock()
	if cur != fresh {
		t.Fatal("a late OnClosed from the OLD stream deleted the replacement")
	}
}

// jsonSink records control-plane frames.
type jsonSink struct {
	mu      sync.Mutex
	ks      []string
	reasons []string
}

func (s *jsonSink) SendBinary(b *upstream.Buf) { b.Release() }
func (s *jsonSink) SendJSON(typ string, data any) {
	s.mu.Lock()
	s.ks = append(s.ks, typ)
	if m, ok := data.(map[string]any); ok {
		if r, ok := m["reason"].(string); ok {
			s.reasons = append(s.reasons, r)
		}
	}
	s.mu.Unlock()
}

func (s *jsonSink) sawReason(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
func (s *jsonSink) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.ks...)
	s.ks = nil
	return out
}

// TestSupervisorNeverFailsSilently: when a LIVE target's upstream stream dies
// and cannot be brought back, the client must be TOLD. Six reproductions of the
// original bug ended with the client holding an open websocket, zero frames,
// zero gaps, zero `closed` and zero errors — believing it was live on a pane
// that had stopped streaming minutes earlier. Silence is the failure mode this
// test exists to forbid.
func TestSupervisorNeverFailsSilently(t *testing.T) {
	min, max, n := RestartBackoffMin, RestartBackoffMax, MaxRestarts
	RestartBackoffMin, RestartBackoffMax, MaxRestarts = time.Millisecond, 2*time.Millisecond, 3
	defer func() { RestartBackoffMin, RestartBackoffMax, MaxRestarts = min, max, n }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &jsonSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	// No such session upstream, so every restart attempt fails: the worst case.
	const target = "ghost/w1:p1"
	sess.mu.Lock()
	sess.modes[target] = ModeLive
	sess.mu.Unlock()

	ls := &liveStream{hdrLen: HeaderLenFor(target), target: target}
	sess.mu.Lock()
	sess.streams[target] = ls
	sess.mu.Unlock()
	(&streamHandler{sess: sess, target: target, hdrLen: HeaderLenFor(target), ls: ls}).
		OnClosed("terminal.closed")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sink.sawReason("could not be kept alive") {
		time.Sleep(5 * time.Millisecond)
	}
	if !sink.sawReason("could not be kept alive") {
		t.Fatal("a LIVE target died, could not be restarted, and the client was never told")
	}
	// Let the supervisor goroutine finish before the deferred restore writes
	// the tuning vars it is still reading.
	for time.Now().Before(deadline) {
		sess.mu.Lock()
		busy := len(sess.restarting)
		sess.mu.Unlock()
		if busy == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("supervisor goroutine never finished")
}
