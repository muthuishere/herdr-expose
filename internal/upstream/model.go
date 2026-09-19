package upstream

import "encoding/json"

// Types mirroring the shapes verified live against Herdr 0.9.0 / protocol 22.
// Deliberately loose: unknown fields are ignored, and the raw snapshot is kept
// so we can forward tree data to clients without re-deriving it.

// Request is the newline-delimited JSON request envelope.
type Request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// Response is the newline-delimited JSON response envelope.
type Response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError is Herdr's error payload.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

// Pong is the result of the ping method.
type Pong struct {
	Type         string          `json:"type"`
	Version      string          `json:"version"`
	Protocol     int             `json:"protocol"`
	Capabilities map[string]any  `json:"capabilities"`
	Raw          json.RawMessage `json:"-"`
}

// AgentSession identifies the agent conversation bound to a terminal.
type AgentSession struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

// Scroll is a pane's scrollback position.
type Scroll struct {
	OffsetFromBottom    int `json:"offset_from_bottom"`
	MaxOffsetFromBottom int `json:"max_offset_from_bottom"`
	ViewportRows        int `json:"viewport_rows"`
}

// Pane is one terminal pane.
type Pane struct {
	PaneID        string `json:"pane_id"`
	TerminalID    string `json:"terminal_id"`
	WorkspaceID   string `json:"workspace_id"`
	TabID         string `json:"tab_id"`
	Focused       bool   `json:"focused"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
	Agent         string `json:"agent,omitempty"`
	// Label is what the user explicitly named this pane (`herdr pane rename`).
	// It must win over any terminal title: a title is whatever the shell last
	// wrote to OSC 0/2 and changes under you, whereas a label is a deliberate
	// human choice. Missing this field meant a renamed pane silently lost its
	// name in the web UI.
	Label string `json:"label,omitempty"`
	// Title is Herdr's own resolved display title, which already folds in the
	// label, the agent name and the terminal title.
	Title                 string        `json:"title,omitempty"`
	DisplayAgent          string        `json:"display_agent,omitempty"`
	TerminalTitle         string        `json:"terminal_title,omitempty"`
	TerminalTitleStripped string        `json:"terminal_title_stripped,omitempty"`
	AgentStatus           string        `json:"agent_status,omitempty"`
	AgentSession          *AgentSession `json:"agent_session,omitempty"`
	Scroll                *Scroll       `json:"scroll,omitempty"`
	Revision              int64         `json:"revision"`
}

// Tab groups panes.
type Tab struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus string `json:"agent_status,omitempty"`
}

// Workspace groups tabs.
type Workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	TabCount    int    `json:"tab_count"`
	ActiveTabID string `json:"active_tab_id"`
	AgentStatus string `json:"agent_status,omitempty"`
}

// Agent is an agent bound to a terminal.
type Agent struct {
	TerminalID            string        `json:"terminal_id"`
	Agent                 string        `json:"agent"`
	TerminalTitle         string        `json:"terminal_title,omitempty"`
	TerminalTitleStripped string        `json:"terminal_title_stripped,omitempty"`
	AgentStatus           string        `json:"agent_status,omitempty"`
	AgentSession          *AgentSession `json:"agent_session,omitempty"`
	WorkspaceID           string        `json:"workspace_id"`
	TabID                 string        `json:"tab_id"`
	PaneID                string        `json:"pane_id"`
	Focused               bool          `json:"focused"`
	StateChangeSeq        int64         `json:"state_change_seq"`
	Cwd                   string        `json:"cwd"`
	ForegroundCwd         string        `json:"foreground_cwd"`
	Revision              int64         `json:"revision"`
}

// Snapshot is the whole session tree, from session.snapshot.
type Snapshot struct {
	Version          string            `json:"version"`
	Protocol         int               `json:"protocol"`
	FocusedWorkspace string            `json:"focused_workspace_id"`
	FocusedTabID     string            `json:"focused_tab_id"`
	FocusedPaneID    string            `json:"focused_pane_id"`
	Workspaces       []Workspace       `json:"workspaces"`
	Tabs             []Tab             `json:"tabs"`
	Panes            []Pane            `json:"panes"`
	Layouts          []json.RawMessage `json:"layouts"`
	Agents           []Agent           `json:"agents"`
}

type sessionSnapshotResult struct {
	Type     string   `json:"type"`
	Snapshot Snapshot `json:"snapshot"`
}

// PaneReadResult is the result of pane.read / agent.read.
type PaneReadResult struct {
	Type string `json:"type"`
	Read struct {
		PaneID      string `json:"pane_id"`
		WorkspaceID string `json:"workspace_id"`
		TabID       string `json:"tab_id"`
		Source      string `json:"source"`
		Format      string `json:"format"`
		Text        string `json:"text"`
	} `json:"read"`
}

// Event is one pushed event on a subscription connection.
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// TerminalRecord is one NDJSON line on a `herdr terminal session` subprocess.
// Verified shape: {"type":"terminal.frame","seq":1,"bytes":"<b64>",
// "encoding":"ansi","full":true,"width":100,"height":30}
type TerminalRecord struct {
	Type     string `json:"type"`
	Seq      uint64 `json:"seq"`
	Bytes    string `json:"bytes"`
	Encoding string `json:"encoding"`
	Full     bool   `json:"full"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Reason   string `json:"reason"`
}

// GlobalSubscriptions are the event kinds that take no extra parameters.
// pane.output_matched / pane.agent_status_changed / pane.scroll_changed each
// require a pane_id, so they are not usable as session-wide subscriptions.
var GlobalSubscriptions = []string{
	"workspace.created", "workspace.updated", "workspace.metadata_updated",
	"workspace.renamed", "workspace.moved", "workspace.reordered",
	"workspace.closed", "workspace.focused",
	"worktree.created", "worktree.opened", "worktree.removed",
	"tab.created", "tab.closed", "tab.focused", "tab.renamed", "tab.moved",
	"pane.created", "pane.closed", "pane.updated", "pane.focused",
	"pane.moved", "pane.exited", "pane.agent_detected",
	"layout.updated",
}
