package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// StateDir is $HERDR_PLUGIN_STATE_DIR, else ~/.local/state/herdr-expose.
func StateDir() (string, error) {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return d, os.MkdirAll(d, 0o700)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".local", "state", "herdr-expose")
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
	_ = syscall.Flock(int(p.f.Fd()), syscall.LOCK_UN)
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

// pidAlive reports whether a pid exists (signal 0).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

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
			_ = p.Signal(syscall.SIGTERM)
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
			_ = p.Signal(syscall.SIGKILL)
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

// --- layer 2: real service units -------------------------------------------

const serviceLabel = "com.deemwar.herdr-expose"

// managerInCharge reports whether a launchd/systemd unit currently supervises
// us. Killing a supervised process only makes the manager respawn it, so every
// start/stop path checks this first.
func managerInCharge() (bool, string) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "list").Output()
		if err == nil && strings.Contains(string(out), serviceLabel) {
			return true, "launchd:" + serviceLabel
		}
	case "linux":
		out, err := exec.Command("systemctl", "--user", "is-enabled", "herdr-expose.service").Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "enabled") {
			return true, "systemd:herdr-expose.service"
		}
	}
	return false, ""
}

// installService writes and loads a real supervised unit for this platform.
func installService(exePath, state string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchAgent(exePath, state)
	case "linux":
		return installSystemdUnit(exePath, state)
	default:
		return "", fmt.Errorf("service install is not supported on %s", runtime.GOOS)
	}
}

func installLaunchAgent(exePath, state string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	plistPath := filepath.Join(dir, serviceLabel+".plist")
	// PATH is spelled out: launchd gives a unit a minimal PATH that contains
	// none of the places herdr is normally installed.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
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
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return "", err
	}
	_ = exec.Command("launchctl", "unload", plistPath).Run()
	if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
		return plistPath, fmt.Errorf("launchctl load: %v: %s", err, out)
	}
	return plistPath, nil
}

func installSystemdUnit(exePath, state string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	unitPath := filepath.Join(dir, "herdr-expose.service")
	unit := fmt.Sprintf(`[Unit]
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
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return "", err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "herdr-expose.service").CombinedOutput(); err != nil {
		return unitPath, fmt.Errorf("systemctl enable: %v: %s", err, out)
	}
	return unitPath, nil
}

func uninstallService() error {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		plistPath := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		// Uninstalling twice is success: the desired state is "no unit", and
		// it is already reached. An ENOENT here used to make the second run
		// exit 1 on a machine that was in exactly the state asked for.
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", "herdr-expose.service").Run()
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		// Same rule as launchd above: already-absent is the desired state.
		if err := os.Remove(filepath.Join(home, ".config", "systemd", "user",
			"herdr-expose.service")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return fmt.Errorf("service uninstall is not supported on %s", runtime.GOOS)
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

func readServiceState() serviceState {
	st := serviceState{}
	home, err := os.UserHomeDir()
	if err != nil {
		return st
	}
	switch runtime.GOOS {
	case "darwin":
		st.Who = "launchd:" + serviceLabel
		st.UnitPath = filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
		if _, err := os.Stat(st.UnitPath); err == nil {
			st.Installed = true
		}
		out, err := exec.Command("launchctl", "list", serviceLabel).Output()
		if err != nil {
			return st
		}
		st.Loaded = true
		// `launchctl list <label>` prints a plist-ish dict with "PID" = N;
		// when something is actually running, and omits it when it is not.
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, "\"PID\"") {
				continue
			}
			f := strings.FieldsFunc(line, func(r rune) bool { return r < '0' || r > '9' })
			if len(f) > 0 {
				if n, err := strconv.Atoi(f[len(f)-1]); err == nil && n > 0 {
					st.PID, st.Active = n, true
				}
			}
		}
	case "linux":
		st.Who = "systemd:herdr-expose.service"
		st.UnitPath = filepath.Join(home, ".config", "systemd", "user", "herdr-expose.service")
		if _, err := os.Stat(st.UnitPath); err == nil {
			st.Installed = true
		}
		out, err := exec.Command("systemctl", "--user", "show", "herdr-expose.service",
			"-p", "MainPID", "-p", "ActiveState", "-p", "LoadState").Output()
		if err != nil {
			return st
		}
		for _, line := range strings.Split(string(out), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			switch k {
			case "LoadState":
				st.Loaded = v == "loaded"
			case "ActiveState":
				st.Active = v == "active"
			case "MainPID":
				if n, err := strconv.Atoi(v); err == nil {
					st.PID = n
				}
			}
		}
	}
	return st
}

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
