package inventory

import (
	"sort"
	"sync"
	"time"
)

// Agent is one agent and everything the inventory knows about it.
type Agent struct {
	Address string `json:"address"` // session/name, or session/pane when unnamed
	Session string `json:"session"`
	Name    string `json:"name,omitempty"`
	PaneID  string `json:"pane_id"`
	Kind    string `json:"kind,omitempty"` // claude, codex, ...
	Cwd     string `json:"cwd,omitempty"`
	Title   string `json:"title,omitempty"`

	// Status is what Herdr said. State is what that MEANS, which is not the
	// same thing: Herdr calls a finished agent and a crashed one both idle.
	Status string `json:"status,omitempty"`
	State  string `json:"state"`
	// Error is the line that made this StateError. Always reported with the
	// state, so a reader can disagree with the classification.
	Error string `json:"error,omitempty"`

	// Since is when this agent entered its current state, as far as this
	// daemon saw. Absent when the daemon has only just started watching --
	// which is honest, and better than implying it has been idle since boot.
	Since *time.Time `json:"since,omitempty"`
	// IdleSeconds is how long it has been idle. Only set for an idle agent
	// whose Since is known.
	IdleSeconds int `json:"idle_seconds,omitempty"`

	// Closable is a SUGGESTION: idle, not working, not blocked, and idle for
	// longer than ClosableAfter. Reason says why, in words, always.
	Closable bool   `json:"closable"`
	Reason   string `json:"reason,omitempty"`

	// Collaborators are the other agents in this agent's session that are free
	// to take work right now. Same session means a shared workspace and panes
	// they can see, which is what makes them the cheap ones to ask.
	Collaborators []string `json:"collaborators,omitempty"`

	// Self marks the agent asking. An agent that messages itself gets its own
	// envelope back and waits for a reply it is supposed to send.
	Self bool `json:"self,omitempty"`
}

// Tracker remembers when each agent last changed state, because nothing
// upstream does.
//
// Herdr reports a status, not a timestamp: it says "idle", never "idle since
// 9:14". So "this pane has done nothing for three hours and can be closed"
// cannot be answered by asking Herdr -- it has to be WATCHED. The daemon is
// already running, so it watches, and an inventory taken before it had a
// chance to see a transition says so rather than inventing a duration.
type Tracker struct {
	mu   sync.Mutex
	seen map[string]entry // key: session/pane
	now  func() time.Time
}

type entry struct {
	state string
	since time.Time
}

func NewTracker() *Tracker {
	return &Tracker{seen: map[string]entry{}, now: time.Now}
}

func key(session, pane string) string { return session + "/" + pane }

// Observe records the current state of every agent in one round, and returns
// when each entered its state. An agent that has vanished is forgotten, so a
// pane id reused later does not inherit the old one's history.
func (t *Tracker) Observe(agents []Agent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	fresh := make(map[string]entry, len(agents))
	for _, a := range agents {
		k := key(a.Session, a.PaneID)
		if prev, ok := t.seen[k]; ok && prev.state == a.State {
			fresh[k] = prev // unchanged: keep the ORIGINAL timestamp
			continue
		}
		fresh[k] = entry{state: a.State, since: now}
	}
	t.seen = fresh
}

// Since returns when this agent entered its current state, if known.
func (t *Tracker) Since(session, pane, state string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.seen[key(session, pane)]
	if !ok || e.state != state {
		return time.Time{}, false
	}
	return e.since, true
}

// Enrich fills in Since, IdleSeconds, Closable, Reason and Collaborators.
//
// Called after Observe, on the same slice, so the timestamps it reads are the
// ones that round just recorded.
func (t *Tracker) Enrich(agents []Agent, self string) {
	now := t.now()

	// Who is free to help, per session. Collected first: an agent's
	// collaborators are its peers, so everyone has to be classified before
	// anyone's list can be written.
	free := map[string][]string{}
	for _, a := range agents {
		if a.State == StateIdle {
			free[a.Session] = append(free[a.Session], a.Address)
		}
	}
	for s := range free {
		sort.Strings(free[s])
	}

	for i := range agents {
		a := &agents[i]
		a.Self = self != "" && a.Address == self

		if since, ok := t.Since(a.Session, a.PaneID, a.State); ok {
			s := since
			a.Since = &s
			if a.State == StateIdle || a.State == StateError {
				a.IdleSeconds = int(now.Sub(since) / time.Second)
			}
		}

		for _, peer := range free[a.Session] {
			if peer != a.Address {
				a.Collaborators = append(a.Collaborators, peer)
			}
		}

		switch {
		case a.State == StateWorking:
			a.Reason = "mid-turn — leave it alone"
		case a.State == StateBlocked:
			a.Reason = "stopped on a dialog a human must answer"
		case a.State == StateError:
			a.Reason = "stopped after an error: " + a.Error
		case a.State == StateUnknown:
			a.Reason = "herdr reports no agent status for this pane"
		case a.Self:
			a.Reason = "this is you"
		case a.Since == nil:
			a.Reason = "idle, but this daemon has not watched it long enough to say for how long"
		case time.Duration(a.IdleSeconds)*time.Second >= ClosableAfter:
			a.Closable = true
			a.Reason = "idle " + short(time.Duration(a.IdleSeconds)*time.Second) + " — nothing in flight, safe to close"
		default:
			a.Reason = "idle " + short(time.Duration(a.IdleSeconds)*time.Second) + " — may just be between turns"
		}
	}
}

// short renders a duration the way a person says it.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "a moment"
	case d < time.Hour:
		return itoa(int(d/time.Minute)) + "m"
	default:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m == 0 {
			return itoa(h) + "h"
		}
		return itoa(h) + "h" + itoa(m) + "m"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
