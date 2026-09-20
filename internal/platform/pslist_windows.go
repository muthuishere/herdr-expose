//go:build windows

package platform

import (
	"encoding/csv"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// ProcessesMatching returns the pids whose FULL COMMAND LINE contains every one
// of the given substrings. Same contract as the Unix twin, same reason: the
// stray-cloudflared reclaim matches on a config path WE generated, never on a
// process name, so it cannot hit somebody else's tunnel.
//
// Windows has no ps. A Toolhelp snapshot gives process names but not command
// lines, and the config path is only in the command line — so this asks CIM,
// which is the supported way to read another process's command line without
// PROCESS_VM_READ and PEB spelunking. It is slow (a PowerShell start), but it
// runs only on a teardown backstop path, never in a hot loop.
func ProcessesMatching(needles ...string) []int {
	if len(needles) == 0 {
		return nil
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-CimInstance Win32_Process | Select-Object ProcessId,CommandLine | ConvertTo-Csv -NoTypeInformation`)
	// No console window for a background reclaim.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	r := csv.NewReader(strings.NewReader(string(out)))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil || len(rows) < 2 {
		return nil
	}
	var pids []int
	for _, row := range rows[1:] {
		if len(row) < 2 {
			continue
		}
		if !containsAll(row[1], needles) {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(row[0])); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

func containsAll(s string, needles []string) bool {
	for _, n := range needles {
		if n == "" || !strings.Contains(s, n) {
			return false
		}
	}
	return true
}
