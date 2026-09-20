package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/herdr-expose/internal/platform"
)

// Supervision is three layers (SPEC amendment B7):
//
//  1. `daemon` fork-execs `serve` detached and exits 0, because Herdr's startup
//     hooks are one-shot and unsupervised.
//  2. A real service unit — launchd LaunchAgent with KeepAlive on macOS,
//     systemd --user with Restart=always on Linux — running `serve` in the
//     FOREGROUND as its main process.
//  3. An append-only managed-pid ledger with reclaimStrays() on takeover, so a
//     manual start never ends up fighting a manager for the port.
//
// Everything that starts or stops the server routes through managerInCharge()
// first: killing a supervised process just makes the manager respawn it.

// StateDir is $HERDR_PLUGIN_STATE_DIR, else the platform's per-user state dir:
// ~/.local/state/herdr-expose on Unix, %LOCALAPPDATA%\herdr-expose\state on
// Windows. See internal/platform/paths_*.go.
func StateDir() (string, error) {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return d, os.MkdirAll(d, 0o700)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	_ = home
	d, err := platform.StateDir("herdr-expose")
	if err != nil {
		return "", err
	}
	return d, os.MkdirAll(d, 0o700)
}

func pidfilePath(state string) string { return filepath.Join(state, "herdr-expose.pid") }
func ledgerPath(state string) string  { return filepath.Join(state, "managed-pids.ndjson") }

// pidLock is an exclusive flock on the pidfile, held for the process lifetime.
type pidLock struct {
	f    *os.File
	path string
}

// ErrAlreadyRunning means another instance holds the lock.
var ErrAlreadyRunning = errors.New("another herdr-expose instance is already running")

// acquirePidLock takes an exclusive, non-blocking flock. The startup hook fires
// on every Herdr session restore, so a double start must be harmless.
func acquirePidLock(state string) (*pidLock, error) {
	path := pidfilePath(state)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if held, err := platform.LockFile(f, false); err != nil || !held {
		_ = f.Close()
		return nil, ErrAlreadyRunning
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &pidLock{f: f, path: path}, nil
}

func (p *pidLock) release() {
	if p == nil || p.f == nil {
		return
	}
	_ = platform.UnlockFile(p.f)
	_ = p.f.Close()
	_ = os.Remove(p.path)
}

// readPid returns the pid recorded in the pidfile, if any.
func readPid(state string) int {
	b, err := os.ReadFile(pidfilePath(state))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// pidAlive reports whether a pid exists. Signal 0 on Unix; an OpenProcess +
// GetExitCodeProcess probe on Windows, which has no signals at all.
func pidAlive(pid int) bool { return platform.Alive(pid) }

// ledgerEntry is one append-only record of a process we started.
type ledgerEntry struct {
	PID  int       `json:"pid"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Addr string    `json:"addr"`
}

func recordPID(state string, e ledgerEntry) {
	f, err := os.OpenFile(ledgerPath(state), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, _ = f.Write(append(b, '\n'))
}

func readLedger(state string) []ledgerEntry {
	b, err := os.ReadFile(ledgerPath(state))
	if err != nil {
		return nil
	}
	var out []ledgerEntry
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e ledgerEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func truncateLedger(state string) { _ = os.Remove(ledgerPath(state)) }

// reclaimStrays takes over from any process we previously started: SIGTERM
// every recorded pid, WAIT for the port to actually be released, then SIGKILL
// whatever is left. Skipping the wait is how you get "address already in use"
// a quarter second after a clean-looking stop.
func reclaimStrays(state, addr string) {
	self := os.Getpid()
	var live []int
	for _, e := range readLedger(state) {
		if e.PID == self || !pidAlive(e.PID) {
			continue
		}
		live = append(live, e.PID)
	}
	if pid := readPid(state); pid > 0 && pid != self && pidAlive(pid) {
		live = append(live, pid)
	}
	if len(live) == 0 {
		truncateLedger(state)
		return
	}
	for _, pid := range live {
		if p, err := os.FindProcess(pid); err == nil {
			_ = platform.Terminate(p)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if portFree(addr) && noneAlive(live) {
			truncateLedger(state)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range live {
		if p, err := os.FindProcess(pid); err == nil && pidAlive(pid) {
			_ = platform.ForceKill(p)
		}
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !portFree(addr) {
		time.Sleep(100 * time.Millisecond)
	}
	truncateLedger(state)
}

func noneAlive(pids []int) bool {
	for _, pid := range pids {
		if pidAlive(pid) {
			return false
		}
	}
	return true
}

// portFree reports whether addr can be bound right now.
func portFree(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// --- service state, so install/uninstall can CONVERGE rather than repeat ----
//
// `service install` used to be a blind sequence: write the unit, unload it,
// load it. Run twice it dropped a healthy supervised server and brought it
// back; run on a machine with an unmanaged daemon it took the port over
// SILENTLY, because the unit's `serve` calls reclaimStrays, which SIGTERMs
// every pid in the ledger. On this machine that ledger is the daemon serving
// the owner's live sessions.
//
// Taking over an unmanaged daemon is the right behaviour — two copies fighting
// for one port is worse — but it has to be a thing the operator is TOLD about
// and can decline. So install now reads the world first, says what it is about
// to do, and verifies afterwards that the unit genuinely came up.

// serviceLabel is the unit's identity: a launchd label, a systemd unit name,
// and (as "herdr-expose") a Windows service name.
const serviceLabel = "com.deemwar.herdr-expose"

// serviceState is what the platform's service manager says right now.
type serviceState struct {
	Installed bool   // the unit file exists on disk
	Loaded    bool   // the manager knows about it
	Active    bool   // it is actually running something
	PID       int    // the supervised process, when there is one
	Who       string // "launchd:<label>" / "systemd:herdr-expose.service"
	UnitPath  string
}

// Healthy is the only state in which a second `service install` is a no-op.
func (s serviceState) Healthy() bool { return s.Loaded && s.Active && s.PID > 0 }

// awaitServiceUp polls for the unit to actually be running something, so
// install VERIFIES rather than assumes. A unit that loads and then exits
// instantly (the KeepAlive-flapping failure mode) is caught here.
func awaitServiceUp(timeout time.Duration) serviceState {
	deadline := time.Now().Add(timeout)
	var st serviceState
	for {
		st = readServiceState()
		if st.Healthy() || time.Now().After(deadline) {
			return st
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// unmanagedDaemonPID reports a herdr-expose server that is running WITHOUT a
// service manager — the one that installing a unit is about to take over.
func unmanagedDaemonPID(state string) int {
	self := os.Getpid()
	if pid := readPid(state); pid > 0 && pid != self && pidAlive(pid) {
		return pid
	}
	for _, e := range readLedger(state) {
		if e.PID != self && pidAlive(e.PID) {
			return e.PID
		}
	}
	return 0
}

// --- unit templates, as PURE functions --------------------------------------
//
// Rendering is separated from loading so the content can be asserted without
// installing anything. That is not tidiness: the last regression here was a
// missing HERDR_EXPOSE_SUPERVISED, which made the supervised process exit
// instantly as a no-op while the loaded unit simultaneously caused every other
// start path to refuse — `service install` silently disabled the product, and
// nothing could catch it because the only way to see the template was to load
// it on a live machine.

func launchAgentPlist(exePath, home, state string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array>
    <string>%s</string><string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>%s/.local/bin:%s/.cargo/bin:%s/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    <key>HERDR_PLUGIN_STATE_DIR</key><string>%s</string>
    <!-- Without this, the unit is a KeepAlive loop of instant no-op exits:
         serve refuses to start when a manager is in charge (main.go), which is
         exactly what this unit IS. Worse, while the unit is loaded every other
         start path refuses too - including the plugin's own daemon hook - so
         installing the service silently disables the product. -->
    <key>HERDR_EXPOSE_SUPERVISED</key><string>1</string>
  </dict>
  <key>StandardOutPath</key><string>%s/serve.log</string>
  <key>StandardErrorPath</key><string>%s/serve.log</string>
</dict></plist>
`, serviceLabel, exePath, home, home, home, state, state, state)
}

func systemdUnit(exePath, home, state string) string {
	return fmt.Sprintf(`[Unit]
Description=herdr-expose (Herdr web bridge)
After=default.target

[Service]
Type=simple
ExecStart=%s serve
Restart=always
RestartSec=2
Environment=PATH=%s/.local/bin:%s/.cargo/bin:%s/bin:/usr/local/bin:/usr/bin:/bin
Environment=HERDR_PLUGIN_STATE_DIR=%s
# Without this the unit never actually serves: see the plist comment above.
Environment=HERDR_EXPOSE_SUPERVISED=1

[Install]
WantedBy=default.target
`, exePath, home, home, home, state)
}
