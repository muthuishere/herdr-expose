package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/muthuishere/herdr-expose/internal/platform"
	"sync"
	"time"
)

// Supervision policy.
//
// An adapter talks to somebody else's server over somebody else's network, so
// it WILL exit: a dropped connection, a 429, a rolled bot token, an uncaught
// promise. Restarting it is correct. Restarting it forever is not.
const (
	// MaxRestarts is how many times one adapter is restarted before it is
	// left down for good.
	//
	// The failure this bounds is a crash loop: a bad token or a syntax error
	// fails identically every time, and a supervisor that keeps trying turns
	// one mistake into a process spawned every few seconds until somebody
	// notices -- filling the log with the same line, and on a metered API
	// burning the rate limit that would have let the FIXED adapter connect.
	MaxRestarts = 5
)

// Timings. Variables, not constants, so tests can shrink them -- the same
// reason internal/expose does it for its JS call budgets.
var (
	// RestartBackoff is the wait before the first restart. It doubles, so five
	// restarts span about a minute rather than five instants: an adapter that
	// died because the network blinked gets time for the network to come back.
	RestartBackoff = 2 * time.Second

	// HealthyRun is how long an adapter must stay up before its restart count
	// is forgiven. Without this, an adapter that works fine for a week and
	// then has its fifth bad night is permanently down, which is a supervisor
	// punishing something for surviving.
	HealthyRun = 2 * time.Minute
)

// GiveUp is why a supervisor stopped trying. It is a state, not an error:
// the adapter is DOWN and nothing further will happen until a person acts.
type GiveUp struct {
	AdapterID string
	Restarts  int
	LastErr   error
}

func (g *GiveUp) Error() string {
	return fmt.Sprintf("adapter %s stayed down after %d restarts; not trying again (last: %v)",
		g.AdapterID, g.Restarts, g.LastErr)
}

// Spec is everything needed to run one adapter.
type Spec struct {
	ID string
	// Argv is already split; it is executed DIRECTLY, never through a shell.
	Argv []string
	// Dir is the working directory, normally the chat adapters directory.
	Dir string
	// Env is the child's environment, with $NAME already expanded.
	Env []string
}

// Logf is a log sink. The supervisor passes every line through it already
// scrubbed of secrets by the caller's redactor.
type Logf func(format string, args ...any)

// Supervisor runs one adapter and keeps it running, within limits.
type Supervisor struct {
	spec Spec
	logf Logf

	// onLine receives each protocol line the adapter printed on stdout.
	onLine func([]byte)
	// stdin is how frames are written to the running child.
	mu       sync.Mutex
	stdin    io.WriteCloser
	restarts int
	gaveUp   *GiveUp
}

// NewSupervisor builds one. Nothing runs until Run is called: constructing a
// supervisor must never start a process, so that listing or inspecting
// adapters cannot accidentally launch them.
func NewSupervisor(spec Spec, logf Logf, onLine func([]byte)) *Supervisor {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if onLine == nil {
		onLine = func([]byte) {}
	}
	return &Supervisor{spec: spec, logf: logf, onLine: onLine}
}

// GaveUp reports the give-up state, or nil while the supervisor is still
// willing to try.
func (s *Supervisor) GaveUp() *GiveUp {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gaveUp
}

// Send writes one frame to the running adapter. It is a no-op when the adapter
// is down -- a chat message that cannot be delivered because the adapter is
// dead is not an error worth propagating into the agent's pane.
func (s *Supervisor) Send(v any) error {
	line, err := Encode(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	w := s.stdin
	s.mu.Unlock()
	if w == nil {
		return nil
	}
	_, err = w.Write(line)
	return err
}

// Run supervises until ctx is cancelled, or until it gives up.
//
// It returns nil on a clean shutdown and a *GiveUp when it stopped trying.
func (s *Supervisor) Run(ctx context.Context) error {
	backoff := RestartBackoff
	var lastErr error

	for {
		started := time.Now()
		err := s.runOnce(ctx)
		ran := time.Since(started)

		if ctx.Err() != nil {
			return nil // asked to stop; not a failure
		}
		lastErr = err

		// A long, healthy run earns a clean slate.
		if ran >= HealthyRun {
			s.mu.Lock()
			s.restarts = 0
			s.mu.Unlock()
			backoff = RestartBackoff
		}

		s.mu.Lock()
		s.restarts++
		n := s.restarts
		s.mu.Unlock()

		if n > MaxRestarts {
			g := &GiveUp{AdapterID: s.spec.ID, Restarts: n - 1, LastErr: lastErr}
			s.mu.Lock()
			s.gaveUp = g
			s.mu.Unlock()
			// Said once, plainly, and then nothing. The log must not become
			// the crash loop it was meant to report.
			s.logf("chat adapter %s: %v", s.spec.ID, g)
			return g
		}

		s.logf("chat adapter %s exited after %s (%v); restart %d of %d in %s",
			s.spec.ID, ran.Round(time.Millisecond), lastErr, n, MaxRestarts, backoff)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 32*time.Second {
			backoff *= 2
		}
	}
}

// runOnce starts the adapter and returns when it exits.
func (s *Supervisor) runOnce(ctx context.Context) error {
	if len(s.spec.Argv) == 0 {
		return fmt.Errorf("no command")
	}
	// exec.Command, not a shell: a config file must not be able to smuggle a
	// pipe or a command substitution into a process running as the owner.
	cmd := exec.CommandContext(ctx, s.spec.Argv[0], s.spec.Argv[1:]...)
	cmd.Dir = s.spec.Dir
	cmd.Env = s.spec.Env

	// Own the whole TREE, not just the child.
	//
	// `node telegram.js` is one process today and two the moment somebody's
	// adapter shells out to curl or spawns a worker. Killing only the direct
	// child then leaves the grandchild holding the long poll -- on Windows
	// especially, where terminating a process does nothing to its children, so
	// a restarted adapter would quietly end up with two pollers answering the
	// same chat. PrepareGroup is a process group on Unix and a job object on
	// Windows; AdoptGroup must follow Start, and both are no-ops on the
	// platform that does not need them.
	platform.PrepareGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.spec.Argv[0], err)
	}
	group, gerr := platform.AdoptGroup(cmd)
	if gerr != nil {
		// Not fatal: an un-adopted child still runs and is still killed
		// directly. Say so, because the consequence is subtle -- a leftover
		// grandchild on the next restart.
		s.logf("chat adapter %s: could not adopt the process group: %v", s.spec.ID, gerr)
	}
	defer func() {
		if group != nil {
			group.Close()
		}
	}()

	s.mu.Lock()
	s.stdin = stdin
	s.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)

	// stdout is the protocol.
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		// A transcript screen is the biggest thing on this wire, and the
		// default 64KB token would truncate one into invalid JSON.
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			if len(line) > 0 {
				s.onLine(line)
			}
		}
	}()

	// stderr is NOT the protocol: it is the adapter's own debugging, captured
	// so a plain print cannot corrupt the stream it speaks on.
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if t := sc.Text(); t != "" {
				s.logf("chat adapter %s: %s", s.spec.ID, t)
			}
		}
	}()

	// Context cancellation kills the direct child; the TREE needs this.
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			platform.KillTree(cmd.Process.Pid, group, s.logf)
		}
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	s.mu.Lock()
	s.stdin = nil
	s.mu.Unlock()
	_ = stdin.Close()

	return waitErr
}
