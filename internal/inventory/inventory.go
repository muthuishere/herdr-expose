// Package inventory answers the questions an agent asks about the other agents
// it is sharing a machine with.
//
// `msg agents` says what exists and what state Herdr reports. That is not
// enough to act on. The questions that actually come up are:
//
//   - Who am I? An agent cannot address its own colleagues without knowing
//     which session it is in, and it cannot avoid messaging itself either.
//   - Who is doing nothing, and for how long? A pane idle for three hours is a
//     candidate to close; one idle for ten seconds is between turns.
//   - Who stopped because something BROKE, as opposed to finishing? Herdr
//     reports both as idle, and they need opposite responses.
//   - Who could help with this? Agents in the same session share a workspace
//     and can see each other's panes, so they are the cheap collaborators.
//
// Everything here is read-only and attach-free: agent.list and pane.read take
// no geometry, so taking an inventory cannot resize anybody's pane.
package inventory

import (
	"regexp"
	"strings"
	"time"
)

// State is what an agent is actually doing, as distinct from the status string
// Herdr reports.
const (
	// StateWorking is mid-turn. Leave it alone.
	StateWorking = "working"
	// StateBlocked is stopped on a dialog. A HUMAN must answer it; another
	// agent cannot, and must not try.
	StateBlocked = "blocked"
	// StateIdle finished and is waiting. Free to take work, or to close.
	StateIdle = "idle"
	// StateError is idle BECAUSE SOMETHING BROKE. Herdr cannot tell this from
	// a clean finish -- both are "idle" to it -- so it is read off the screen,
	// and the matched line travels with it so the claim can be checked.
	StateError = "error"
	// StateUnknown is a pane whose agent status Herdr does not report. Every
	// plain shell pane is this, so it is not a fault.
	StateUnknown = "unknown"
)

// errorSignals are the shapes a stopped-because-it-broke screen ends with.
//
// Deliberately narrow. A false "error" is worse than a missed one: it tells
// another agent to intervene where nothing is wrong, and the whole value of
// this field is that it is trustworthy. Each pattern must be something no
// healthy finished turn prints, and the matched LINE is always reported so a
// reader can disagree.
var errorSignals = []*regexp.Regexp{
	// An agent that gave up mid-turn.
	regexp.MustCompile(`(?i)^\s*(error|fatal|panic):`),
	regexp.MustCompile(`(?i)\b(api error|rate limit(ed)?|quota exceeded|overloaded)\b`),
	regexp.MustCompile(`(?i)\b(connection (refused|reset|closed)|network error|ETIMEDOUT|ECONNREFUSED)\b`),
	regexp.MustCompile(`(?i)\bcontext (window )?(is )?(full|exceeded)\b`),
	regexp.MustCompile(`(?i)\b(credit balance|insufficient (credit|quota))\b`),
	regexp.MustCompile(`(?i)^\s*(traceback \(most recent call last\)|panic: runtime error)`),
	regexp.MustCompile(`(?i)\bauthentication (failed|error)\b|\binvalid api key\b`),
}

// TailLines is how much of the end of a screen is examined for a failure. The
// signal, when there is one, is the LAST thing printed.
const TailLines = 12

// ClassifyError reports whether an idle agent stopped because of a failure, and
// the line that says so. Only the tail is examined: an error ten screens back
// was survived, and reporting it would make every long-running pane look broken.
func ClassifyError(screen string) (line string, failed bool) {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > TailLines {
		lines = lines[len(lines)-TailLines:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		for _, re := range errorSignals {
			if re.MatchString(l) {
				return l, true
			}
		}
	}
	return "", false
}

// StateOf maps Herdr's status, plus the screen for an idle agent, onto a State.
//
// The screen is consulted ONLY for an agent Herdr calls finished. A working
// agent's screen is mid-flight and may hold an error it is already handling;
// calling that "error" would be reporting a problem somebody else already owns.
func StateOf(status, screen string) (state, errLine string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "working", "running":
		return StateWorking, ""
	case "blocked":
		return StateBlocked, ""
	case "idle", "done":
		if l, failed := ClassifyError(screen); failed {
			return StateError, l
		}
		return StateIdle, ""
	default:
		return StateUnknown, ""
	}
}

// ClosableAfter is how long an agent must have been idle before this package
// will suggest closing it. Short enough to be useful on a machine with dozens
// of panes, long enough that an agent between turns is never suggested.
const ClosableAfter = 30 * time.Minute
