//go:build windows

package platform

import (
	"os/exec"
	"testing"
	"time"
)

// The orphan hazard, tested directly: start a process that starts a CHILD, kill
// the tree, and prove the grandchild went too. `taskkill /PID` would fail this;
// a Job Object with KILL_ON_JOB_CLOSE is why it passes.
func TestKillTreeReapsAGrandchild(t *testing.T) {
	// cmd.exe (parent) -> ping -t (child that never exits on its own).
	cmd := exec.Command("cmd", "/c", "ping -n 600 127.0.0.1 >NUL")
	PrepareGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	group, err := AdoptGroup(cmd)
	if err != nil {
		t.Fatalf("AdoptGroup: %v", err)
	}
	pid := cmd.Process.Pid
	if !Alive(pid) {
		t.Fatalf("pid %d did not come up", pid)
	}
	// Let the grandchild actually spawn before we kill the tree.
	time.Sleep(500 * time.Millisecond)

	KillTree(pid, group, t.Logf)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if Alive(pid) {
		t.Fatalf("pid %d survived KillTree", pid)
	}
	// And nothing of ours is left: the ping's command line is distinctive.
	if left := ProcessesMatching("ping -n 600 127.0.0.1"); len(left) > 0 {
		t.Errorf("KillTree left %d orphan(s) behind: %v", len(left), left)
	}
	_ = cmd.Wait()
}

// Closing the job handle alone must also reap, because that is the path taken
// when herdr-expose dies without getting to run any cleanup.
func TestClosingTheJobKillsTheTree(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping -n 600 127.0.0.2 >NUL")
	PrepareGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	group, err := AdoptGroup(cmd)
	if err != nil {
		t.Fatalf("AdoptGroup: %v", err)
	}
	pid := cmd.Process.Pid
	time.Sleep(300 * time.Millisecond)

	group.Close() // no TerminateJobObject; KILL_ON_JOB_CLOSE must do it

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && Alive(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	if Alive(pid) {
		t.Errorf("pid %d survived closing the job handle", pid)
	}
	_ = cmd.Wait()
}

func TestProcessesMatchingFindsOurOwnChild(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping -n 600 127.0.0.3 >NUL")
	PrepareGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	group, _ := AdoptGroup(cmd)
	defer func() { KillTree(cmd.Process.Pid, group, t.Logf); _ = cmd.Wait() }()
	time.Sleep(700 * time.Millisecond)

	if got := ProcessesMatching("127.0.0.3"); len(got) == 0 {
		t.Error("ProcessesMatching found none of our own child processes")
	}
	// Two needles must AND, not OR.
	if got := ProcessesMatching("127.0.0.3", "definitely-not-in-any-command-line"); len(got) != 0 {
		t.Errorf("needles OR'd instead of AND'ing: %v", got)
	}
}
