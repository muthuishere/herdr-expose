package serve

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// Two tree pushes that overlap used to be a FATAL crash, not a flake.
//
// sendTree ran `go s.emitAgents(...)` per push, and emitAgents wrote the plain
// map conn.agentState while the reader goroutine flipped conn.first. Tree
// pushes are driven by upstream events across every attached Herdr session, so
// two of them landing close together ran two writers over the same map — and a
// concurrent map write is a runtime throw with no recover, which takes the
// daemon down with every session on it.
//
// These tests need -race to be meaningful; `go test -race ./internal/serve/...`
// is where they earn their keep.

// raceConn builds a wsConn with no real socket. Nothing here writes to the
// websocket: SendJSON only needs the outbound queue, which a drainer empties so
// the queue-full path (which would close a nil *websocket.Conn) is never hit.
func raceConn(t *testing.T, s *Server, ctx context.Context) (*wsConn, func() int) {
	t.Helper()
	conn := &wsConn{
		log:        s.log,
		id:         "racetest",
		who:        Identity{Kind: "local", Name: "loopback"},
		agentState: map[string]string{},
		first:      true,
		treeReq:    make(chan struct{}, 1),
		out:        make(chan outMsg, SendQueueDepth),
		done:       make(chan struct{}),
	}
	conn.sess = s.hub.NewSession(ctx, conn)

	var mu sync.Mutex
	seen := 0
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-conn.out:
				if m.buf != nil {
					m.buf.Release()
					continue
				}
				mu.Lock()
				seen++
				mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() { <-drained })
	return conn, func() int {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// agentTreeView is a tree with panes that carry agents, so emitAgents has
// something to record. The store in a unit test has no upstream sessions, so
// the view is built by hand rather than pushed through Herdr.
func agentTreeView(state string) TreeView {
	panes := make([]PaneView, 0, 6)
	for i := 0; i < 6; i++ {
		panes = append(panes, PaneView{
			ID:    "sess/w1:t1:p" + string(rune('1'+i)),
			Title: "pane",
			Agent: &AgentView{ID: "term" + string(rune('1'+i)), Kind: "claude", State: state},
		})
	}
	return TreeView{
		Rev: 1, Connected: true,
		FocusedSession: "sess",
		Sessions: []SessionView{{
			ID: "sess", Name: "sess", Running: true, Connected: true, Focused: true,
			Workspaces: []WorkspaceView{{
				ID: "sess/w1", Label: "ws", Number: 1, Focused: true,
				Tabs: []TabView{{
					ID: "sess/w1:t1", Label: "tab", Number: 1, Focused: true, Panes: panes,
				}},
			}},
		}},
	}
}

// TestConcurrentTreePushesDoNotRaceAgentState is the regression test for the
// fatal: it runs overlapping tree pushes over one connection's agent state.
//
// On the pre-fix code (`go s.emitAgents(...)` writing a plain map) this fails
// under -race, and on an unlucky schedule crashes outright with "concurrent map
// writes". `blocked` is in the rotation on purpose: it is the state that takes
// emitAgents down the store.Resolve / PaneRead branch.
func TestConcurrentTreePushesDoNotRaceAgentState(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, _ := raceConn(t, s, ctx)

	states := []string{AgentWorking, AgentIdle, AgentBlocked, AgentDone}
	views := make([]TreeView, len(states))
	for i, st := range states {
		views[i] = agentTreeView(st)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; n < 150; n++ {
				s.emitAgents(ctx, conn, views[(g+n)%len(views)], conn.takeFirst())
			}
		}(g)
	}
	wg.Wait()

	// The seen-set is PER CONNECTION (SPEC §6 / B-seen) and `done` is derived
	// from it server-side; the pushes above must not have leaked it anywhere.
	if conn.sess.Seen.Unseen("sess/w1:t1:p1", 1) != true {
		t.Fatal("an unacknowledged doneSeq must still read as unseen")
	}
}

// TestTreeLoopSerialisesPushes drives the real push path: one treeLoop owning
// the connection's tree state, with every other goroutine going through
// requestTree. Watch signals and reader-goroutine requests are fired together,
// which is exactly the interleaving the old code handled with a goroutine per
// push.
func TestTreeLoopSerialisesPushes(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, frames := raceConn(t, s, ctx)

	treeCh := make(chan struct{}, 1)
	loop := make(chan struct{})
	go func() { defer close(loop); s.treeLoop(ctx, conn, treeCh) }()

	var wg sync.WaitGroup
	// The watch goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			select {
			case treeCh <- struct{}{}:
			default:
			}
		}
	}()
	// The reader goroutine: `hello` and `seen` both ask for a push, and `seen`
	// mutates the per-connection seen set first.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				s.handleControl(ctx, conn, []byte(`{"type":"hello","data":{}}`))
				s.handleControl(ctx, conn,
					json.RawMessage(`{"type":"seen","data":{"target":"sess/w1:t1:p1"}}`))
			}
		}()
	}
	wg.Wait()

	// Let the loop settle, then stop it.
	deadline := time.Now().Add(2 * time.Second)
	for frames() == 0 && time.Now().Before(deadline) {
		select {
		case treeCh <- struct{}{}:
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if frames() == 0 {
		t.Fatal("treeLoop pushed no frames")
	}
	close(conn.done)
	select {
	case <-loop:
	case <-time.After(2 * time.Second):
		t.Fatal("treeLoop did not exit when the connection closed")
	}
}

// An UNCHANGED tree is not pushed, and `rev` alone is not a change.
//
// Measured on an idle 12-session bed: 44 tree frames in 12s, 12.5KB each, one
// distinct payload among them, and `rev` the only field that ever differed —
// so 97% of the bytes carried no information and the bump defeated client-side
// dedup as well. The store now refuses to publish an identical tree, but the
// tree it holds carries fields no view renders (herdr bumps Pane.Revision on
// every byte a pane emits), so the decision that actually reaches the wire has
// to be made HERE, on the rendered view.
func TestUnchangedTreeIsNotPushed(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, seen := raceConn(t, s, ctx)
	defer conn.sess.Close()

	s.sendTree(ctx, conn)
	waitForCount(t, seen, 1)

	for i := 0; i < 20; i++ {
		s.sendTree(ctx, conn)
	}
	time.Sleep(120 * time.Millisecond)
	if got := seen(); got != 1 {
		t.Fatalf("%d control frames after 21 identical tree pushes, want 1", got)
	}

	// A real change still goes out on the very next push.
	conn.treeHash, conn.treeSent = 0, false // stands in for "the view changed"
	s.sendTree(ctx, conn)
	waitForCount(t, seen, 2)
}

func waitForCount(t *testing.T, seen func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if seen() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("saw %d control frames, want %d", seen(), want)
}
