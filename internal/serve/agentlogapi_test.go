package serve

import (
	"testing"

	"github.com/muthuishere/herdr-expose/internal/core"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// tree builds a two-session tree: each session has one agent pane carrying a
// distinct agent_session id, plus one plain pane with none.
func tree() *core.Tree {
	mk := func(name, pane, sid string) *core.SessionState {
		panes := []upstream.Pane{{PaneID: "w9:p9"}} // a plain shell: no agent session
		if sid != "" {
			panes = append(panes, upstream.Pane{
				PaneID: pane,
				Agent:  "claude",
				AgentSession: &upstream.AgentSession{
					Source: "herdr:claude", Agent: "claude", Kind: "id", Value: sid,
				},
			})
		}
		return &core.SessionState{Name: name, Snapshot: &upstream.Snapshot{Panes: panes}}
	}
	return &core.Tree{Sessions: []*core.SessionState{
		mk("alpha", "w1:p1", "11111111-1111-1111-1111-111111111111"),
		mk("beta", "w2:p2", "22222222-2222-2222-2222-222222222222"),
	}}
}

// With no scope, any visible agent resolves.
func TestAnUnscopedInstanceResolvesAnyAgentSession(t *testing.T) {
	for id, want := range map[string]string{
		"11111111-1111-1111-1111-111111111111": "alpha/w1:p1",
		"22222222-2222-2222-2222-222222222222": "beta/w2:p2",
	} {
		got, ok := targetForAgentSession(tree(), nil, id)
		if !ok || got != want {
			t.Fatalf("id %s resolved to (%q, %v), want %q", id, got, ok, want)
		}
	}
}

// THE ONE THAT MATTERS. A share pinned to one session must not read another
// session's conversation, even when the caller knows its id — the id is a
// NAME, not authority, and treating it as authority would be a scope bypass
// that hands over strictly more than the share was ever allowed to show.
func TestAScopedInstanceRefusesAnotherSessionsAgentSession(t *testing.T) {
	scope, err := core.ParseScope("alpha", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Active() {
		t.Fatal("test scope is not active; the assertion below would prove nothing")
	}

	// Its own session: allowed.
	if got, ok := targetForAgentSession(tree(), scope, "11111111-1111-1111-1111-111111111111"); !ok || got != "alpha/w1:p1" {
		t.Fatalf("a scoped instance could not read its OWN agent: (%q, %v)", got, ok)
	}
	// The other session: refused, and refused the same way an unknown id is,
	// so the answer is not an oracle for which sessions exist.
	if got, ok := targetForAgentSession(tree(), scope, "22222222-2222-2222-2222-222222222222"); ok {
		t.Fatalf("SCOPE BYPASS: a share pinned to alpha resolved beta's agent to %q", got)
	}
}

// A pane-scoped share is narrower still: the right session is not enough.
func TestAPaneScopedInstanceRefusesASiblingPane(t *testing.T) {
	// Pinned to a pane that is NOT the agent pane in that session.
	scope, err := core.ParseScope("alpha", []string{"alpha/w9:p9"})
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Active() {
		t.Fatal("test scope is not active")
	}
	if got, ok := targetForAgentSession(tree(), scope, "11111111-1111-1111-1111-111111111111"); ok {
		t.Fatalf("a share pinned to alpha/w9:p9 resolved a sibling pane's agent to %q", got)
	}
}

// An unknown id, an empty id and a nil tree all answer the same: not found.
func TestUnknownIDsAreNotFound(t *testing.T) {
	for _, id := range []string{"", "99999999-9999-9999-9999-999999999999", "../../etc/passwd"} {
		if _, ok := targetForAgentSession(tree(), nil, id); ok {
			t.Fatalf("id %q resolved, and should not have", id)
		}
	}
	if _, ok := targetForAgentSession(nil, nil, "11111111-1111-1111-1111-111111111111"); ok {
		t.Fatal("a nil tree resolved an id")
	}
}

// A plain shell pane has no agent session, so nothing can address it here.
func TestAPlainPaneIsNotAddressable(t *testing.T) {
	if _, ok := targetForAgentSession(tree(), nil, "w9:p9"); ok {
		t.Fatal("a pane id was accepted as an agent session id")
	}
}
