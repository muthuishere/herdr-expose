package core

import (
	"fmt"
	"sort"
	"strings"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Scope narrows a whole herdr-expose instance down to ONE Herdr session (and
// optionally to specific panes inside it). It is the server-side half of
// `herdr-expose share` (SPEC AMENDMENTS 9, G2).
//
// The rule that matters: scope is enforced HERE, in the store/view layer, not
// in the client. An out-of-scope pane is not hidden — it does not exist to a
// scoped instance. Its session is never attached to, so there is no upstream
// client to route to; it is absent from the tree, absent from `subscribe`,
// absent from the summary poller, and every Resolve of a target outside the
// scope fails with ErrOutOfScope. A scoped share must never be one client bug
// away from exposing the rest of the machine.
//
// The zero value (and a nil *Scope) means "no scope": the normal, unrestricted
// multi-session server.
type Scope struct {
	// Session is the ONLY Herdr session this instance may see.
	Session string
	// Panes, when non-empty, restricts the scope further to these bare Herdr
	// pane ids (`w1:p1`) inside Session.
	Panes []string

	paneSet map[string]struct{}
}

// ErrOutOfScope is returned for any target this instance is not allowed to
// see. It deliberately reads the same whether the target exists or not.
type outOfScopeError struct{ target string }

func (e *outOfScopeError) Error() string {
	return "core: target is outside this instance's share scope: " + e.target
}

// IsOutOfScope reports whether err is a scope rejection.
func IsOutOfScope(err error) bool {
	_, ok := err.(*outOfScopeError)
	return ok
}

// ErrOutOfScope builds the rejection for a target.
func ErrOutOfScope(target string) error { return &outOfScopeError{target: target} }

// ParseScope builds a Scope from the two CLI forms:
//
//	--only <session>                   whole session
//	--only-target <session>/<pane>     one pane (repeatable, comma separated)
//
// Both may be given; --only-target's session must agree with --only.
func ParseScope(only string, onlyTargets []string) (*Scope, error) {
	only = strings.TrimSpace(only)
	var sc *Scope
	if only != "" {
		if strings.Contains(only, TargetSep) {
			return nil, fmt.Errorf("--only takes a SESSION name, not a target (%q); use --only-target for a pane", only)
		}
		sc = &Scope{Session: only}
	}
	for _, raw := range onlyTargets {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			session, pane := SplitTarget(part)
			if session == "" || pane == "" {
				return nil, fmt.Errorf("--only-target must be <session>/<pane>, got %q", part)
			}
			if sc == nil {
				sc = &Scope{Session: session}
			}
			if sc.Session != session {
				return nil, fmt.Errorf("--only-target %q names session %q but the scope is already pinned to %q",
					part, session, sc.Session)
			}
			sc.Panes = append(sc.Panes, pane)
		}
	}
	if sc == nil {
		return nil, nil
	}
	sc.index()
	return sc, nil
}

func (s *Scope) index() {
	if s == nil {
		return
	}
	sort.Strings(s.Panes)
	s.paneSet = make(map[string]struct{}, len(s.Panes))
	for _, p := range s.Panes {
		s.paneSet[p] = struct{}{}
	}
}

// Active reports whether this scope restricts anything.
func (s *Scope) Active() bool { return s != nil && s.Session != "" }

// String is the human/JSON-stable description of the scope.
func (s *Scope) String() string {
	if !s.Active() {
		return ""
	}
	if len(s.Panes) == 0 {
		return s.Session
	}
	out := make([]string, 0, len(s.Panes))
	for _, p := range s.Panes {
		out = append(out, JoinTarget(s.Session, p))
	}
	return strings.Join(out, ",")
}

// AllowsSession reports whether a Herdr session is inside the scope.
func (s *Scope) AllowsSession(name string) bool {
	if !s.Active() {
		return true
	}
	return name == s.Session
}

// AllowsPane reports whether a BARE Herdr pane id is inside the scope.
func (s *Scope) AllowsPane(id string) bool {
	if !s.Active() {
		return true
	}
	if len(s.paneSet) == 0 {
		return true
	}
	// A pane-scoped share still has to let through the tab/workspace ids that
	// contain the pane, because the tree nests them — but never another PANE.
	if !strings.Contains(id, ":p") {
		return true
	}
	_, ok := s.paneSet[id]
	return ok
}

// AllowsTarget reports whether a wire target (`<session>/<id>`, or a bare id
// meaning the default session) is inside the scope.
func (s *Scope) AllowsTarget(target string) bool {
	if !s.Active() {
		return true
	}
	session, id := SplitTarget(target)
	if session == "" {
		session = s.Session // unqualified resolves to the only session there is
	}
	return session == s.Session && s.AllowsPane(id)
}

// filterSnapshot returns a copy of snap containing ONLY in-scope panes, with
// tabs and workspaces that end up empty removed. It is what makes the tree
// CONTAIN the share rather than merely mark the rest as hidden.
func (s *Scope) filterSnapshot(snap *upstream.Snapshot) *upstream.Snapshot {
	if snap == nil || !s.Active() || len(s.paneSet) == 0 {
		return snap
	}
	out := *snap
	out.Panes = nil
	keepTab := map[string]bool{}
	keepWS := map[string]bool{}
	for _, p := range snap.Panes {
		if _, ok := s.paneSet[p.PaneID]; !ok {
			continue
		}
		out.Panes = append(out.Panes, p)
		keepTab[p.TabID] = true
		keepWS[p.WorkspaceID] = true
	}
	out.Tabs = nil
	for _, t := range snap.Tabs {
		if keepTab[t.TabID] {
			out.Tabs = append(out.Tabs, t)
		}
	}
	out.Workspaces = nil
	for _, w := range snap.Workspaces {
		if keepWS[w.WorkspaceID] {
			out.Workspaces = append(out.Workspaces, w)
		}
	}
	out.Agents = nil
	for _, a := range snap.Agents {
		if _, ok := s.paneSet[a.PaneID]; ok {
			out.Agents = append(out.Agents, a)
		}
	}
	out.Layouts = nil
	if _, ok := s.paneSet[out.FocusedPaneID]; !ok {
		// Never point a client at a pane it is not allowed to see.
		out.FocusedPaneID = ""
		if len(out.Panes) > 0 {
			out.FocusedPaneID = out.Panes[0].PaneID
		}
	}
	if !keepTab[out.FocusedTabID] {
		out.FocusedTabID = ""
	}
	if !keepWS[out.FocusedWorkspace] {
		out.FocusedWorkspace = ""
	}
	return &out
}

// ScopedCommands is the method allowlist for a SCOPED instance (G2): prompt,
// send-keys, read, scroll and resize, on in-scope targets only.
//
// Everything else is refused by construction — pane.split/close/move, tab and
// workspace mutation, worktrees, `command.invoke`, `plugin.*`, `server.*`, and
// every listing method that would enumerate what the share is not allowed to
// see. A share is a public RCE endpoint scoped to one agent session; the
// pass-through that makes the unscoped server pleasant is exactly what must not
// survive into it.
var ScopedCommands = map[string]struct{}{
	"agent.prompt":        {},
	"agent.send_keys":     {},
	"agent.read":          {},
	"pane.send_text":      {},
	"pane.send_keys":      {},
	"pane.send_input":     {},
	"pane.read":           {},
	"pane.scroll":         {},
	"pane.resize":         {},
	"pane.selection.read": {},
	"pane.copy_search":    {},
	"pane.copy_motion":    {},
}

// CommandAllowed reports whether a scoped instance may pass a method through.
func (s *Scope) CommandAllowed(method string) bool {
	if !s.Active() {
		return true
	}
	_, ok := ScopedCommands[method]
	return ok
}
