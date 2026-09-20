package expose

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// procSpec describes a supervised external tunnel process (cloudflared, or
// whatever binary a JS adapter spawns).
// Build is called once per launch attempt so that per-attempt credentials (a
// freshly fetched tunnel token, say) are never cached on disk or in a struct.
type procSpec struct {
	Name      string // provider name, e.g. "cloudflare"
	Mode      string // "named" | "quick" | "js"
	Bin       string // resolved binary path, for error messages only
	Build     func(ctx context.Context) (*exec.Cmd, error)
	ScanURL   func(line string) string // returns a public URL found in a log line, or ""
	StaticURL string                   // known up front (named tunnels)

	// Verify delays publishing a URL until https://<host>/healthz actually
	// answers (C2 step 7): a process that started is not a tunnel that works.
	//
	// With StaticURL set it verifies that hostname. With only ScanURL set (a
	// quick tunnel, whose hostname is assigned by the edge at start) it
	// verifies whatever the scanner scraped, and re-verifies a NEW hostname
	// after a restart — a quick tunnel does not come back on the same name.
	Verify        bool
	VerifyTimeout time.Duration
	Log           func(format string, args ...any)
	Redact        *redactor

	// HealthInterval > 0 enables a periodic GET of the public URL. A failing
	// probe is reported, never acted on: the tunnel edge rate-limiting us is
	// not a reason to kill a working process.
	HealthInterval time.Duration

	// Print is what this provider CREATES outside the process, declared up
	// front so teardown is symmetric with creation even when creation was
	// interrupted (see Footprint).
	Print Footprint
	// Teardown removes Print. nil means there is nothing to remove, which is
	// the honest answer for every ephemeral provider.
	Teardown func(ctx context.Context, logf func(format string, args ...any)) error
}

// procTunnel supervises one external process: it restarts it with backoff when
// it dies, scrapes the public URL out of its output, and shuts down cleanly and
// idempotently.
type procTunnel struct {
	spec procSpec

	mu        sync.RWMutex
	url       string
	pid       int
	healthy   bool
	restarts  int
	lastErr   string
	startedAt time.Time
	running   bool

	cancel   context.CancelFunc
	done     chan struct{}
	urlReady chan struct{}
	stopOnce sync.Once

	// candidates carries scraped-but-unverified URLs to verifyLoop. Buffered
	// so the output scanner never blocks on it.
	candidates chan string
	// offered is the last URL handed to verifyLoop, so a hostname repeated on
	// every log line is not re-probed.
	offered string
}

func newProcTunnel(spec procSpec) *procTunnel {
	if spec.Log == nil {
		spec.Log = func(string, ...any) {}
	}
	if spec.Redact == nil {
		spec.Redact = &redactor{}
	}
	return &procTunnel{
		spec:       spec,
		done:       make(chan struct{}),
		urlReady:   make(chan struct{}),
		candidates: make(chan string, 4),
	}
}

func (p *procTunnel) Name() string { return p.spec.Name }
func (p *procTunnel) Mode() string { return p.spec.Mode }

// Footprint is what this provider creates outside the process.
func (p *procTunnel) Footprint() Footprint { return p.spec.Print }

// Destroy stops the process and then removes exactly what Footprint declares.
//
// Stopping FIRST is not tidiness: Cloudflare refuses to delete a tunnel that
// still has a connector registered, so a Destroy that raced its own process
// would delete the DNS record and orphan the tunnel — the precise half-teardown
// this is meant to prevent. It is idempotent at both halves: Stop is a no-op
// the second time, and every Teardown re-reads the world and treats "already
// absent" as success.
func (p *procTunnel) Destroy(ctx context.Context, logf func(string, ...any)) error {
	if logf == nil {
		logf = p.spec.Log
	}
	_ = p.Stop()
	if p.spec.Teardown == nil {
		logf("%s: nothing to destroy — %s", p.spec.Name, p.spec.Print.Describe())
		return nil
	}
	return p.spec.Teardown(ctx, logf)
}

// Launch starts the supervision loop. It returns as soon as the first process
// has been spawned; use WaitForURL to block until a public URL is known.
func (p *procTunnel) Launch(parent context.Context) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	p.mu.Lock()
	p.cancel = cancel
	p.running = true
	p.startedAt = time.Now()
	if p.spec.StaticURL != "" && !p.spec.Verify {
		p.url = p.spec.StaticURL
	}
	p.mu.Unlock()
	if p.spec.StaticURL != "" && !p.spec.Verify {
		p.signalURL()
	}

	// First attempt synchronously so that a missing binary or a bad flag is a
	// real error from `expose start` instead of a background mystery.
	cmd, err := p.spec.Build(ctx)
	if err != nil {
		cancel()
		p.markStopped(err.Error())
		return err
	}
	wait, err := p.spawn(cmd)
	if err != nil {
		cancel()
		p.markStopped(err.Error())
		return err
	}

	go p.supervise(ctx, wait)
	if p.spec.Verify {
		go p.verifyLoop(ctx)
	}
	if p.spec.HealthInterval > 0 {
		go p.healthLoop(ctx)
	}
	return nil
}

// spawn starts cmd, wires up line scanning, and returns a wait function.
func (p *procTunnel) spawn(cmd *exec.Cmd) (func() error, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stdout pipe: %w", p.spec.Name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stderr pipe: %w", p.spec.Name, err)
	}
	// Own the whole process group so that a kill takes children with it.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", p.spec.Bin, err)
	}

	p.mu.Lock()
	p.pid = cmd.Process.Pid
	p.mu.Unlock()
	p.spec.Log("%s: started %s (pid %d)", p.spec.Name, p.spec.Bin, cmd.Process.Pid)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.scan(stdout) }()
	go func() { defer wg.Done(); p.scan(stderr) }()

	return func() error {
		wg.Wait() // drain the pipes before reaping
		return cmd.Wait()
	}, nil
}

// scan reads the child's output a line at a time, looking for the public URL.
// Every line is scrubbed before it reaches a log sink.
func (p *procTunnel) scan(r io.Reader) {
	defer func() {
		if rec := recover(); rec != nil {
			p.spec.Log("%s: output scanner recovered from panic: %v", p.spec.Name, rec)
		}
	}()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if p.spec.ScanURL != nil {
			if u := p.spec.ScanURL(line); u != "" {
				p.offerURL(u)
			}
		}
		p.spec.Log("%s: %s", p.spec.Name, p.spec.Redact.scrub(strings.TrimSpace(line)))
	}
}

// supervise waits on the current process and restarts it with capped backoff.
func (p *procTunnel) supervise(ctx context.Context, wait func() error) {
	defer close(p.done)
	defer func() {
		if rec := recover(); rec != nil {
			p.spec.Log("%s: supervisor recovered from panic: %v", p.spec.Name, rec)
			p.markStopped(fmt.Sprintf("supervisor panic: %v", rec))
		}
	}()

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		started := time.Now()
		err := wait()
		if ctx.Err() != nil {
			p.markStopped("")
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second // it ran fine for a while; treat this as fresh
		}
		msg := "exited"
		if err != nil {
			msg = err.Error()
		}
		p.mu.Lock()
		p.restarts++
		p.lastErr = msg
		p.healthy = false
		restarts := p.restarts
		p.mu.Unlock()
		p.spec.Log("%s: process %s; restarting in %s (restart #%d)", p.spec.Name, msg, backoff, restarts)

		select {
		case <-ctx.Done():
			p.markStopped("")
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}

		cmd, err := p.spec.Build(ctx)
		if err != nil {
			p.spec.Log("%s: cannot rebuild command: %v", p.spec.Name, p.spec.Redact.scrub(err.Error()))
			p.mu.Lock()
			p.lastErr = p.spec.Redact.scrub(err.Error())
			p.mu.Unlock()
			continue
		}
		wait, err = p.spawn(cmd)
		if err != nil {
			p.spec.Log("%s: restart failed: %v", p.spec.Name, err)
			p.mu.Lock()
			p.lastErr = err.Error()
			p.mu.Unlock()
			wait = func() error { return errors.New("not started") }
			continue
		}
	}
}

// offerURL takes a URL scraped from the child's output. Without verification
// it is published immediately; with it, verifyLoop probes it first, because a
// hostname cloudflared has printed is not yet a hostname the edge routes.
func (p *procTunnel) offerURL(u string) {
	if !p.spec.Verify {
		p.setURL(u)
		return
	}
	p.mu.Lock()
	if p.offered == u {
		p.mu.Unlock()
		return // the same hostname repeated across log lines
	}
	p.offered = u
	p.mu.Unlock()
	select {
	case p.candidates <- u:
	default: // verifyLoop is busy with one; it re-reads the latest on its next pass
	}
}

// verifyLoop publishes a URL only once <url>/healthz answers through it.
//
// A static hostname is known up front, so it is probed straight away. A quick
// tunnel's is not: the loop waits for the scanner to scrape one, probes that,
// and then keeps waiting — a restarted quick tunnel is assigned a DIFFERENT
// hostname, and continuing to advertise the dead one is worse than briefly
// having none.
func (p *procTunnel) verifyLoop(ctx context.Context) {
	timeout := p.spec.VerifyTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	target := p.spec.StaticURL
	deadline := time.Now().Add(timeout)
	published := false
	if target != "" {
		p.spec.Log("%s: waiting for %s to answer", p.spec.Name, target)
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if target == "" {
			// Nothing to probe yet. Before the first publish this is on the
			// clock (a tunnel that never names itself is a failed start);
			// afterwards it simply waits for the next restart's hostname.
			var giveUp <-chan time.Time
			if !published {
				giveUp = time.After(time.Until(deadline))
			}
			select {
			case <-ctx.Done():
				return
			case u := <-p.candidates:
				target = u
				deadline = time.Now().Add(timeout)
				p.spec.Log("%s: edge assigned %s; waiting for it to answer", p.spec.Name, u)
			case <-giveUp:
				p.noteVerifyTimeout("no public hostname was reported", timeout)
				return
			}
			continue
		}

		if probe(ctx, target) {
			p.setURL(target)
			if p.spec.ScanURL == nil {
				return // a static hostname is verified once and stays put
			}
			published, target = true, ""
			continue
		}

		// A newer hostname supersedes the one being probed: after a restart
		// the old one will never answer again.
		select {
		case <-ctx.Done():
			return
		case u := <-p.candidates:
			target = u
			deadline = time.Now().Add(timeout)
			continue
		case <-time.After(2 * time.Second):
		}
		if time.Now().After(deadline) {
			p.noteVerifyTimeout(target+" did not answer", timeout)
			if !published {
				return
			}
			target = "" // go back to waiting for a fresh hostname
		}
	}
}

func (p *procTunnel) noteVerifyTimeout(what string, timeout time.Duration) {
	p.mu.Lock()
	if p.lastErr == "" {
		p.lastErr = fmt.Sprintf("%s within %s", what, timeout)
	}
	p.mu.Unlock()
}

// healthLoop probes the public URL. It never kills the process; it only marks
// the status so the UI can show a degraded tunnel.
func (p *procTunnel) healthLoop(ctx context.Context) {
	t := time.NewTicker(p.spec.HealthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u := p.URL()
			if u == "" {
				continue
			}
			ok := probe(ctx, u)
			p.mu.Lock()
			p.healthy = ok
			p.mu.Unlock()
		}
	}
}

func (p *procTunnel) setURL(u string) {
	p.mu.Lock()
	changed := p.url != u
	p.url = u
	p.healthy = true
	p.mu.Unlock()
	if changed {
		p.spec.Log("%s: public url %s", p.spec.Name, u)
	}
	p.signalURL()
}

func (p *procTunnel) signalURL() {
	select {
	case <-p.urlReady:
	default:
		close(p.urlReady)
	}
}

func (p *procTunnel) markStopped(lastErr string) {
	p.mu.Lock()
	p.running = false
	p.pid = 0
	p.healthy = false
	if lastErr != "" {
		p.lastErr = p.spec.Redact.scrub(lastErr)
	}
	p.mu.Unlock()
}

// URL is the current public URL, empty until one is known.
func (p *procTunnel) URL() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.url
}

// WaitForURL blocks until a public URL is known, the tunnel dies, or the
// deadline passes.
func (p *procTunnel) WaitForURL(ctx context.Context, timeout time.Duration) (string, error) {
	select {
	case <-p.urlReady:
		return p.URL(), nil
	case <-p.done:
		return "", fmt.Errorf("%s exited before reporting a url: %s", p.spec.Name, p.Snapshot().LastError)
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(timeout):
		return "", fmt.Errorf("%s did not report a public url within %s", p.spec.Name, timeout)
	}
}

// Snapshot returns the status. It deliberately carries no credentials.
func (p *procTunnel) Snapshot() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return Status{
		Provider:  p.spec.Name,
		Tunnel:    p.spec.Mode,
		Running:   p.running,
		Healthy:   p.healthy,
		URL:       p.url,
		PID:       p.pid,
		Restarts:  p.restarts,
		StartedAt: p.startedAt,
		LastError: p.lastErr,
	}
}

// Stop is idempotent: calling it twice, or after the process already died, is
// a no-op that still returns nil.
func (p *procTunnel) Stop() error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		cancel := p.cancel
		pid := p.pid
		p.running = false
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		killProcessGroup(pid, p.spec.Log)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			p.spec.Log("%s: supervisor did not stop within 5s", p.spec.Name)
		}
		p.markStopped("")
		p.spec.Log("%s: stopped", p.spec.Name)
	})
	return nil
}

// killProcessGroup sends SIGTERM to the whole group, then SIGKILL if needed.
func killProcessGroup(pid int, logf func(string, ...any)) {
	if pid <= 0 {
		return
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = pid
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	if logf != nil {
		logf("process group %d ignored SIGTERM; sending SIGKILL", pgid)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
