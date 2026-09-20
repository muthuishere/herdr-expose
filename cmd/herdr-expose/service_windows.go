//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows installs NO autostart mechanism, on purpose.
//
// This started as a Windows Service, then grew a logon-triggered Scheduled Task
// beside it, then a Startup-folder fallback beneath that — three mechanisms,
// each with its own install, status, uninstall and failure modes, on the one
// platform none of us runs daily. macOS has one. Linux has one. That asymmetry
// was not sophistication, it was surface area, and all of it was code that
// could only be exercised on a VM someone had to go and unwedge.
//
// Every one of the three was also worse than it looked:
//
//   - The SCM Service runs as LocalSystem, which resolves %APPDATA% to
//     C:\Windows\System32\config\systemprofile and therefore finds ZERO of the
//     user's Herdr sessions. Measured: it also could not find the herdr binary
//     (Herdr installs onto the USER PATH) and the SCM reported "terminated with
//     the following error: Incorrect function". It supervised an empty UI.
//   - The Scheduled Task was refused outright on this machine:
//     `schtasks /Create` → "ERROR: Access is denied." for a non-admin.
//   - The Startup folder does not supervise anything. It starts us at logon and
//     that is all; nothing restarts us on a crash.
//
// So Windows gets what it already had and nobody noticed: **Herdr's own plugin
// startup hook**, which fork-execs `./bin/herdr-expose daemon` and is
// cross-platform, per-user and needs no privilege. When that has not happened
// and the user reaches for something that needs a server, we TELL them, with
// the command — see notifyServerDown. A line the user acts on beats a
// background mechanism they cannot see, did not ask for, and would have to
// discover in order to remove.

// managerInCharge reports whether something else supervises us. On Windows
// nothing does, by design: the honest answer is always no.
func managerInCharge() (bool, string) { return false, "" }

// installService installs nothing on Windows and says so plainly. It is not an
// error — the desired state (no machine-wide service, no scheduled task, no
// startup entry) is already reached, and Herdr's startup hook covers the job.
func installService(_, _ string) (string, error) {
	fmt.Println("nothing to install: Windows has no autostart mechanism here, on purpose.")
	fmt.Println()
	fmt.Println("  Herdr's own plugin startup hook already fork-execs `herdr-expose daemon`")
	fmt.Println("  whenever Herdr starts, which is per-user and needs no privilege. That is")
	fmt.Println("  the same layer macOS and Linux get for free on top of their unit files.")
	fmt.Println()
	fmt.Println("  What Windows does NOT get is a supervisor: nothing restarts herdr-expose")
	fmt.Println("  if it crashes, where macOS has launchd KeepAlive and Linux has systemd")
	fmt.Println("  Restart=always. If it is not running, start it with:")
	fmt.Println()
	fmt.Println("      herdr-expose daemon")
	return "", nil
}

// uninstallService has nothing to remove, and says so rather than pretending.
func uninstallService() error {
	fmt.Println("nothing to uninstall: Windows installs no service, task or startup entry.")
	return nil
}

// readServiceState is always "nobody in charge" on Windows. The pidfile still
// knows whether a server is up, and `herdr-expose status` reports that.
func readServiceState() serviceState { return serviceState{} }

// notifyServerDown surfaces ONE line, in the Herdr TUI where the user actually
// is, naming the command that fixes it.
//
// Rules, because a prompt that nags is worse than no prompt: it fires only when
// the user has REACHED for something that needs a running server (an action, a
// pane, `open`), never on a timer, and never while the server is up. Herdr's
// own `notification show` is the surface — a plugin should not invent its own.
// Entirely best effort: a notification that cannot be delivered must never fail
// the command the user actually ran.
func notifyServerDown(bin string) {
	if bin == "" {
		return
	}
	cmd := exec.Command(bin, "notification", "show",
		"herdr-expose is not running",
		"--body", "Start it with:  herdr-expose daemon",
		"--sound", "none")
	// No console window, no inherited stdio, and a hard deadline: this is a
	// courtesy, and a courtesy must not hang the command the user ran.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
	}
	fmt.Fprintln(os.Stderr, "herdr-expose is not running — start it with `herdr-expose daemon`")
}
