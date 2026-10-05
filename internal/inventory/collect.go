package inventory

import (
	"context"
	"strings"
	"time"
)

// Source is what collecting an inventory needs. Both calls are attach-free:
// agent.list and pane.read take no geometry, so taking an inventory cannot
// resize anybody's pane. That is the rule the whole product rests on and it
// applies here too.
type Source interface {
	// Agents lists every agent on this machine.
	Agents(ctx context.Context) ([]Agent, error)
	// Screen returns the tail of one pane's buffer, for error classification.
	Screen(ctx context.Context, session, pane string, lines int) (string, error)
}

// Collect takes a full inventory. self is the asking agent's address, or "".
//
// Screens are read ONLY for agents Herdr reports as finished, because that is
// the only case where the screen decides anything. On a machine with forty
// panes that is the difference between forty reads and the handful that are
// actually idle.
func Collect(ctx context.Context, src Source, tr *Tracker, self string) ([]Agent, error) {
	agents, err := src.Agents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range agents {
		a := &agents[i]
		status := strings.ToLower(strings.TrimSpace(a.Status))
		if status == "idle" || status == "done" {
			sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			screen, rerr := src.Screen(sctx, a.Session, a.PaneID, TailLines*2)
			cancel()
			if rerr != nil {
				// A pane that will not answer is not evidence of a failure.
				// It stays plain idle rather than being called broken.
				screen = ""
			}
			a.State, a.Error = StateOf(a.Status, screen)
			continue
		}
		a.State, a.Error = StateOf(a.Status, "")
	}
	tr.Observe(agents)
	tr.Enrich(agents, self)
	return agents, nil
}

// Summary is the one-paragraph answer: how many of each, and what to do.
type Summary struct {
	Machine  string   `json:"machine"`
	Self     *Self    `json:"self,omitempty"`
	Sessions []string `json:"sessions"`
	Counts   Counts   `json:"counts"`
	Agents   []Agent  `json:"agents"`
}

// Self is who is asking: the one thing an agent cannot work out from a list.
type Self struct {
	Machine string `json:"machine"`
	Session string `json:"session"`
	PaneID  string `json:"pane_id,omitempty"`
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
	Cwd     string `json:"cwd,omitempty"`
}

type Counts struct {
	Total    int `json:"total"`
	Working  int `json:"working"`
	Idle     int `json:"idle"`
	Blocked  int `json:"blocked"`
	Error    int `json:"error"`
	Closable int `json:"closable"`
}

// Summarise counts the states and lists the sessions, in a stable order.
func Summarise(machine string, self *Self, agents []Agent) Summary {
	s := Summary{Machine: machine, Self: self, Agents: agents, Sessions: []string{}}
	seen := map[string]bool{}
	for _, a := range agents {
		if !seen[a.Session] {
			seen[a.Session] = true
			s.Sessions = append(s.Sessions, a.Session)
		}
		s.Counts.Total++
		switch a.State {
		case StateWorking:
			s.Counts.Working++
		case StateIdle:
			s.Counts.Idle++
		case StateBlocked:
			s.Counts.Blocked++
		case StateError:
			s.Counts.Error++
		}
		if a.Closable {
			s.Counts.Closable++
		}
	}
	return s
}
