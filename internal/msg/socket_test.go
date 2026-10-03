package msg

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

type fakeCaller struct {
	calls []string
	err   error
}

func (f *fakeCaller) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	f.calls = append(f.calls, method)
	return nil, f.err
}

func (f *fakeCaller) CallInto(_ context.Context, method string, _ any, out any) error {
	f.calls = append(f.calls, method)
	if f.err != nil {
		return f.err
	}
	if r, ok := out.(*struct {
		Snapshot upstream.Snapshot `json:"snapshot"`
	}); ok {
		r.Snapshot = upstream.Snapshot{
			Panes:  []upstream.Pane{{PaneID: "w1:p1", Label: "ops"}},
			Agents: []upstream.Agent{{PaneID: "w1:p1", Agent: "claude", AgentStatus: "idle"}},
		}
	}
	return nil
}

// Delivery must reach the pane WITHOUT attaching to it.
//
// The CLI path started a herdr client, which attaches to the terminal session
// at 120x40; on a pane of any other size that SIGWINCHes it, the agent's TUI
// throws its screen away and redraws, and whoever is reading watches it jump.
// This pins the transport, because the symptom appears on someone else's
// screen and never in a test's output.
func TestPromptGoesOverTheSocketAndNeverSpawnsAnything(t *testing.T) {
	f := &fakeCaller{}
	s := Socket{Client: func(string) Caller { return f }}

	code, err := s.Prompt(context.Background(), "work", "w1:p1", "hello")
	if err != nil || code != "" {
		t.Fatalf("Prompt = %q, %v; want no error", code, err)
	}
	if len(f.calls) != 1 || f.calls[0] != "agent.prompt" {
		t.Fatalf("calls = %v, want exactly [agent.prompt]", f.calls)
	}
}

// Herdr's refusal codes are what the service routes on, so they must survive
// the move from the CLI's stderr JSON to a typed socket error. Losing one
// turns "that agent is on a permission dialog" into a generic failure, and the
// request is retried instead of reported as blocked.
func TestPromptPreservesHerdrsRefusalCode(t *testing.T) {
	f := &fakeCaller{err: &upstream.RPCError{Code: "agent_blocked", Message: "on a dialog"}}
	s := Socket{Client: func(string) Caller { return f }}

	code, err := s.Prompt(context.Background(), "work", "w1:p1", "hello")
	if code != "agent_blocked" {
		t.Fatalf("code = %q, want agent_blocked", code)
	}
	if err == nil {
		t.Fatal("want an error alongside the code")
	}
}

// An unknown session is reported, not silently dropped: a message addressed to
// a session that is gone must fail loudly rather than sit queued forever.
func TestPromptToAnUnknownSessionIsReported(t *testing.T) {
	s := Socket{Client: func(string) Caller { return nil }}
	code, err := s.Prompt(context.Background(), "gone", "w1:p1", "hello")
	if code != "agent_not_found" || err == nil {
		t.Fatalf("code = %q, err = %v; want agent_not_found and an error", code, err)
	}
}

// Listing agents uses session.snapshot -- the same call the daemon already
// makes for its tree -- so it costs nothing extra and, unlike `herdr agent
// list`, touches no pane. The listing is what ran every two seconds for as
// long as a message awaited a reply, so this is the call that was resizing
// somebody's pane on a timer.
func TestAgentsListsFromTheSnapshotAndNamesByLabel(t *testing.T) {
	f := &fakeCaller{}
	s := Socket{
		Client: func(string) Caller { return f },
		Sessions: func(context.Context) ([]upstream.Session, error) {
			return []upstream.Session{{Name: "work"}}, nil
		},
	}
	got, err := s.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != "session.snapshot" {
		t.Fatalf("calls = %v, want exactly [session.snapshot]", f.calls)
	}
	if len(got) != 1 {
		t.Fatalf("got %d agents, want 1", len(got))
	}
	// A pane's label lives on the PANE, not the agent, so an agent is
	// addressable by the name a person gave it only if the lookup crosses
	// over -- otherwise every agent is addressable solely by pane id.
	if got[0].Address != "work/ops" || got[0].Name != "ops" || got[0].PaneID != "w1:p1" {
		t.Fatalf("agent = %+v, want it addressed by its label", got[0])
	}
}

// One dead session must not hide the others: a listing that aborts on the
// first failure makes every agent unreachable because one pane went away.
func TestOneDeadSessionDoesNotHideTheRest(t *testing.T) {
	bad := &fakeCaller{err: errors.New("socket gone")}
	good := &fakeCaller{}
	s := Socket{
		Client: func(name string) Caller {
			if name == "dead" {
				return bad
			}
			return good
		},
		Sessions: func(context.Context) ([]upstream.Session, error) {
			return []upstream.Session{{Name: "dead"}, {Name: "work"}}, nil
		},
	}
	got, err := s.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Session != "work" {
		t.Fatalf("got %+v, want the live session's agent", got)
	}
}
