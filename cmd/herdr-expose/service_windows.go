//go:build windows

package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"sort"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Windows supervision.
//
// WHY A SERVICE, NOT A SCHEDULED TASK.
//
// `service install` means one thing on Unix: survive a reboot and survive a
// crash. launchd KeepAlive and systemd Restart=always both deliver exactly
// that. A logon-triggered Scheduled Task does not — it starts only after a
// human logs in, and its "restart on failure" is a retry count, not a
// supervisor. Shipping the same verb with a materially weaker promise on one
// platform is how a user ends up believing their server is supervised when it
// is not.
//
// So the default is a real Windows Service: auto-start at boot with no logon,
// SCM failure actions set to restart forever, and a `service status` that reads
// live state out of the SCM rather than guessing. That needs Administrator to
// install, which is a real cost, so it is stated plainly and there is a named
// opt-out: `service install --task` installs the logon-triggered Scheduled Task
// instead and SAYS what it gives up.
//
// A Service also means `serve` can be started by the SCM, which speaks a
// control protocol rather than signals. runWindowsService below is that
// adapter; shutdown_windows.go carries the stop into the same context the
// signal path uses.

const (
	windowsServiceName = "herdr-expose"
	windowsServiceDesc = "herdr-expose (Herdr web bridge)"
	windowsTaskName    = "herdr-expose"
)

// --- layer 2a: the service, as the SCM sees it ------------------------------

// serviceHandler bridges the SCM's control protocol onto cmdServe.
type serviceHandler struct{}

func (serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	s <- svc.Status{State: svc.StartPending}

	errc := make(chan error, 1)
	go func() { errc <- cmdServe(nil) }()

	s <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case err := <-errc:
			// serve returned on its own: a bind failure, a bad config. Exiting
			// non-zero is what tells the SCM to apply the failure actions and
			// restart us, so this must NOT be swallowed.
			if err != nil {
				return false, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				signalSCMStop()
				select {
				case <-errc:
				case <-time.After(20 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// runWindowsServiceIfManaged runs the SCM protocol when we were started BY the
// SCM, and reports false when we were started from a shell. Called first thing
// in main so that one binary is both the CLI and the service.
func runWindowsServiceIfManaged() (bool, error) {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false, nil
	}
	return true, svc.Run(windowsServiceName, serviceHandler{})
}

// --- elevation --------------------------------------------------------------

func isElevated() bool {
	tok := windows.GetCurrentProcessToken()
	return tok.IsElevated()
}

// --- layer 2b: install / uninstall / read ------------------------------------

func managerInCharge() (bool, string) {
	if st := readServiceState(); st.Loaded {
		return true, st.Who
	}
	return false, ""
}

func installService(exePath, state string, useTask bool) (string, error) {
	if useTask {
		return installScheduledTask(exePath, state)
	}
	if !isElevated() {
		return "", errors.New(
			"installing a Windows Service needs Administrator.\n" +
				"  Either re-run this in an elevated shell:\n" +
				"    Start-Process powershell -Verb RunAs -ArgumentList '-NoExit','-Command','herdr-expose service install'\n" +
				"  or install the weaker, no-admin supervisor instead:\n" +
				"    herdr-expose service install --task\n" +
				"  A Scheduled Task starts only AFTER you log in and does not restart on crash the\n" +
				"  way launchd KeepAlive / systemd Restart=always do. It is the lesser promise, on purpose.")
	}
	m, err := mgr.Connect()
	if err != nil {
		return "", fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer m.Disconnect()

	// Converge, do not stack: a leftover service with the same name must be
	// replaced, not fought with.
	if s, err := m.OpenService(windowsServiceName); err == nil {
		_ = stopAndDelete(s)
		s.Close()
		waitForServiceGone(m, 10*time.Second)
	}

	s, err := m.CreateService(windowsServiceName, exePath, mgr.Config{
		DisplayName: windowsServiceDesc,
		Description: "Serves Herdr panes to a browser on loopback, with an optional tunnel.",
		StartType:   mgr.StartAutomatic,
	}, "serve")
	if err != nil {
		return "", fmt.Errorf("create service %s: %w", windowsServiceName, err)
	}
	defer s.Close()

	// This is the KeepAlive / Restart=always equivalent, and without it the
	// service is merely auto-starting, not supervised. Restart after 2s, every
	// time, and never stop retrying (ResetPeriod 0 = never reset the counter).
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 2 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
	}, 0); err != nil {
		return "", fmt.Errorf("set restart-on-failure actions: %w", err)
	}

	// The unit carries its own environment on Unix. A Windows service inherits
	// the machine environment instead, so the two variables that MUST be right
	// are set on the service's own registry key.
	if err := setServiceEnvironment(map[string]string{
		"HERDR_EXPOSE_SUPERVISED": "1",
		"HERDR_PLUGIN_STATE_DIR":  state,
	}); err != nil {
		return "", fmt.Errorf("set service environment: %w", err)
	}

	if err := s.Start(); err != nil {
		return "", fmt.Errorf("start service %s: %w", windowsServiceName, err)
	}
	return `SCM:` + windowsServiceName, nil
}

func uninstallService() error {
	// A Scheduled Task needs no admin either way, so remove it unconditionally
	// and treat "not there" as success.
	_ = removeScheduledTask()

	m, err := mgr.Connect()
	if err != nil {
		return nil // no SCM access at all: the task removal above is all we can do
	}
	defer m.Disconnect()
	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return nil // already absent is the desired state
	}
	defer s.Close()
	if !isElevated() {
		return errors.New("removing the Windows Service needs Administrator; re-run in an elevated shell")
	}
	if err := stopAndDelete(s); err != nil {
		return err
	}
	waitForServiceGone(m, 10*time.Second)
	return nil
}

func stopAndDelete(s *mgr.Service) error {
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err == nil {
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if st, err := s.Query(); err == nil && st.State == svc.Stopped {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
	return s.Delete()
}

func waitForServiceGone(m *mgr.Mgr, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := m.OpenService(windowsServiceName)
		if err != nil {
			return
		}
		s.Close()
		time.Sleep(200 * time.Millisecond)
	}
}

// setServiceEnvironment writes REG_MULTI_SZ `Environment` under the service's
// own key, which is how a Windows service gets per-unit environment. It is the
// exact counterpart of the plist's <EnvironmentVariables> and the systemd
// unit's Environment= lines -- and it carries the same load-bearing
// HERDR_EXPOSE_SUPERVISED=1, without which the supervised process exits
// instantly as a no-op while every other start path refuses.
func setServiceEnvironment(env map[string]string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Services\`+windowsServiceName, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	entries := make([]string, 0, len(env))
	for name, v := range env {
		entries = append(entries, name+"="+v)
	}
	sort.Strings(entries) // deterministic, so a reinstall is a no-op diff
	return k.SetStringsValue("Environment", entries)
}

func readServiceState() serviceState {
	st := serviceState{Who: "windows-service:" + windowsServiceName, UnitPath: `SCM:` + windowsServiceName}
	m, err := mgr.Connect()
	if err == nil {
		defer m.Disconnect()
		if s, err := m.OpenService(windowsServiceName); err == nil {
			defer s.Close()
			st.Installed, st.Loaded = true, true
			if q, err := s.Query(); err == nil {
				st.Active = q.State == svc.Running
				st.PID = int(q.ProcessId)
			}
			return st
		}
	}
	// No service. A Scheduled Task is the other supervisor we install, so
	// `service status` must be honest about that one too rather than printing
	// "no service manager in charge" while a task is running the server.
	return readScheduledTaskState()
}

// --- the no-admin alternative: a logon-triggered Scheduled Task -------------

func taskXML(exePath, state string) string {
	user := os.Getenv("USERNAME")
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>` + xmlEscape(windowsServiceDesc) + `</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Principals><Principal id="Author">
    <UserId>` + xmlEscape(user) + `</UserId><LogonType>InteractiveToken</LogonType>
    <RunLevel>LeastPrivilege</RunLevel>
  </Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author"><Exec>
    <Command>` + xmlEscape(exePath) + `</Command>
    <Arguments>serve</Arguments>
    <WorkingDirectory>` + xmlEscape(state) + `</WorkingDirectory>
  </Exec></Actions>
</Task>`
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func installScheduledTask(exePath, state string) (string, error) {
	// schtasks reads the XML as UTF-16, which is why the declaration says so.
	tmp := filepath.Join(os.TempDir(), "herdr-expose-task.xml")
	if err := os.WriteFile(tmp, utf16LE(taskXML(exePath, state)), 0o600); err != nil {
		return "", err
	}
	defer os.Remove(tmp)

	// The task inherits the user environment, so the two variables the unit
	// must carry are set for this user rather than on the task.
	if err := setUserEnv("HERDR_EXPOSE_SUPERVISED", "1"); err != nil {
		return "", err
	}
	if err := setUserEnv("HERDR_PLUGIN_STATE_DIR", state); err != nil {
		return "", err
	}

	if out, err := exec.Command("schtasks", "/Create", "/TN", windowsTaskName,
		"/XML", tmp, "/F").CombinedOutput(); err != nil {
		return "", fmt.Errorf("schtasks /Create: %v: %s", err, out)
	}
	if out, err := exec.Command("schtasks", "/Run", "/TN", windowsTaskName).CombinedOutput(); err != nil {
		return "", fmt.Errorf("schtasks /Run: %v: %s", err, out)
	}
	return `Task Scheduler:\` + windowsTaskName, nil
}

func removeScheduledTask() error {
	_, err := exec.Command("schtasks", "/End", "/TN", windowsTaskName).CombinedOutput()
	_ = err
	out, err := exec.Command("schtasks", "/Delete", "/TN", windowsTaskName, "/F").CombinedOutput()
	if err != nil && !strings.Contains(strings.ToLower(string(out)), "cannot find") {
		return fmt.Errorf("schtasks /Delete: %v: %s", err, out)
	}
	return nil
}

func readScheduledTaskState() serviceState {
	st := serviceState{
		Who:      `scheduled-task:` + windowsTaskName,
		UnitPath: `Task Scheduler:\` + windowsTaskName,
	}
	out, err := exec.Command("schtasks", "/Query", "/TN", windowsTaskName,
		"/FO", "LIST", "/V").Output()
	if err != nil {
		return serviceState{} // nothing in charge at all
	}
	st.Installed, st.Loaded = true, true
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if strings.EqualFold(k, "Status") && strings.EqualFold(v, "Running") {
			st.Active = true
		}
	}
	// schtasks does not report the child pid. Our own pidfile does, and it is
	// the same process, so report that rather than pretending we do not know.
	if state, err := StateDir(); err == nil {
		if pid := readPid(state); pid > 0 && pidAlive(pid) {
			st.PID = pid
		}
	}
	return st
}

// setUserEnv writes a persistent per-user environment variable (HKCU
// \Environment), which is what a logon-triggered task will inherit next logon.
func setUserEnv(name, value string) error {
	out, err := exec.Command("setx", name, value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("setx %s: %v: %s", name, err, out)
	}
	return nil
}

// utf16LE encodes the task XML the way schtasks /XML insists on reading it:
// UTF-16 little-endian with a BOM. Handing it UTF-8 fails with an opaque
// "The task XML is malformed".
func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(u)*2+2)
	b = append(b, 0xFF, 0xFE) // BOM
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}
