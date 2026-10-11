package core

import (
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// snapOf builds a one-pane snapshot in a given agent status.
func snapOf(status string) *upstream.Snapshot {
	return &upstream.Snapshot{Panes: []upstream.Pane{
		{PaneID: "w1:p1", Agent: "claude", AgentStatus: status},
	}}
}

func freshMutable(name string) *mutable {
	return &mutable{
		sessions:  map[string]*SessionState{name: {Name: name}},
		doneSeq:   map[string]int64{},
		lastState: map[string]string{},
		changedAt: map[string]time.Time{},
	}
}

// THE ONE THAT MATTERS.
//
// Herdr reports a status, never a timestamp. So the only honest source for
// "idle since when" is having WATCHED the transition. Dating a state on first
// sight dates it to OUR startup, and the phone then renders "idle 12s" on a
// pane that has been idle for six hours — a confident wrong answer to the only
// question the clock exists to answer ("can I close this?").
func TestAStateWeNeverSawStartHasNoTimestamp(t *testing.T) {
	s := quietStore(t)
	m := freshMutable("alpha")

	s.applySnapshot(m, "alpha", snapOf("idle"))

	if ts, ok := m.changedAt["alpha/w1:p1"]; ok {
		t.Fatalf("first sight invented a timestamp (%v); absent is the honest answer", ts)
	}
	if m.lastState["alpha/w1:p1"] != "idle" {
		t.Fatalf("the state itself must still be recorded, got %q", m.lastState["alpha/w1:p1"])
	}
}

// A transition we DID watch is dated, because now we genuinely know.
func TestATransitionWeWatchedIsDated(t *testing.T) {
	s := quietStore(t)
	m := freshMutable("alpha")

	s.applySnapshot(m, "alpha", snapOf("working")) // first sight: undated
	s.applySnapshot(m, "alpha", snapOf("idle"))    // watched: dated

	if _, ok := m.changedAt["alpha/w1:p1"]; !ok {
		t.Fatal("a transition this process watched must carry its timestamp")
	}
}

// Re-reporting the same status is not a transition and must not re-date it:
// the store re-reads the tree every 1.5s, so a bug here would reset every
// agent's clock twice a second and no pane would ever look idle.
func TestRepeatingTheSameStatusDoesNotRestartTheClock(t *testing.T) {
	s := quietStore(t)
	m := freshMutable("alpha")

	s.applySnapshot(m, "alpha", snapOf("working"))
	s.applySnapshot(m, "alpha", snapOf("idle"))
	first := m.changedAt["alpha/w1:p1"]

	for i := 0; i < 5; i++ {
		s.applySnapshot(m, "alpha", snapOf("idle"))
	}
	if got := m.changedAt["alpha/w1:p1"]; !got.Equal(first) {
		t.Fatalf("the clock restarted on an unchanged status: %v -> %v", first, got)
	}
}
