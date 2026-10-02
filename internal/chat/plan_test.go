package chat

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/config"
)

func stubLookPath(t *testing.T, found ...string) {
	t.Helper()
	old := execLookPath
	execLookPath = func(name string) (string, error) {
		for _, f := range found {
			if f == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { execLookPath = old })
}

// Planning must not start anything: the web UI asks "what is configured" every
// time somebody opens a page, and that must not spawn five chat bots.
func TestPlanListsEveryAdapterIncludingTheOffOnes(t *testing.T) {
	stubLookPath(t, "node")
	c := config.Chat{Enabled: false, Adapters: []config.ChatAdapter{
		{ID: "telegram", Command: "node telegram.js", Enabled: false},
		{ID: "discord", Command: "node discord.js", Enabled: true},
	}}
	sum := Plan(c, "/tmp/adapters", nil)

	if sum.Enabled {
		t.Error("summary says enabled; the table is off")
	}
	if len(sum.Adapters) != 2 {
		t.Fatalf("listed %d adapters, want 2 -- an adapter you cannot see is one you cannot turn on", len(sum.Adapters))
	}
	// Both are off: discord's own switch is on but the table's is not, and
	// reporting that as running would be a lie.
	for _, a := range sum.Adapters {
		if a.State != StateOff {
			t.Errorf("%s state = %q, want %q", a.ID, a.State, StateOff)
		}
	}
	if sum.NeedsAttention() {
		t.Error("an off adapter needs attention; it does not")
	}
}

// An unset env reference is reported before anything is spawned, and the
// REPORT names the variable, never a value.
func TestPlanReportsUnsetEnvAsMisconfigured(t *testing.T) {
	stubLookPath(t, "node")
	c := config.Chat{Enabled: true, Adapters: []config.ChatAdapter{{
		ID: "telegram", Command: "node telegram.js", Enabled: true,
		Env: map[string]string{"token": "$HX_NOT_SET_ANYWHERE"},
	}}}
	sum := Plan(c, "/tmp/adapters", nil)

	a := sum.Adapters[0]
	if a.State != StateMisconfigured {
		t.Fatalf("state = %q, want %q", a.State, StateMisconfigured)
	}
	if len(a.Problems) == 0 || !strings.Contains(strings.Join(a.Problems, " "), "HX_NOT_SET_ANYWHERE") {
		t.Fatalf("problems = %v, want the unset variable named", a.Problems)
	}
	if !sum.NeedsAttention() {
		t.Error("a misconfigured adapter does not need attention")
	}
}

// Misconfigured outranks off: an adapter that is switched off AND broken must
// say it is broken, or turning the switch on is the only way to find out it
// never could have worked.
func TestMisconfiguredOutranksOff(t *testing.T) {
	stubLookPath(t) // nothing installed
	c := config.Chat{Enabled: false, Adapters: []config.ChatAdapter{{
		ID: "telegram", Command: "bun telegram.ts", Enabled: false,
	}}}
	a := Plan(c, "/tmp/adapters", nil).Adapters[0]
	if a.State != StateMisconfigured {
		t.Fatalf("state = %q, want %q", a.State, StateMisconfigured)
	}
	if !strings.Contains(strings.Join(a.Problems, " "), "bun") {
		t.Errorf("problems = %v, want bun named as missing", a.Problems)
	}
}

// A live supervisor's numbers win: only it knows the process is actually up.
func TestLiveStatusWinsOverInference(t *testing.T) {
	stubLookPath(t, "node")
	c := config.Chat{Enabled: true, Adapters: []config.ChatAdapter{{
		ID: "telegram", Command: "node telegram.js", Enabled: true,
	}}}
	live := map[string]Status{"telegram": {
		ID: "telegram", State: StateDown, Restarts: 5, LastExit: "exit status 1",
	}}
	a := Plan(c, "/tmp/adapters", live).Adapters[0]

	if a.State != StateDown {
		t.Fatalf("state = %q, want the live %q", a.State, StateDown)
	}
	if a.Restarts != 5 || a.MaxRestarts != MaxRestarts {
		t.Errorf("restarts = %d of %d, want 5 of %d", a.Restarts, a.MaxRestarts, MaxRestarts)
	}
	// down must be distinguishable from off: both are "not running", and
	// conflating them hides a broken adapter behind a switch somebody thinks
	// they turned off on purpose.
	if !a.NeedsAttention() {
		t.Error("a down adapter does not need attention")
	}
}

// A settings screen must be able to show what an adapter is configured with,
// and must never be able to show what it resolved TO. The whole point of
// writing `token = "$TOKEN"` is that the value reaches the adapter's process
// and not a web page served to whoever paired a phone.
func TestEnvEntriesNeverCarryAResolvedValue(t *testing.T) {
	t.Setenv("HX_SECRET", "the-actual-secret-value")
	stubLookPath(t, "node")

	c := config.Chat{Enabled: true, Adapters: []config.ChatAdapter{{
		ID: "telegram", Command: "node telegram.js", Enabled: true,
		Env: map[string]string{
			"HX_SECRET": "$HX_SECRET",
			"chat_id":   "5656629735",
			"missing":   "$HX_NOT_SET",
		},
	}}}
	a := Plan(c, "/tmp/adapters", nil).Adapters[0]

	blob, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	// The decisive assertion: the secret's VALUE must not appear anywhere in
	// the payload a client receives -- not in env, not in problems, not in a
	// command echo.
	if strings.Contains(string(blob), "the-actual-secret-value") {
		t.Fatalf("the resolved secret reached the wire: %s", blob)
	}

	by := map[string]EnvEntry{}
	for _, e := range a.Env {
		by[e.Key] = e
	}
	if got := by["HX_SECRET"]; !got.Reference || !got.Resolved || got.Literal != "$HX_SECRET" {
		t.Errorf("HX_SECRET = %+v, want the reference as written, resolved", got)
	}
	if got := by["chat_id"]; got.Reference || !got.Resolved || got.Literal != "5656629735" {
		t.Errorf("chat_id = %+v, want a resolved non-reference literal", got)
	}
	if got := by["missing"]; !got.Reference || got.Resolved {
		t.Errorf("missing = %+v, want an unresolved reference", got)
	}
}
