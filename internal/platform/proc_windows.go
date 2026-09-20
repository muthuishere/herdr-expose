//go:build windows

package platform

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows has no process groups in the Unix sense. CREATE_NEW_PROCESS_GROUP
// exists, but it only scopes console Ctrl events — it does NOT make a kill
// recursive, and killing a pid leaves its children running. cloudflared spawns
// children; an orphaned one holds a tunnel open against a hostname we think we
// tore down, which is the exact hazard the Unix stray-reclaim ledger exists
// for.
//
// A JOB OBJECT is the only construct that actually guarantees the tree dies.
// With JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, the kernel kills every process in
// the job the moment the last handle to it closes — including when we crash, or
// are TerminateProcess'd, and never get to run cleanup at all. That is the
// Windows equivalent of "the kernel drops the flock when the holder dies", and
// it is why this is a job and not a taskkill /T.
//
// The one honest caveat: exec.Cmd gives no way to create the child suspended,
// so there is a sub-millisecond window between CreateProcess and
// AssignProcessToJobObject in which a grandchild could be spawned outside the
// job. KillTree therefore still sweeps by pid afterwards, so the job is the
// guarantee and the sweep is the backstop.

// ProcGroup owns a spawned process tree.
type ProcGroup struct {
	once sync.Once
	h    windows.Handle
}

// PrepareGroup must be called before cmd.Start. CREATE_NEW_PROCESS_GROUP keeps
// a console Ctrl-C in our window from reaching the child, so the child's
// lifetime is ours to decide; the job object does the actual reaping.
func PrepareGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// PrepareDetached must be called before cmd.Start on a child that must OUTLIVE
// this process: the daemon fork-exec and a spawned share. DETACHED_PROCESS is
// the Windows answer to setsid — no inherited console, so closing the terminal
// that ran `herdr-expose daemon` does not take the server with it.
func PrepareDetached(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
}

// AdoptGroup is called immediately after cmd.Start. It creates an unnamed job
// with KILL_ON_JOB_CLOSE and puts the child in it. A failure here is reported
// but is not fatal: KillTree degrades to a pid-tree sweep.
func AdoptGroup(cmd *exec.Cmd) (*ProcGroup, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, nil
	}
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafePointerOf(&info)), uint32(unsafeSizeOf(info))); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	ph, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(h, ph); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &ProcGroup{h: h}, nil
}

// Close releases the job handle. Because the job carries KILL_ON_JOB_CLOSE,
// releasing the LAST handle also kills the tree — which is the point: an
// herdr-expose that dies without cleaning up still leaves no orphan.
func (g *ProcGroup) Close() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		if g.h != 0 {
			_ = windows.CloseHandle(g.h)
			g.h = 0
		}
	})
}

// KillTree stops the whole tree. TerminateJobObject reaches every process in
// the job in one call; the pid sweep afterwards catches the vanishingly rare
// grandchild spawned in the window before AdoptGroup ran.
func KillTree(pid int, g *ProcGroup, logf func(string, ...any)) {
	if g != nil && g.h != 0 {
		if err := windows.TerminateJobObject(g.h, 1); err != nil && logf != nil {
			logf("terminate job object for pid %d: %v", pid, err)
		}
		g.Close()
	}
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if logf != nil {
		logf("pid %d survived the job object; terminating directly", pid)
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// Alive reports whether a pid exists.
//
// os.Process.Signal(0) is not available here — Windows has no signals — so this
// asks the kernel directly. STILL_ACTIVE distinguishes a live process from a
// handle that merely still resolves, and ERROR_ACCESS_DENIED is treated as
// ALIVE: a pid we cannot open is a pid that exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

// Terminate asks a process to shut down.
//
// There is no SIGTERM. A console Ctrl-Break is the closest thing, but it only
// reaches processes attached to OUR console, and every process we start here is
// deliberately detached or in its own group — so it would be a no-op dressed up
// as a graceful stop. TerminateProcess is the honest answer, and every caller
// already follows it with a liveness wait.
func Terminate(p *os.Process) error { return p.Kill() }

// ForceKill stops a process that would not go quietly.
func ForceKill(p *os.Process) error { return p.Kill() }

// ShutdownSignals are the signals `serve` treats as "wind down". Go maps a
// console Ctrl-C to os.Interrupt on Windows; SIGTERM is delivered by the Go
// runtime only in narrow cases but costs nothing to listen for.
func ShutdownSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }

// ReloadSignal is the signal that re-reads config. Windows has no SIGHUP, so
// there is none: config reload happens through the file watcher and the API.
func ReloadSignal() os.Signal { return nil }
