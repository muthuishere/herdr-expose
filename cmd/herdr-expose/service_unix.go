//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Unix supervision: launchd on macOS, systemd --user on Linux. Moved here
// VERBATIM when Windows support landed, so the platform that has been in
// production cannot have changed behaviour in the port.
//
// serviceInstallOpts.Task is meaningless here and ignored; the CLI rejects
// --task before it reaches this file.

// --- layer 2: real service units -------------------------------------------

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
func installService(exePath, state string, _ bool) (string, error) {
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
	plist := launchAgentPlist(exePath, home, state)
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
	unit := systemdUnit(exePath, home, state)
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
