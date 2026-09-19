package core

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// LOOKING MUST NOT TOUCH.
//
// The owner's complaint was "you are scrolling actual herdr terminal": opening
// a pane in the browser moved the pane he was working in on his laptop. These
// tests hold the two mechanisms that could do that.
//
// Measured on herdr 0.9.0, on a throwaway session, with `tput` inside the pane
// as the ground truth:
//
//	terminal session observe <pane>                      -> 120x40 unchanged
//	terminal session observe <pane> --cols 100 --rows 60 -> 120x40 unchanged
//	terminal session control <pane> --cols 100 --rows 60 --takeover
//	                                                     -> 120x40 BECOMES 100x60
//	                                                        and stays after detach
//	                                    (scroll.viewport_rows follows: 40 -> 60)
//
// So the mutating call is CONTROL, and the safe number to pass it is the one
// herdr itself reports for the pane. An observe with no --cols/--rows attaches
// at the pane's own size and reports it in the first frame's width/height,
// which is how we learn that number without having touched anything.

func newTestSession(t *testing.T) (*Session, func()) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	ctx, cancel := context.WithCancel(context.Background())
	sess := h.NewSession(ctx, &jsonSink{})
	return sess, func() {
		sess.Close()
		cancel()
	}
}

// A live attach that nobody asked to resize passes NO geometry at all. A zero
// geometry is not a missing value here; it is the only value that cannot
// disturb somebody else's terminal.
func TestLiveAttachRequestsNoGeometry(t *testing.T) {
	sess, done := newTestSession(t)
	defer done()

	const target = "s/w1:p1"
	sess.mu.Lock()
	sess.modes[target] = ModeLive
	g := sess.geometryFor(target)
	explicit := sess.explicit[target]
	sess.mu.Unlock()

	if explicit {
		t.Fatal("a target nobody resized must not be marked explicit")
	}
	if g.Cols != 0 || g.Rows != 0 {
		t.Fatalf("live attach would request %dx%d; it must request nothing so herdr uses the pane's own size", g.Cols, g.Rows)
	}
}

// The control upgrade — the one call that really does resize the owner's pane —
// must reuse the size herdr already told us the pane is. Same cols, same rows,
// nothing to SIGWINCH.
func TestControlUpgradeReusesThePanesOwnSize(t *testing.T) {
	sess, done := newTestSession(t)
	defer done()

	const target = "s/w1:p1"
	// What an observe stream's first frame reported: the pane is 120x40.
	sess.noteAttached(target, Geometry{Cols: 120, Rows: 40})

	sess.mu.Lock()
	g := sess.geometryFor(target)
	sess.mu.Unlock()
	if g.Cols != 120 || g.Rows != 40 {
		t.Fatalf("control upgrade would attach at %dx%d, not the pane's 120x40", g.Cols, g.Rows)
	}
}

// An EXPLICIT fit is still honoured — the capability is not removed, only the
// accident. And `match` gives the pane its size back.
func TestExplicitResizeThenMatch(t *testing.T) {
	sess, done := newTestSession(t)
	defer done()

	const target = "s/w1:p1"
	sess.noteAttached(target, Geometry{Cols: 120, Rows: 40})
	sess.SetGeometry(target, 80, 24)

	sess.mu.Lock()
	explicit := sess.explicit[target]
	g := sess.geometryFor(target)
	sess.mu.Unlock()
	if !explicit {
		t.Fatal("an explicit resize must mark the target explicit")
	}
	if g.Cols != 80 || g.Rows != 24 {
		t.Fatalf("explicit geometry not honoured: got %dx%d, want 80x24", g.Cols, g.Rows)
	}

	sess.MatchPane(target)
	sess.mu.Lock()
	explicit = sess.explicit[target]
	g = sess.geometryFor(target)
	sess.mu.Unlock()
	if explicit {
		t.Fatal("match must clear the explicit flag")
	}
	// Back to the pane's own reported size, not to 80x24 and not to a floor.
	if g.Cols != 120 || g.Rows != 40 {
		t.Fatalf("after match, attach geometry is %dx%d; want the pane's 120x40", g.Cols, g.Rows)
	}
}

// `pane.scroll` moves the pane's scrollback for EVERY client attached to it —
// measured: `{"pane_id":"w1:p2","offset_from_bottom":20}` sets it and it stays
// set. A connection that is only WATCHING must never be able to reach it. That
// call is the literal "you are scrolling actual herdr terminal".
func TestObserverCannotScrollUpstream(t *testing.T) {
	sess, done := newTestSession(t)
	defer done()

	const target = "s/w1:p1"
	sess.mu.Lock()
	sess.streams[target] = &liveStream{mode: upstream.ModeObserve, target: target}
	sess.mu.Unlock()

	if err := sess.Scroll(target, -20); err != ErrObserverScroll {
		t.Fatalf("observer scroll: got %v, want ErrObserverScroll", err)
	}
}

// A transcript subscriber has no stream at all, so it has nothing to scroll and
// must not invent one.
func TestTranscriptScrollIsANoOp(t *testing.T) {
	sess, done := newTestSession(t)
	defer done()

	const target = "s/w1:p1"
	sess.SetViewport(map[string]string{target: "transcript"})
	if err := sess.Scroll(target, -20); err != nil {
		t.Fatalf("scrolling a transcript target must be a silent no-op, got %v", err)
	}
	sess.mu.Lock()
	streams := len(sess.streams)
	sess.mu.Unlock()
	if streams != 0 {
		t.Fatalf("scrolling a transcript target started %d stream(s)", streams)
	}
}

// PaneSize reads the pane's size from the two places herdr actually reports it:
// cols from the tab layout (the only place a WIDTH appears anywhere in the
// 0.9.0 API) and rows from the pane's own scroll report, which tracks the PTY.
func TestSnapshotPaneSize(t *testing.T) {
	snap := &upstream.Snapshot{
		Panes: []upstream.Pane{
			{PaneID: "w1:p1", Scroll: &upstream.Scroll{ViewportRows: 40}},
			{PaneID: "w1:p2", Scroll: &upstream.Scroll{ViewportRows: 60}},
			{PaneID: "w1:p3"},
		},
		Layouts: []upstream.Layout{{
			TabID: "w1:t1",
			Area:  upstream.Rect{Width: 120, Height: 40},
			Panes: []upstream.LayoutPane{
				{PaneID: "w1:p1", Rect: upstream.Rect{Width: 60, Height: 40}},
				{PaneID: "w1:p2", Rect: upstream.Rect{Width: 60, Height: 40, X: 60}},
			},
		}},
	}
	for _, c := range []struct {
		id         string
		cols, rows int
	}{
		{"w1:p1", 60, 40},
		{"w1:p2", 60, 60},
		{"w1:p3", 0, 0},
		{"w9:p9", 0, 0},
	} {
		cols, rows := snap.PaneSize(c.id)
		if cols != c.cols || rows != c.rows {
			t.Errorf("PaneSize(%s) = %dx%d, want %dx%d", c.id, cols, rows, c.cols, c.rows)
		}
	}
}
