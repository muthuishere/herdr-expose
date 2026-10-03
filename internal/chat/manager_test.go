package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// A refusal the manager keeps to itself becomes a lie in the UI.
//
// The failure this pins: Start logged "HERDR_EXPOSE_TELEGRAM_TOKEN is not set"
// and skipped the adapter WITHOUT registering anything, so Plan -- finding
// nothing live for an adapter that is enabled and whose command is on disk --
// fell through to StateRestarting. The web UI and `chat status` then both
// claimed the adapter was starting, when the daemon had decided against it and
// would not try again until it was restarted. The giveaway is exactly this
// sequence: the person exports the variable, so Plan can now resolve it and
// reports no problem at all, while the daemon is still refusing.
func TestRefusedAdapterReportsRefusedNotRestarting(t *testing.T) {
	stubLookPath(t, "node")
	const name = "HX_REFUSED_TOKEN_NOT_SET_AT_START"
	c := config.Chat{Enabled: true, Adapters: []config.ChatAdapter{{
		ID: "telegram", Command: "node telegram.js", Enabled: true,
		Env: map[string]string{name: "$" + name},
	}}}

	// Start with the variable unset: the manager refuses rather than spawn
	// something that would fail five times and be retired.
	m := NewManager(nil, nil, t.TempDir(), nil)
	m.Start(context.Background(), c)

	live := m.Live()
	st, ok := live["telegram"]
	if !ok {
		t.Fatal("the refused adapter is absent from Live(); Plan then has to guess, and it guessed restarting")
	}
	if st.State != StateRefused {
		t.Fatalf("state = %q, want %q", st.State, StateRefused)
	}

	// The variable is exported afterwards, which is what a person does on
	// being told. It does not un-refuse the daemon, and the report must not
	// pretend otherwise.
	t.Setenv(name, "a-token-value")
	a := Plan(c, "/tmp/adapters", live).Adapters[0]

	if a.State == StateRestarting {
		t.Fatal("a refused adapter reported restarting; nothing is going to happen next")
	}
	if a.State != StateRefused {
		t.Fatalf("state = %q, want %q", a.State, StateRefused)
	}
	if !strings.Contains(strings.Join(a.Problems, " "), name) {
		t.Errorf("problems = %v, want the refusal reason naming %s", a.Problems, name)
	}
	if !a.NeedsAttention() {
		t.Error("a refused adapter does not need attention; it is the one state nobody is coming for")
	}

	// The reason is a NAME. A refusal that leaked the value it was handed
	// later would be worse than the lie it replaced.
	if strings.Contains(strings.Join(a.Problems, " "), "a-token-value") {
		t.Fatalf("the refusal reason carried a value: %v", a.Problems)
	}
}
