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
		Agents []upstream.Agent `json:"agents"`
	}); ok {
		r.Agents = []upstream.Agent{{
			PaneID: "w1:p1", Agent: "claude", AgentStatus: "idle",
			Name: "ops", Cwd: "/src", ForegroundCwd: "/src/sub",
		}, {
			PaneID: "w1:p2", Agent: "claude", AgentStatus: "idle",
		}}
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

// Listing agents uses agent.list, which carries the agent's NAME and cwd.
//
// session.snapshot does not: its agents have no name and its panes have no
// label on 0.9.x, so a lister built on it addressed every agent as
// `session/wR:p1` with an empty cwd. Agents on other machines address Mac
// agents by name, so that broke messaging between boxes.
//
// agent.list is still a SOCKET call and so still touches no pane -- the attach
// that moved this package off the CLI belongs to the herdr client, not to the
// call. This listing is what runs every two seconds for as long as a message
// awaits a reply, so it is the one that must never resize anything.
func TestAgentsListsByNameAndCwd(t *testing.T) {
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
	if len(f.calls) != 1 || f.calls[0] != "agent.list" {
		t.Fatalf("calls = %v, want exactly [agent.list]", f.calls)
	}
	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2", len(got))
	}
	// Named: addressed by the name, and the cwd filled in. ForegroundCwd wins
	// because it is where the agent actually is once it has cd'd.
	if got[0].Address != "work/ops" || got[0].Name != "ops" || got[0].PaneID != "w1:p1" {
		t.Fatalf("agent = %+v, want it addressed by its name", got[0])
	}
	if got[0].Cwd != "/src/sub" {
		t.Fatalf("cwd = %q, want the foreground cwd", got[0].Cwd)
	}
	// Unnamed: still addressable, by pane id. Service.find matches either.
	if got[1].Address != "work/w1:p2" || got[1].Name != "" {
		t.Fatalf("unnamed agent = %+v, want it addressed by pane id", got[1])
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
	if len(got) != 2 || got[0].Session != "work" || got[1].Session != "work" {
		t.Fatalf("got %+v, want only the live session's agents", got)
	}
}
