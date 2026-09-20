//go:build !windows

package platform

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ProcGroup owns a spawned process TREE so that stopping a tunnel cannot leave
// an orphaned cloudflared behind. On Unix the tree is a process group and the
// group id is enough to address it, so the handle carries no state.
type ProcGroup struct{}

// PrepareGroup must be called before cmd.Start. It puts the child in its own
// process group, which is what makes a single kill reach its children.
func PrepareGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// PrepareDetached must be called before cmd.Start on a child that must OUTLIVE
// this process: the daemon fork-exec and a spawned share. Setsid detaches it
// from the controlling terminal so a Ctrl-C in the parent's shell does not
// reach it.
func PrepareDetached(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// AdoptGroup is called after cmd.Start. On Unix PrepareGroup already did the
// work, so there is nothing to adopt.
func AdoptGroup(*exec.Cmd) (*ProcGroup, error) { return nil, nil }

// Close releases the handle without killing anything.
func (*ProcGroup) Close() {}

// KillTree stops the whole tree: SIGTERM to the group, then SIGKILL to whatever
// is still standing. g is unused on Unix.
func KillTree(pid int, _ *ProcGroup, logf func(string, ...any)) {
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

// Alive reports whether a pid exists. Signal 0 is the portable-on-Unix probe.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// Terminate asks a process to shut down cleanly.
func Terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// ForceKill stops a process that would not go quietly.
func ForceKill(p *os.Process) error { return p.Signal(syscall.SIGKILL) }

// ShutdownSignals are the signals `serve` treats as "wind down".
func ShutdownSignals() []os.Signal { return []os.Signal{syscall.SIGINT, syscall.SIGTERM} }

// ReloadSignal is the signal that re-reads config, or nil where there is none.
func ReloadSignal() os.Signal { return syscall.SIGHUP }
