package msg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Socket is the Herdr implementation that speaks the herdr SOCKET directly,
// instead of spawning the herdr CLI.
//
// WHY THIS EXISTS, and it is not about speed.
//
// Shelling out to `herdr … agent prompt` and `herdr … agent list` starts a
// herdr CLIENT, and a client ATTACHES to the terminal session at its own
// size -- 120x40. If the pane is any other size, that attach SIGWINCHes it:
// the agent's TUI throws its screen away and redraws, and whoever was reading
// the pane watches it jump.
//
// It was reported as "the herdr screen keeps scrolling", and "keeps" is the
// important word. A delivery attaches once, which is bad enough, but the
// service re-lists agents every two seconds for as long as any request is
// still awaiting a reply -- so one unanswered message resized somebody's pane
// every two seconds, indefinitely, while they tried to read it.
//
// The daemon already had an attach-free path and used it everywhere else: the
// web UI's prompt box and the chat runner both call agent.prompt over this
// socket, and the tree is built from session.snapshot. Only this package
// reached for the CLI. The comment that justified it -- that the CLI is
// "Herdr's documented, versioned automation surface" -- is true and was still
// the wrong trade, because the socket is what the rest of the product already
// depends on and it does not touch the pane.
type Socket struct {
	// Client returns a caller for one session, or nil when there is no such
	// session. It is a function rather than a store so this package does not
	// depend on internal/core, which depends on it.
	Client func(session string) Caller
	// Sessions lists the sessions to search. Defaults to the running ones.
	Sessions func(ctx context.Context) ([]upstream.Session, error)
}

// Caller is the one method this needs from a herdr connection.
type Caller interface {
	CallInto(ctx context.Context, method string, params any, out any) error
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
}

const callTimeout = 10 * time.Second

func (s Socket) sessions(ctx context.Context) ([]upstream.Session, error) {
	if s.Sessions != nil {
		return s.Sessions(ctx)
	}
	all, err := upstream.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	return upstream.RunningSessions(all), nil
}

// Agents lists agents across every running session, from session.snapshot.
//
// The same call the daemon already makes to build its tree, so listing agents
// for messaging costs nothing a running daemon was not already paying -- and,
// unlike `herdr agent list`, it does not attach to anything.
func (s Socket) Agents(ctx context.Context) ([]Agent, error) {
	sessions, err := s.sessions(ctx)
	if err != nil {
		return nil, err
	}
	var out []Agent
	for _, sess := range sessions {
		c := s.Client(sess.Name)
		if c == nil {
			continue // one dead session must not hide the others
		}
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		var res struct {
			Snapshot upstream.Snapshot `json:"snapshot"`
		}
		err := c.CallInto(cctx, "session.snapshot", map[string]any{}, &res)
		cancel()
		if err != nil {
			continue
		}
		// A pane's label is what a person named it; it is on the pane rather
		// than the agent, so it has to be looked up by pane id or every agent
		// is addressable only by its pane.
		label := map[string]string{}
		for _, p := range res.Snapshot.Panes {
			if p.Label != "" {
				label[p.PaneID] = p.Label
			}
		}
		for _, a := range res.Snapshot.Agents {
			name := label[a.PaneID]
			id := name
			if id == "" {
				id = a.PaneID
			}
			title := a.TerminalTitleStripped
			if title == "" {
				title = a.TerminalTitle
			}
			out = append(out, Agent{
				Address: sess.Name + "/" + id, Session: sess.Name,
				Name: name, PaneID: a.PaneID, Kind: a.Agent,
				Status: a.AgentStatus, Title: title,
			})
		}
	}
	return out, nil
}

// Prompt submits text to session/target over the socket.
//
// The parameter is `target`, NOT `pane_id`: other methods take pane_id, so the
// wrong one is accepted, delivers nothing, and the only symptom is a message
// that never arrives.
func (s Socket) Prompt(ctx context.Context, session, target, text string) (string, error) {
	c := s.Client(session)
	if c == nil {
		return "agent_not_found", fmt.Errorf("no session %q", session)
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	_, err := c.Call(cctx, "agent.prompt", map[string]any{"target": target, "text": text})
	if err == nil {
		return "", nil
	}
	// Herdr's own refusal codes (agent_blocked, agent_not_found, …) are what
	// the service routes on, so they must survive the transport change: the
	// CLI reported them as JSON on stderr, the socket as a typed error. Losing
	// them would turn "that agent is on a permission dialog" into a generic
	// failure and the request would be retried instead of reported.
	var rpc *upstream.RPCError
	if errors.As(err, &rpc) && rpc.Code != "" {
		return rpc.Code, err
	}
	return "", err
}
