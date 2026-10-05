package inventory

import (
	"testing"
	"time"
)

// Herdr calls a finished agent and a crashed one both "idle". They need
// opposite responses, so the difference is read off the screen -- and the line
// that decided it is always reported, because a classification nobody can check
// is worse than none.
func TestAnIdleAgentThatBrokeIsNotTheSameAsOneThatFinished(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		want   string
	}{
		{"clean finish", "⏺ Done. The tests pass.\n", StateIdle},
		{"api error", "⏺ working…\nAPI Error: overloaded_error\n", StateError},
		{"rate limited", "you are rate limited, retry after 60s\n", StateError},
		{"connection reset", "fetch failed: connection reset by peer\n", StateError},
		{"python traceback", "Traceback (most recent call last):\n  File x\n", StateError},
		{"auth", "Authentication failed: invalid api key\n", StateError},
		{"credits", "Your credit balance is too low\n", StateError},
		// The ones that must NOT fire: ordinary prose about errors is the
		// commonest thing a coding agent prints.
		{"talking about errors", "I fixed the error handling in parser.go and added a test.\n", StateIdle},
		{"a filename", "  edited internal/core/errors.go\n", StateIdle},
		{"past tense report", "The connection error we saw yesterday is gone now.\n", StateIdle},
		{"empty", "", StateIdle},
	} {
		got, line := StateOf("done", tc.screen)
		if got != tc.want {
			t.Fatalf("%s: state = %q, want %q (line %q)", tc.name, got, tc.want, line)
		}
		if got == StateError && line == "" {
			t.Fatalf("%s: classified as error with no line to show for it", tc.name)
		}
	}
}

// An error ten screens back was survived. Reporting it would make every
// long-running pane look broken.
func TestOnlyTheTailIsExamined(t *testing.T) {
	screen := "API Error: overloaded_error\n"
	for i := 0; i < TailLines+5; i++ {
		screen += "⏺ carried on and did more work\n"
	}
	if got, _ := StateOf("idle", screen); got != StateIdle {
		t.Fatalf("state = %q, want idle: an old error is not a current failure", got)
	}
}

// A working agent's screen is mid-flight. An error on it may be one the agent
// is already handling, and calling that a failure reports a problem somebody
// else already owns.
func TestAWorkingAgentIsNeverClassifiedFromItsScreen(t *testing.T) {
	if got, _ := StateOf("working", "API Error: overloaded_error\n"); got != StateWorking {
		t.Fatalf("state = %q, want working", got)
	}
	if got, _ := StateOf("blocked", "Error: nope\n"); got != StateBlocked {
		t.Fatalf("state = %q, want blocked", got)
	}
}

func TestIdleDurationIsWatchedNotInvented(t *testing.T) {
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.now = func() time.Time { return now }

	agents := []Agent{
		{Address: "s/a", Session: "s", PaneID: "w1:p1", State: StateIdle},
		{Address: "s/b", Session: "s", PaneID: "w2:p1", State: StateWorking},
	}
	tr.Observe(agents)
	tr.Enrich(agents, "s/b")

	// Seen for the first time THIS instant: it is idle, but nobody can say for
	// how long, and the inventory says exactly that instead of guessing.
	if agents[0].Closable {
		t.Fatal("suggested closing a pane it has watched for zero seconds")
	}
	if agents[0].IdleSeconds != 0 {
		t.Fatalf("idle_seconds = %d, want 0", agents[0].IdleSeconds)
	}

	// Still idle an hour later: the ORIGINAL timestamp must survive, or every
	// round would reset the clock and nothing would ever look closable.
	now = now.Add(time.Hour)
	tr.Observe(agents)
	tr.Enrich(agents, "s/b")
	if agents[0].IdleSeconds != 3600 {
		t.Fatalf("idle_seconds = %d, want 3600", agents[0].IdleSeconds)
	}
	if !agents[0].Closable {
		t.Fatalf("an hour idle is not closable? reason=%q", agents[0].Reason)
	}
	if agents[1].Closable {
		t.Fatal("a WORKING agent was suggested for closing")
	}
	if agents[1].Reason == "" {
		t.Fatal("no reason given for the working agent")
	}
	if !agents[1].Self {
		t.Fatal("the asking agent was not marked as itself")
	}
}

// A state change restarts the clock: an agent that worked for a minute and
// then went idle has been idle for seconds, not for the hour before that.
func TestAStateChangeRestartsTheClock(t *testing.T) {
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.now = func() time.Time { return now }

	a := []Agent{{Address: "s/a", Session: "s", PaneID: "w1:p1", State: StateIdle}}
	tr.Observe(a)
	now = now.Add(2 * time.Hour)
	a[0].State = StateWorking
	tr.Observe(a)
	now = now.Add(time.Minute)
	a[0].State = StateIdle
	tr.Observe(a)
	tr.Enrich(a, "")
	if a[0].IdleSeconds != 0 {
		t.Fatalf("idle_seconds = %d, want 0 — it only just went idle", a[0].IdleSeconds)
	}
	if a[0].Closable {
		t.Fatal("closable one minute after it stopped working")
	}
}

// Collaborators are the free agents in the SAME session, and never yourself.
func TestCollaboratorsAreFreeAgentsInTheSameSession(t *testing.T) {
	tr := NewTracker()
	agents := []Agent{
		{Address: "s1/a", Session: "s1", PaneID: "w1:p1", State: StateIdle},
		{Address: "s1/b", Session: "s1", PaneID: "w2:p1", State: StateIdle},
		{Address: "s1/c", Session: "s1", PaneID: "w3:p1", State: StateWorking},
		{Address: "s2/d", Session: "s2", PaneID: "w1:p1", State: StateIdle},
	}
	tr.Observe(agents)
	tr.Enrich(agents, "s1/a")

	if len(agents[0].Collaborators) != 1 || agents[0].Collaborators[0] != "s1/b" {
		t.Fatalf("collaborators = %v, want [s1/b]", agents[0].Collaborators)
	}
	// A busy agent is not offered as a collaborator, and another session's
	// agent is not either: a different session is a different workspace.
	for _, c := range agents[0].Collaborators {
		if c == "s1/c" || c == "s2/d" {
			t.Fatalf("offered %s as a collaborator", c)
		}
	}
	if len(agents[3].Collaborators) != 0 {
		t.Fatalf("the lone agent in s2 has collaborators: %v", agents[3].Collaborators)
	}
}
