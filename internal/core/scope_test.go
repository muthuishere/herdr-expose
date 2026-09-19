package core

import (
	"context"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

func TestParseScopeForms(t *testing.T) {
	if sc, err := ParseScope("", nil); err != nil || sc.Active() {
		t.Fatalf("empty scope must be inactive: %v %v", sc, err)
	}
	sc, err := ParseScope("alpha", nil)
	if err != nil || !sc.Active() || sc.Session != "alpha" {
		t.Fatalf("session scope: %+v %v", sc, err)
	}
	sc, err = ParseScope("", []string{"alpha/w1:p1", "alpha/w1:p2"})
	if err != nil || sc.Session != "alpha" || len(sc.Panes) != 2 {
		t.Fatalf("target scope: %+v %v", sc, err)
	}
	if _, err := ParseScope("alpha", []string{"beta/w1:p1"}); err == nil {
		t.Fatal("a target from another session must be refused")
	}
	if _, err := ParseScope("alpha/w1:p1", nil); err == nil {
		t.Fatal("--only must refuse a target")
	}
}

// The point of the scope is that an out-of-scope target is UNREACHABLE, not
// merely unlisted: every live path goes through AllowsTarget.
func TestScopeRejectsOtherSessionsAndPanes(t *testing.T) {
	sc, _ := ParseScope("alpha", nil)
	if !sc.AllowsTarget("alpha/w1:p1") {
		t.Fatal("in-scope pane refused")
	}
	if sc.AllowsTarget("beta/w1:p1") {
		t.Fatal("another session's pane accepted")
	}
	if !sc.AllowsTarget("w1:p1") {
		t.Fatal("an unqualified target must resolve to the only session in scope")
	}

	pane, _ := ParseScope("", []string{"alpha/w1:p1"})
	if !pane.AllowsTarget("alpha/w1:p1") {
		t.Fatal("the shared pane itself was refused")
	}
	if pane.AllowsTarget("alpha/w1:p2") {
		t.Fatal("a sibling pane in the same session must be out of scope")
	}
	if pane.AllowsTarget("beta/w1:p1") {
		t.Fatal("same pane id in another session must be out of scope")
	}
}

// A pane-scoped tree must CONTAIN only the shared pane — the others are absent
// from the snapshot entirely, along with any tab or workspace left empty.
func TestFilterSnapshotContainsOnlyScope(t *testing.T) {
	snap := &upstream.Snapshot{
		FocusedPaneID:    "w1:p9",
		FocusedTabID:     "w1:t2",
		FocusedWorkspace: "w2",
		Workspaces:       []upstream.Workspace{{WorkspaceID: "w1"}, {WorkspaceID: "w2"}},
		Tabs: []upstream.Tab{
			{TabID: "w1:t1", WorkspaceID: "w1"},
			{TabID: "w1:t2", WorkspaceID: "w1"},
			{TabID: "w2:t1", WorkspaceID: "w2"},
		},
		Panes: []upstream.Pane{
			{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1"},
			{PaneID: "w1:p9", TabID: "w1:t2", WorkspaceID: "w1"},
			{PaneID: "w2:p1", TabID: "w2:t1", WorkspaceID: "w2"},
		},
		Agents: []upstream.Agent{{PaneID: "w1:p1"}, {PaneID: "w2:p1"}},
	}
	sc, _ := ParseScope("", []string{"alpha/w1:p1"})
	out := sc.filterSnapshot(snap)

	if len(out.Panes) != 1 || out.Panes[0].PaneID != "w1:p1" {
		t.Fatalf("expected only the shared pane, got %+v", out.Panes)
	}
	if len(out.Tabs) != 1 || out.Tabs[0].TabID != "w1:t1" {
		t.Fatalf("empty tabs must be dropped, got %+v", out.Tabs)
	}
	if len(out.Workspaces) != 1 || out.Workspaces[0].WorkspaceID != "w1" {
		t.Fatalf("empty workspaces must be dropped, got %+v", out.Workspaces)
	}
	if len(out.Agents) != 1 || out.Agents[0].PaneID != "w1:p1" {
		t.Fatalf("agents outside the scope leaked: %+v", out.Agents)
	}
	if out.FocusedPaneID != "w1:p1" {
		t.Fatalf("focus must never point at an out-of-scope pane, got %q", out.FocusedPaneID)
	}
	if snap.Panes[0].PaneID != "w1:p1" || len(snap.Panes) != 3 {
		t.Fatal("the upstream snapshot must not be mutated in place")
	}
}

// The command pass-through is narrowed for a scoped instance: prompt /
// send-keys / read / scroll / resize only, and nothing that enumerates or
// mutates the machine.
func TestScopedCommandAllowlist(t *testing.T) {
	sc, _ := ParseScope("alpha", nil)
	for _, m := range []string{"agent.prompt", "pane.send_text", "pane.read", "pane.scroll", "pane.resize"} {
		if !sc.CommandAllowed(m) {
			t.Fatalf("%s should be allowed on a share", m)
		}
	}
	for _, m := range []string{
		"pane.split", "pane.close", "pane.run", "command.invoke",
		"plugin.action.invoke", "plugin.list", "session.snapshot",
		"workspace.list", "tab.create", "server.stop", "agent.list", "pane.list",
	} {
		if sc.CommandAllowed(m) {
			t.Fatalf("%s must NOT be reachable from a scoped share", m)
		}
	}
	var none *Scope
	if !none.CommandAllowed("pane.split") {
		t.Fatal("an unscoped server still passes every method through")
	}
}

// Store.Resolve is the single choke point every live path uses; it must refuse
// an out-of-scope target before any lookup happens.
func TestStoreResolveRefusesOutOfScope(t *testing.T) {
	s := quietStore(t)
	sc, _ := ParseScope("alpha", nil)
	s.SetScope(sc)
	if _, _, _, err := s.Resolve("beta/w1:p1"); !IsOutOfScope(err) {
		t.Fatalf("expected an out-of-scope rejection, got %v", err)
	}
	if c := s.Client("beta"); c != nil {
		t.Fatal("a scoped store must have no client for another session")
	}
}

// A scoped store must never even ATTACH to another session: the out-of-scope
// session is absent from the tree, not marked hidden in it.
func TestScopedStoreTreeContainsOnlyItsSession(t *testing.T) {
	s := quietStore(t)
	sc, _ := ParseScope("alpha", nil)
	s.SetScope(sc)
	f := &fakeLister{list: []upstream.Session{
		{Name: "alpha", SocketPath: "/nonexistent/alpha.sock", Running: true},
		{Name: "beta", SocketPath: "/nonexistent/beta.sock", Running: true},
	}}
	s.Registry().List = f.List
	s.SetRegistryInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	tr := waitFor(t, s, func(tr *Tree) bool { return len(tr.Sessions) > 0 })
	if len(tr.Sessions) != 1 || tr.Sessions[0].Name != "alpha" {
		t.Fatalf("scoped tree must contain only alpha, got %+v", tr.Sessions)
	}
	if got := s.Sessions(); len(got) != 1 {
		t.Fatalf("session enumeration leaked: %v", got)
	}
}
