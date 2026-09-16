package serve

import (
	"context"
	"time"

	"github.com/muthuishere/herdr-expose/internal/core"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// The client-facing tree shape (SPEC amendment D2 / the web client's contract).
//
// It is NOT Herdr's snapshot forwarded verbatim: Herdr returns four flat lists
// (workspaces, tabs, panes, agents) plus focus ids, and every client would have
// to re-derive the nesting identically or render a different tree. The server
// owns layout, so the server does the nesting once.

// AgentView is the agent bound to a pane.
type AgentView struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind,omitempty"`
	State     string     `json:"state"`
	Summary   string     `json:"summary,omitempty"`
	ChangedAt *time.Time `json:"changed_at,omitempty"`
}

// PaneView is one pane.
type PaneView struct {
	ID         string     `json:"id"`
	TerminalID string     `json:"terminal_id,omitempty"`
	Title      string     `json:"title"`
	Command    string     `json:"command,omitempty"`
	Cwd        string     `json:"cwd,omitempty"`
	Agent      *AgentView `json:"agent,omitempty"`
	Mode       string     `json:"mode,omitempty"`
	Dead       bool       `json:"dead,omitempty"`
	Cols       int        `json:"cols,omitempty"`
	Rows       int        `json:"rows,omitempty"`
	Focused    bool       `json:"focused"`
}

// TabView groups panes.
type TabView struct {
	ID      string     `json:"id"`
	Label   string     `json:"label"`
	Number  int        `json:"number"`
	Focused bool       `json:"focused"`
	Panes   []PaneView `json:"panes"`
}

// WorkspaceView groups tabs.
type WorkspaceView struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Number  int       `json:"number"`
	Focused bool      `json:"focused"`
	Tabs    []TabView `json:"tabs"`
}

// TreeView is the `tree` control frame's data.
type TreeView struct {
	Rev              uint64          `json:"rev"`
	Connected        bool            `json:"connected"`
	HerdrVersion     string          `json:"herdr_version"`
	HerdrProtocol    int             `json:"herdr_protocol"`
	FocusedWorkspace string          `json:"focused_workspace,omitempty"`
	FocusedTab       string          `json:"focused_tab,omitempty"`
	FocusedPane      string          `json:"focused_pane,omitempty"`
	Workspaces       []WorkspaceView `json:"workspaces"`
}

// Agent state enum. `done` is SERVER-DERIVED: idle-but-unseen, per connection.
// A client must never compute it, because "unseen" is a fact about one
// connection and a global answer would let any device wipe another's badge.
const (
	AgentBlocked = "blocked"
	AgentDone    = "done"
	AgentWorking = "working"
	AgentIdle    = "idle"
	AgentUnknown = "unknown"
)

// agentState maps Herdr's agent_status onto the client enum, folding in this
// connection's own seen set.
func agentState(status string, unseen bool) string {
	switch status {
	case "working", "running", "busy":
		return AgentWorking
	case "blocked", "waiting":
		return AgentBlocked
	case "idle", "done", "ready":
		if unseen {
			return AgentDone
		}
		return AgentIdle
	case "":
		return AgentUnknown
	default:
		return AgentUnknown
	}
}

// buildTree renders the tree for one connection.
func buildTree(t *core.Tree, sess *core.Session) TreeView {
	out := TreeView{
		Rev: t.Rev, Connected: t.Connected,
		HerdrVersion: t.Version, HerdrProtocol: t.Protocol,
		Workspaces: []WorkspaceView{},
	}
	snap := t.Snapshot
	if snap == nil {
		return out
	}
	out.FocusedWorkspace = snap.FocusedWorkspace
	out.FocusedTab = snap.FocusedTabID
	out.FocusedPane = snap.FocusedPaneID

	panesByTab := map[string][]PaneView{}
	for i := range snap.Panes {
		p := &snap.Panes[i]
		pv := paneView(p, t, sess)
		panesByTab[p.TabID] = append(panesByTab[p.TabID], pv)
	}
	tabsByWorkspace := map[string][]TabView{}
	for i := range snap.Tabs {
		tb := &snap.Tabs[i]
		tabsByWorkspace[tb.WorkspaceID] = append(tabsByWorkspace[tb.WorkspaceID], TabView{
			ID: tb.TabID, Label: tb.Label, Number: tb.Number,
			Focused: tb.Focused, Panes: nonNilPanes(panesByTab[tb.TabID]),
		})
	}
	for i := range snap.Workspaces {
		ws := &snap.Workspaces[i]
		tabs := tabsByWorkspace[ws.WorkspaceID]
		if tabs == nil {
			tabs = []TabView{}
		}
		out.Workspaces = append(out.Workspaces, WorkspaceView{
			ID: ws.WorkspaceID, Label: ws.Label, Number: ws.Number,
			Focused: ws.Focused, Tabs: tabs,
		})
	}
	return out
}

func nonNilPanes(p []PaneView) []PaneView {
	if p == nil {
		return []PaneView{}
	}
	return p
}

func paneView(p *upstream.Pane, t *core.Tree, sess *core.Session) PaneView {
	title := p.TerminalTitleStripped
	if title == "" {
		title = p.TerminalTitle
	}
	if title == "" {
		title = p.PaneID
	}
	g := sess.Geometry(p.PaneID)
	pv := PaneView{
		ID: p.PaneID, TerminalID: p.TerminalID, Title: title,
		Cwd: p.ForegroundCwd, Focused: p.Focused,
		Cols: g.Cols, Rows: g.Rows,
	}
	if pv.Cwd == "" {
		pv.Cwd = p.Cwd
	}
	if p.AgentStatus == "exited" {
		pv.Dead = true
	}
	if p.Agent != "" || p.AgentStatus != "" {
		id := p.TerminalID // stable across moves, unlike pane_id
		if p.AgentSession != nil && p.AgentSession.Value != "" {
			id = p.AgentSession.Value
		}
		av := &AgentView{
			ID:    id,
			Kind:  p.Agent,
			State: agentState(p.AgentStatus, sess.Seen.Unseen(p.PaneID, t.DoneSeq[p.PaneID])),
		}
		if ts, ok := t.ChangedAt[p.PaneID]; ok {
			at := ts
			av.ChangedAt = &at
		}
		pv.Agent = av
	}
	return pv
}

// agentFrame is the `agent` control frame.
//
// `detection` rides this frame because the tree has no detection field and the
// blocked-agent Q&A view has nothing to render without it. It is fetched only
// for blocked agents, and only when the state actually changed.
type agentFrame struct {
	Target    string     `json:"target"`
	ID        string     `json:"id"`
	Kind      string     `json:"kind,omitempty"`
	State     string     `json:"state"`
	Summary   string     `json:"summary,omitempty"`
	ChangedAt *time.Time `json:"changed_at,omitempty"`
	Detection string     `json:"detection,omitempty"`
}

// emitAgents sends an `agent` frame for every pane whose agent state changed
// for THIS connection since the last tree push, and unconditionally on the
// first push so an already-blocked pane arrives with its question text.
func (s *Server) emitAgents(ctx context.Context, conn *wsConn, view TreeView, first bool) {
	client := s.hub.Store().Client()
	for _, ws := range view.Workspaces {
		for _, tab := range ws.Tabs {
			for _, p := range tab.Panes {
				if p.Agent == nil {
					continue
				}
				prev, seen := conn.agentState[p.ID]
				if seen && prev == p.Agent.State && !first {
					continue
				}
				conn.agentState[p.ID] = p.Agent.State

				f := agentFrame{
					Target: p.ID, ID: p.Agent.ID, Kind: p.Agent.Kind,
					State: p.Agent.State, Summary: p.Title, ChangedAt: p.Agent.ChangedAt,
				}
				if p.Agent.State == AgentBlocked {
					rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
					// `detection` is the same bottom-buffer region Herdr
					// classifies on, so the Q&A view stays generic across
					// agent kinds with no per-agent parsing.
					if text, err := client.PaneRead(rctx, p.ID, "detection", "text", 0); err == nil {
						f.Detection = text
					}
					cancel()
				}
				conn.SendJSON("agent", f)
			}
		}
	}
}
