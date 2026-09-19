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
	// Cols/Rows are THE PANE'S OWN SIZE as herdr reports it, not the size this
	// browser would like it to be.
	//
	// They used to carry our own requested geometry, which made the tree agree
	// with us about a number we had no business choosing. A client that wants
	// to show a terminal renders at THESE, scaled to whatever box it has; only
	// an explicit, confirmed "fit to my window" sends a `resize`, and only that
	// changes what the owner sees.
	Cols    int  `json:"cols,omitempty"`
	Rows    int  `json:"rows,omitempty"`
	Focused bool `json:"focused"`
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

// SessionView is one Herdr session: the new top level of the tree.
//
// `id` is the SESSION NAME, not a pane id: names survive a server restart while
// pane ids are re-minted, so a client's saved "last pane" only means something
// when it is qualified by a stable session id.
type SessionView struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Running       bool            `json:"running"`
	Connected     bool            `json:"connected"`
	Focused       bool            `json:"focused"`
	Origin        bool            `json:"origin,omitempty"`
	Default       bool            `json:"default,omitempty"`
	HerdrVersion  string          `json:"herdr_version,omitempty"`
	HerdrProtocol int             `json:"herdr_protocol,omitempty"`
	Error         string          `json:"error,omitempty"`
	FocusedPane   string          `json:"focused_pane,omitempty"`
	Workspaces    []WorkspaceView `json:"workspaces"`
}

// TreeView is the `tree` control frame's data.
type TreeView struct {
	Rev           uint64 `json:"rev"`
	Connected     bool   `json:"connected"`
	HerdrVersion  string `json:"herdr_version"`
	HerdrProtocol int    `json:"herdr_protocol"`
	// FocusedSession is the session id a client should land on.
	FocusedSession string `json:"focused_session,omitempty"`
	// FocusedPane is session-qualified, like every other target on this wire.
	FocusedPane string        `json:"focused_pane,omitempty"`
	Sessions    []SessionView `json:"sessions"`
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

// buildTree renders the whole multi-session tree for one connection.
//
// Every id it emits is SESSION-QUALIFIED (`<session>/<herdr id>`). That is not
// cosmetic: each Herdr session mints its own ids starting at w1:p1, so bare ids
// collide across sessions and a client keyed on them would route input to the
// wrong machine's pane.
func buildTree(t *core.Tree, sess *core.Session) TreeView {
	out := TreeView{
		Rev: t.Rev, Connected: t.Connected,
		HerdrVersion: t.Version, HerdrProtocol: t.Protocol,
		FocusedSession: t.FocusedSession,
		Sessions:       []SessionView{},
	}
	for _, st := range t.Sessions {
		sv := SessionView{
			ID: st.Name, Name: st.Name,
			Running: st.Running, Connected: st.Connected,
			Focused: st.Name == t.FocusedSession,
			Origin:  st.Origin, Default: st.Default,
			HerdrVersion: st.Version, HerdrProtocol: st.Protocol,
			Error:      st.Err,
			Workspaces: buildWorkspaces(st.Name, st.Snapshot, t, sess),
		}
		if snap := st.Snapshot; snap != nil && snap.FocusedPaneID != "" {
			sv.FocusedPane = core.JoinTarget(st.Name, snap.FocusedPaneID)
			if sv.Focused {
				out.FocusedPane = sv.FocusedPane
			}
		}
		out.Sessions = append(out.Sessions, sv)
	}
	return out
}

func buildWorkspaces(session string, snap *upstream.Snapshot, t *core.Tree, sess *core.Session) []WorkspaceView {
	out := []WorkspaceView{}
	if snap == nil {
		return out
	}
	panesByTab := map[string][]PaneView{}
	for i := range snap.Panes {
		p := &snap.Panes[i]
		cols, rows := snap.PaneSize(p.PaneID)
		panesByTab[p.TabID] = append(panesByTab[p.TabID], paneView(session, p, cols, rows, t, sess))
	}
	tabsByWorkspace := map[string][]TabView{}
	for i := range snap.Tabs {
		tb := &snap.Tabs[i]
		tabsByWorkspace[tb.WorkspaceID] = append(tabsByWorkspace[tb.WorkspaceID], TabView{
			ID: core.JoinTarget(session, tb.TabID), Label: tb.Label, Number: tb.Number,
			Focused: tb.Focused, Panes: nonNilPanes(panesByTab[tb.TabID]),
		})
	}
	for i := range snap.Workspaces {
		ws := &snap.Workspaces[i]
		tabs := tabsByWorkspace[ws.WorkspaceID]
		if tabs == nil {
			tabs = []TabView{}
		}
		out = append(out, WorkspaceView{
			ID: core.JoinTarget(session, ws.WorkspaceID), Label: ws.Label, Number: ws.Number,
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

func paneView(session string, p *upstream.Pane, cols, rows int, t *core.Tree, sess *core.Session) PaneView {
	target := core.JoinTarget(session, p.PaneID)
	// Precedence matters: an explicit `herdr pane rename` must beat a terminal
	// title, because a title is whatever the shell last wrote to OSC 0/2 and
	// changes on its own, while a label is a deliberate human choice. Herdr's
	// own resolved Title already folds label > agent > terminal title, so it
	// comes second. The pane id is a last resort and is never a good label —
	// rendering one is the bug the e2e suite now guards against.
	title := p.Label
	if title == "" {
		title = p.Title
	}
	if title == "" {
		title = p.TerminalTitleStripped
	}
	if title == "" {
		title = p.TerminalTitle
	}
	if title == "" {
		title = p.PaneID
	}
	pv := PaneView{
		ID: target, TerminalID: p.TerminalID, Title: title,
		Cwd: p.ForegroundCwd, Focused: p.Focused,
		Cols: cols, Rows: rows,
	}
	// A stream that is actually attached knows better than the snapshot does:
	// herdr stamps every frame with the size it is rendering at, and for a
	// pane-matched attach that IS the pane's size, measured rather than
	// assembled from two different fields.
	if a := sess.AttachedGeometry(target); a.Valid() {
		pv.Cols, pv.Rows = a.Cols, a.Rows
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
			State: agentState(p.AgentStatus, sess.Seen.Unseen(target, t.DoneSeq[target])),
		}
		if ts, ok := t.ChangedAt[target]; ok {
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
	Session   string     `json:"session"`
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
//
// It runs ON the connection's treeLoop goroutine (see ws.go). It used to be
// spawned per push, which raced the plain agentState map into a fatal
// concurrent map write; the change-detection state now lives behind
// conn.agentChanged and the pushes themselves are serialised.
func (s *Server) emitAgents(ctx context.Context, conn *wsConn, view TreeView, first bool) {
	store := s.hub.Store()
	for _, sv := range view.Sessions {
		for _, ws := range sv.Workspaces {
			for _, tab := range ws.Tabs {
				for _, p := range tab.Panes {
					if p.Agent == nil {
						continue
					}
					if !conn.agentChanged(p.ID, p.Agent.State, first) {
						continue
					}

					f := agentFrame{
						Target: p.ID, Session: sv.ID, ID: p.Agent.ID, Kind: p.Agent.Kind,
						State: p.Agent.State, Summary: p.Title, ChangedAt: p.Agent.ChangedAt,
					}
					if p.Agent.State == AgentBlocked {
						// Route the read to THIS session's socket: a bare pane
						// id would otherwise read the same id in another one.
						if _, c, id, err := store.Resolve(p.ID); err == nil {
							rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
							// `detection` is the same bottom-buffer region
							// Herdr classifies on, so the Q&A view stays
							// generic across agent kinds with no per-agent
							// parsing.
							if text, err := c.PaneRead(rctx, id, "detection", "text", 0); err == nil {
								f.Detection = text
							}
							cancel()
						}
					}
					conn.SendJSON("agent", f)
				}
			}
		}
	}
}
