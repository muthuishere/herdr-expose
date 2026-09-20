package expose

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dop251/goja"
)

// JS adapters are the ESCAPE HATCH (AMENDMENT A2), not the happy path: use
// them for tailscale, a corporate proxy or someone's homelab. Cloudflare and
// ngrok are built in and need no JavaScript.
//
// The runtime is goja — embedded, no node at runtime, and deliberately bare:
// there is no require, no fs, no fetch, no timers and no network primitive in
// the VM. An adapter's only way to reach the world is ctx.spawn(), i.e. running
// a real tool. Everything the adapter can do, the host can see.
//
// Safety properties, all enforced here rather than trusted to the script:
//   - every call into JS runs with a wall-clock interrupt, so a `while(true)`
//     stalls one adapter and nothing else;
//   - every call is wrapped in panic recovery, so a throwing adapter cannot
//     take down the server;
//   - all JS runs on ONE goroutine (goja runtimes are not goroutine-safe);
//   - Stop() kills every process the adapter spawned even if stop() throws,
//     hangs or was never defined, and is idempotent.

// Wall-clock budgets for calls into JS. Variables, not constants, so tests can
// shrink them.
var (
	jsCallTimeout = 20 * time.Second
	jsStopTimeout = 10 * time.Second
)

// exportRe turns `export function f(){}` / `export const x = ...` into plain
// declarations. goja has no ES module loader; adapters are written in the
// module style the spec shows, so normalise instead of demanding a different
// dialect.
var exportRe = regexp.MustCompile(`(?m)^[ \t]*export[ \t]+(default[ \t]+)?(?:(async[ \t]+)?function|const|let|var|class)\b`)

type jsTunnel struct {
	id     string
	script string
	logf   func(string, ...any)
	red    *redactor

	jobs chan func(*goja.Runtime)
	loop sync.WaitGroup

	vm       *goja.Runtime
	ctxObj   *goja.Object
	fnStart  goja.Callable
	fnStatus goja.Callable
	fnStop   goja.Callable

	mu        sync.RWMutex
	url       string
	localURL  string
	procs     []*jsProc
	handlers  map[int][]goja.Callable
	lastErr   string
	running   bool
	startedAt time.Time

	urlReady chan struct{}
	closed   chan struct{}
	stopOnce sync.Once
}

type jsProc struct {
	cmd *exec.Cmd
	pid int
	// dead is atomic, not guarded by jsTunnel.mu. The reaper goroutine writes
	// it while killAll() reads it from INSIDE a ctx.kill() call on the JS
	// goroutine, and killAll deliberately drops the lock before it starts
	// signalling process groups (killing under the lock would block every
	// spawn, status and URL read for as long as the kills take). A plain bool
	// there is an unsynchronised read of a field another goroutine is writing.
	dead atomic.Bool
}

// newJSTunnel loads and compiles an adapter. Nothing runs until Launch.
func newJSTunnel(id, scriptPath, localURL string, adapterConfig map[string]string, logf func(string, ...any), red *redactor) (*jsTunnel, error) {
	src, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("adapter %q: %w", id, err)
	}
	j := &jsTunnel{
		id:       id,
		script:   scriptPath,
		logf:     logf,
		red:      red,
		jobs:     make(chan func(*goja.Runtime), 64),
		handlers: map[int][]goja.Callable{},
		localURL: localURL,
		urlReady: make(chan struct{}),
		closed:   make(chan struct{}),
	}

	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	j.vm = vm
	if err := j.buildContext(adapterConfig); err != nil {
		return nil, err
	}

	normalised := exportRe.ReplaceAllStringFunc(string(src), func(m string) string {
		m = strings.Replace(m, "export", "", 1)
		return strings.Replace(m, "default", "", 1)
	})
	prog, err := goja.Compile(scriptPath, normalised, false)
	if err != nil {
		return nil, fmt.Errorf("adapter %q: compile: %w", id, err)
	}

	// Run the module body and pick up its three entry points. Supports both
	// `export function start()` and `module.exports = { start }`.
	module := vm.NewObject()
	exports := vm.NewObject()
	_ = module.Set("exports", exports)
	_ = vm.Set("module", module)
	_ = vm.Set("exports", exports)

	j.startLoop()
	err = j.run(func(rt *goja.Runtime) error {
		if _, err := rt.RunProgram(prog); err != nil {
			return err
		}
		exp, _ := module.Get("exports").(*goja.Object)
		pick := func(name string) goja.Callable {
			if exp != nil {
				if fn, ok := goja.AssertFunction(exp.Get(name)); ok {
					return fn
				}
			}
			if fn, ok := goja.AssertFunction(rt.GlobalObject().Get(name)); ok {
				return fn
			}
			return nil
		}
		j.fnStart, j.fnStatus, j.fnStop = pick("start"), pick("status"), pick("stop")
		if j.fnStart == nil {
			return fmt.Errorf("no start() function")
		}
		return nil
	}, jsCallTimeout)
	if err != nil {
		j.shutdownLoop()
		return nil, fmt.Errorf("adapter %q: %w", id, err)
	}
	return j, nil
}

func (j *jsTunnel) Name() string { return "js:" + j.id }
func (j *jsTunnel) Mode() string { return "js" }

// Footprint: an adapter is a black box by design, so the honest declaration is
// "we do not know what it created, and therefore we will never claim to have
// removed it". Teardown is confined to what the HOST can see and owns: every
// process the adapter spawned through ctx.spawn, killed by process group.
//
// That asymmetry is deliberate and is the price of the escape hatch. It is
// stated here rather than left for someone to discover, and it is why the
// built-in providers are the happy path.
func (j *jsTunnel) Footprint() Footprint {
	return Footprint{
		Provider:  j.Name(),
		Ephemeral: true, // as far as the host can account for
		StateFiles: []string{
			j.script + " (adapter-owned; the host creates no state for it)",
		},
	}
}

// Destroy calls the adapter's optional destroy() — its chance to remove
// whatever it provisioned — and then stops it either way.
//
// A destroy() that throws, hangs or was never defined must not leave a process
// behind, so the host kills the adapter's process group regardless, exactly as
// Stop does. The host never claims a remote resource was removed: only the
// adapter knows what it made.
func (j *jsTunnel) Destroy(ctx context.Context, logf func(string, ...any)) error {
	if logf == nil {
		logf = j.logf
	}
	var fn goja.Callable
	if err := j.run(func(rt *goja.Runtime) error {
		if f, ok := goja.AssertFunction(rt.GlobalObject().Get("destroy")); ok {
			fn = f
		}
		return nil
	}, jsCallTimeout); err != nil {
		logf("adapter %s: could not look up destroy(): %v", j.id, j.red.scrub(err.Error()))
	}
	if fn != nil {
		if err := j.run(func(rt *goja.Runtime) error {
			_, err := fn(goja.Undefined(), j.ctxObj)
			return err
		}, jsStopTimeout); err != nil {
			logf("adapter %s: destroy() failed (%v); stopping it anyway", j.id, j.red.scrub(err.Error()))
		}
	} else {
		logf("adapter %s: no destroy() — the host removes only what it can see: "+
			"the processes this adapter spawned", j.id)
	}
	return j.Stop()
}

// startLoop runs the single goroutine that owns the goja runtime.
func (j *jsTunnel) startLoop() {
	j.loop.Add(1)
	go func() {
		defer j.loop.Done()
		for job := range j.jobs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						j.logf("adapter %s: recovered from panic: %v", j.id, r)
						j.setErr(fmt.Sprintf("panic: %v", r))
					}
				}()
				job(j.vm)
			}()
		}
	}()
}

func (j *jsTunnel) shutdownLoop() {
	select {
	case <-j.closed:
		return
	default:
	}
	close(j.closed)
	close(j.jobs)
	done := make(chan struct{})
	go func() { j.loop.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(jsStopTimeout):
		j.logf("adapter %s: script loop did not exit; abandoning it", j.id)
	}
}

// run submits a job to the JS goroutine and waits for it, interrupting the VM
// if the script overruns its budget.
func (j *jsTunnel) run(fn func(*goja.Runtime) error, timeout time.Duration) error {
	select {
	case <-j.closed:
		return fmt.Errorf("adapter %s is stopped", j.id)
	default:
	}
	resCh := make(chan error, 1)
	job := func(rt *goja.Runtime) {
		defer func() {
			if r := recover(); r != nil {
				resCh <- fmt.Errorf("panic: %v", r)
			}
		}()
		resCh <- fn(rt)
	}
	select {
	case j.jobs <- job:
	case <-time.After(timeout):
		return fmt.Errorf("adapter %s is busy; call dropped", j.id)
	case <-j.closed:
		return fmt.Errorf("adapter %s is stopped", j.id)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-resCh:
		return err
	case <-timer.C:
		j.vm.Interrupt(fmt.Sprintf("adapter %s exceeded %s", j.id, timeout))
		select {
		case <-resCh:
		case <-time.After(2 * time.Second):
		}
		j.vm.ClearInterrupt()
		return fmt.Errorf("adapter %s timed out after %s", j.id, timeout)
	}
}

// post submits a job without waiting (used for onLine callbacks).
func (j *jsTunnel) post(fn func(*goja.Runtime)) {
	select {
	case <-j.closed:
	case j.jobs <- fn:
	default:
		// Queue full: drop the line rather than block the process reader.
	}
}

// buildContext wires the host API described in SPEC §6 onto `ctx`.
func (j *jsTunnel) buildContext(adapterConfig map[string]string) error {
	vm := j.vm
	obj := vm.NewObject()
	j.ctxObj = obj

	cfg := vm.NewObject()
	for k, v := range adapterConfig {
		if err := cfg.Set(k, v); err != nil {
			return err
		}
	}
	set := func(name string, v any) {
		if err := obj.Set(name, v); err != nil {
			panic(err)
		}
	}

	set("config", cfg)
	set("localUrl", j.localURL)
	set("spawn", j.jsSpawn)
	set("onLine", j.jsOnLine)
	set("setUrl", j.jsSetURL)
	set("isAlive", j.jsIsAlive)
	set("kill", j.jsKill)
	set("log", j.jsLog)
	set("env", j.jsEnv)

	// ctx.url is a live getter, not a snapshot.
	if err := obj.DefineAccessorProperty("url",
		vm.ToValue(func(goja.FunctionCall) goja.Value { return vm.ToValue(j.URL()) }),
		nil, goja.FLAG_FALSE, goja.FLAG_TRUE); err != nil {
		return err
	}
	return vm.Set("ctx", obj)
}

// jsSpawn implements ctx.spawn(cmd, args, [envMap]). The optional third
// argument lets an adapter pass a credential to the child through its
// ENVIRONMENT instead of argv, where `ps` would expose it.
func (j *jsTunnel) jsSpawn(call goja.FunctionCall) goja.Value {
	vm := j.vm
	if len(call.Arguments) < 1 {
		panic(vm.ToValue("spawn(cmd, args) requires a command"))
	}
	name := call.Argument(0).String()
	var args []string
	if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) && !goja.IsNull(call.Argument(1)) {
		if err := vm.ExportTo(call.Argument(1), &args); err != nil {
			panic(vm.ToValue("spawn(cmd, args): args must be an array of strings"))
		}
	}
	env := minimalEnv()
	if len(call.Arguments) > 2 && !goja.IsUndefined(call.Argument(2)) {
		extra := map[string]string{}
		if err := vm.ExportTo(call.Argument(2), &extra); err == nil {
			for k, v := range extra {
				j.red.add(v)
				env = append(env, k+"="+v)
			}
		}
	}

	bin, err := resolveBinary("", name)
	if err != nil {
		panic(vm.ToValue(err.Error()))
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		panic(vm.ToValue(err.Error()))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		panic(vm.ToValue(err.Error()))
	}
	if err := cmd.Start(); err != nil {
		panic(vm.ToValue(fmt.Sprintf("spawn %s: %v", bin, err)))
	}

	p := &jsProc{cmd: cmd, pid: cmd.Process.Pid}
	j.mu.Lock()
	j.procs = append(j.procs, p)
	j.mu.Unlock()
	j.logf("adapter %s: spawned %s (pid %d)", j.id, bin, p.pid)

	go j.pump(p, stdout)
	go j.pump(p, stderr)
	go func() {
		_ = cmd.Wait()
		p.dead.Store(true)
		j.logf("adapter %s: pid %d exited", j.id, p.pid)
	}()

	res := j.vm.NewObject()
	_ = res.Set("pid", p.pid)
	return res
}

// pump feeds a child's output to whatever onLine handlers the adapter
// registered, then logs it (scrubbed).
func (j *jsTunnel) pump(p *jsProc, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		j.mu.RLock()
		handlers := append([]goja.Callable{}, j.handlers[p.pid]...)
		j.mu.RUnlock()
		for _, h := range handlers {
			fn := h
			j.post(func(rt *goja.Runtime) {
				defer func() {
					if rec := recover(); rec != nil {
						j.logf("adapter %s: onLine handler panicked: %v", j.id, rec)
					}
				}()
				if _, err := fn(goja.Undefined(), rt.ToValue(line)); err != nil {
					j.logf("adapter %s: onLine handler threw: %v", j.id, j.red.scrub(err.Error()))
				}
			})
		}
		j.logf("adapter %s: %s", j.id, j.red.scrub(strings.TrimSpace(line)))
	}
}

// jsOnLine implements ctx.onLine(proc, fn).
func (j *jsTunnel) jsOnLine(call goja.FunctionCall) goja.Value {
	pid := pidOf(j.vm, call.Argument(0))
	fn, ok := goja.AssertFunction(call.Argument(1))
	if !ok {
		panic(j.vm.ToValue("onLine(proc, fn): fn must be a function"))
	}
	j.mu.Lock()
	j.handlers[pid] = append(j.handlers[pid], fn)
	j.mu.Unlock()
	return goja.Undefined()
}

func pidOf(vm *goja.Runtime, v goja.Value) int {
	if obj, ok := v.(*goja.Object); ok {
		if pid := obj.Get("pid"); pid != nil {
			return int(pid.ToInteger())
		}
	}
	return int(v.ToInteger())
}

// jsSetURL implements ctx.setUrl(s).
func (j *jsTunnel) jsSetURL(call goja.FunctionCall) goja.Value {
	u := strings.TrimSpace(call.Argument(0).String())
	if u == "" {
		return goja.Undefined()
	}
	if j.red.contains(u) {
		// An adapter must never smuggle a credential out through the URL.
		j.logf("adapter %s: refused a url containing a secret", j.id)
		return goja.Undefined()
	}
	j.mu.Lock()
	changed := j.url != u
	j.url = u
	j.mu.Unlock()
	if changed {
		j.logf("adapter %s: public url %s", j.id, u)
	}
	select {
	case <-j.urlReady:
	default:
		close(j.urlReady)
	}
	return goja.Undefined()
}

// jsIsAlive implements ctx.isAlive(): true when at least one spawned process
// is still running.
func (j *jsTunnel) jsIsAlive(goja.FunctionCall) goja.Value {
	return j.vm.ToValue(j.alive())
}

func (j *jsTunnel) alive() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	for _, p := range j.procs {
		if !p.dead.Load() {
			return true
		}
	}
	return false
}

// jsKill implements ctx.kill(): terminate every process this adapter spawned.
func (j *jsTunnel) jsKill(goja.FunctionCall) goja.Value {
	j.killAll()
	return goja.Undefined()
}

func (j *jsTunnel) killAll() {
	j.mu.RLock()
	procs := append([]*jsProc{}, j.procs...)
	j.mu.RUnlock()
	for _, p := range procs {
		if !p.dead.Load() {
			killProcessGroup(p.pid, j.logf)
		}
	}
}

// jsLog implements ctx.log(s) — scrubbed, always.
func (j *jsTunnel) jsLog(call goja.FunctionCall) goja.Value {
	parts := make([]string, 0, len(call.Arguments))
	for _, a := range call.Arguments {
		parts = append(parts, a.String())
	}
	j.logf("adapter %s: %s", j.id, j.red.scrub(strings.Join(parts, " ")))
	return goja.Undefined()
}

// jsEnv implements ctx.env(name): read a process environment variable BY NAME
// and hand the value to the adapter for use (typically to pass to a spawned
// tool). The value is registered as a secret, so it can never appear in a log
// line, in a status payload or in a state file — even if the adapter tries.
func (j *jsTunnel) jsEnv(call goja.FunctionCall) goja.Value {
	name := strings.TrimSpace(call.Argument(0).String())
	if name == "" {
		return goja.Undefined()
	}
	val, ok := os.LookupEnv(name)
	if !ok {
		return goja.Undefined()
	}
	j.red.add(val)
	return j.vm.ToValue(val)
}

// Launch calls the adapter's start().
func (j *jsTunnel) Launch(ctx context.Context) error {
	j.mu.Lock()
	j.running = true
	j.startedAt = time.Now()
	j.mu.Unlock()

	err := j.run(func(rt *goja.Runtime) error {
		_, err := j.fnStart(goja.Undefined(), j.ctxObj)
		return err
	}, jsCallTimeout)
	if err != nil {
		j.setErr(j.red.scrub(err.Error()))
		j.mu.Lock()
		j.running = false
		j.mu.Unlock()
		j.killAll()
		return fmt.Errorf("adapter %s: start() failed: %w", j.id, err)
	}
	return nil
}

func (j *jsTunnel) WaitForURL(ctx context.Context, timeout time.Duration) (string, error) {
	select {
	case <-j.urlReady:
		return j.URL(), nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(timeout):
		return "", fmt.Errorf("adapter %s did not call ctx.setUrl within %s", j.id, timeout)
	}
}

func (j *jsTunnel) URL() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.url
}

func (j *jsTunnel) setErr(msg string) {
	j.mu.Lock()
	j.lastErr = msg
	j.mu.Unlock()
}

// Snapshot asks the adapter for status, but never trusts it with secrets: the
// reported url is scrubbed and anything that looks like a credential is
// dropped.
func (j *jsTunnel) Snapshot() Status {
	j.mu.RLock()
	st := Status{
		Provider:  "js:" + j.id,
		Tunnel:    "js",
		Running:   j.running,
		URL:       j.url,
		LastError: j.lastErr,
		StartedAt: j.startedAt,
	}
	for _, p := range j.procs {
		if !p.dead.Load() {
			st.PID = p.pid
			break
		}
	}
	j.mu.RUnlock()
	st.Healthy = j.alive() && st.URL != ""

	if j.fnStatus != nil {
		var reported map[string]any
		err := j.run(func(rt *goja.Runtime) error {
			v, err := j.fnStatus(goja.Undefined(), j.ctxObj)
			if err != nil {
				return err
			}
			if obj, ok := v.(*goja.Object); ok {
				reported = obj.Export().(map[string]any)
			}
			return nil
		}, 5*time.Second)
		if err != nil {
			st.LastError = j.red.scrub(err.Error())
		} else if reported != nil {
			if u, ok := reported["url"].(string); ok && u != "" && !j.red.contains(u) {
				st.URL = u
			}
			if h, ok := reported["healthy"].(bool); ok {
				st.Healthy = h
			}
		}
	}
	st.URL = j.red.scrub(st.URL)
	st.LastError = j.red.scrub(st.LastError)
	return st
}

// Stop calls the adapter's stop() and then kills whatever it spawned anyway.
// Idempotent: the second call is a no-op and still returns nil.
func (j *jsTunnel) Stop() error {
	j.stopOnce.Do(func() {
		if j.fnStop != nil {
			if err := j.run(func(rt *goja.Runtime) error {
				_, err := j.fnStop(goja.Undefined(), j.ctxObj)
				return err
			}, jsStopTimeout); err != nil {
				// A stop() that throws or hangs must not leave a process behind.
				j.logf("adapter %s: stop() failed (%v); killing spawned processes anyway", j.id, j.red.scrub(err.Error()))
			}
		}
		j.killAll()
		j.shutdownLoop()
		j.mu.Lock()
		j.running = false
		j.mu.Unlock()
		j.logf("adapter %s: stopped", j.id)
	})
	return nil
}
