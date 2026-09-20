//go:build !windows

package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

// ProcessesMatching returns the pids whose FULL COMMAND LINE contains every one
// of the given substrings.
//
// It matches on a command line, never on a process name, because the only safe
// way to reclaim a stray cloudflared is to match the config path we ourselves
// generated. Matching "cloudflared" alone would kill the owner's unrelated
// tunnels.
func ProcessesMatching(needles ...string) []int {
	if len(needles) == 0 {
		return nil
	}
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !containsAll(line, needles) {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil && pid > 0 {
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
