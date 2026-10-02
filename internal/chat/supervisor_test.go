package chat

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func shrinkTimings(t *testing.T) {
	t.Helper()
	ob, oh := RestartBackoff, HealthyRun
	RestartBackoff, HealthyRun = time.Millisecond, time.Hour
	t.Cleanup(func() { RestartBackoff, HealthyRun = ob, oh })
}

type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, strings.TrimSpace(sprintf(format, args...)))
}

func (r *recorder) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// An adapter that fails identically every time must be left DOWN, and the
// supervisor must then do nothing at all.
//
// This is the crash loop the bound exists for: a bad token or a syntax error
// fails the same way forever, and a supervisor that keeps trying turns one
// mistake into a process spawned every few seconds, filling the log with the
// same line and -- on a metered chat API -- burning the rate limit that would
// have let the fixed adapter connect.
func TestGivesUpAfterMaxRestartsAndThenDoesNothing(t *testing.T) {
	shrinkTimings(t)
	rec := &recorder{}
	// `false` exits non-zero immediately: a crash loop with no output.
	s := NewSupervisor(Spec{ID: "broken", Argv: []string{"false"}}, rec.logf, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := s.Run(ctx)

	var g *GiveUp
	if !asGiveUp(err, &g) {
		t.Fatalf("Run returned %v, want a *GiveUp", err)
	}
	if g.Restarts != MaxRestarts {
		t.Errorf("gave up after %d restarts, want %d", g.Restarts, MaxRestarts)
	}
	if s.GaveUp() == nil {
		t.Error("GaveUp() is nil after giving up")
	}

	// Said ONCE. The log must not become the crash loop it was meant to
	// report.
	if n := rec.count("not trying again"); n != 1 {
		t.Errorf("give-up logged %d times, want exactly 1", n)
	}

	// And nothing further happens: Run has returned, so no process is
	// spawned again, and a send into a dead adapter is a no-op rather than an
	// error pushed back at whoever was talking.
	if err := s.Send(Outbound{Type: "send", Text: "anything"}); err != nil {
		t.Errorf("Send to a given-up adapter returned %v, want nil", err)
	}
}

// Cancelling is a clean shutdown, not a failure, and must not count as a
// restart or trip the give-up.
func TestCancelIsNotAFailure(t *testing.T) {
	shrinkTimings(t)
	s := NewSupervisor(Spec{ID: "sleeper", Argv: []string{"sleep", "30"}}, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if g := s.GaveUp(); g != nil {
		t.Errorf("cancelling tripped the give-up: %v", g)
	}
}

// Constructing a supervisor must never start a process: listing or inspecting
// adapters would otherwise launch every one of them.
func TestNewSupervisorStartsNothing(t *testing.T) {
	rec := &recorder{}
	_ = NewSupervisor(Spec{ID: "x", Argv: []string{"false"}}, rec.logf, nil)
	time.Sleep(100 * time.Millisecond)
	if len(rec.lines) != 0 {
		t.Errorf("constructing a supervisor produced %v", rec.lines)
	}
}

// stdout is the protocol; each line arrives as one frame.
func TestStdoutLinesArriveAsFrames(t *testing.T) {
	shrinkTimings(t)
	var mu sync.Mutex
	var got []string
	s := NewSupervisor(
		Spec{ID: "talker", Argv: []string{"printf", `{"type":"ready"}\n{"type":"message","text":"hi"}\n`}},
		nil,
		func(b []byte) { mu.Lock(); got = append(got, string(b)); mu.Unlock() },
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Run(ctx) // it exits, so it will eventually give up; we only want the lines

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("got %d frames %q, want at least 2", len(got), got)
	}
	if !strings.Contains(got[0], `"ready"`) || !strings.Contains(got[1], `"hi"`) {
		t.Errorf("frames = %q", got)
	}
}
