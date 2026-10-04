package chat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// Command resolution has to work the way a person writes a path, on either
// platform. The bug this pins: testing for a leading "/" to decide "absolute"
// is wrong on Windows, where C:\adapters\x.js would be joined onto the
// adapters directory and then reported missing.
func TestLookCommandHandlesBothSeparators(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join(dir, "mine.js")
	if err := os.WriteFile(rel, []byte("// x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A relative path with a forward slash resolves against the adapters dir
	// on every platform -- that is what a person types, Windows included.
	if _, err := lookCommand("./mine.js", dir); err != nil {
		t.Errorf("./mine.js did not resolve against the adapters dir: %v", err)
	}
	// An absolute path is taken as-is rather than joined.
	if _, err := lookCommand(rel, dir); err != nil {
		t.Errorf("an absolute path did not resolve: %v", err)
	}
	// A path that is not there is an error, not a silent pass.
	if _, err := lookCommand("./absent.js", dir); err == nil {
		t.Error("a missing adapter resolved; it must be reported before anything is spawned")
	}
	// A bare name goes to PATH. (Stubbed, so this does not depend on what
	// happens to be installed.)
	stubLookPath(t, "node")
	if _, err := lookCommand("node", dir); err != nil {
		t.Errorf("a bare name did not go to PATH: %v", err)
	}
	if _, err := lookCommand("definitely-not-installed", dir); err == nil {
		t.Error("an uninstalled runtime resolved")
	}
}

// A report that says "set" must say whose environment said so.
//
// The failure this pins happened in the field: the daemon, started by launchd
// without a login shell, logged "HERDR_EXPOSE_TELEGRAM_TOKEN is not set" and
// refused the adapter, while `herdr-expose chat status` -- run from the
// terminal where that variable IS exported -- printed "(set)" for the same
// variable. Both read their own environment correctly; the report was wrong to
// omit which one it had read. So the resolution travels with its source, and
// "set" cannot be rendered without it.
func TestEnvResolutionIsAttributedToTheEnvironmentItWasReadFrom(t *testing.T) {
	t.Setenv("HX_SET_IN_THIS_SHELL", "value")
	stubLookPath(t, "node")
	c := config.Chat{Enabled: true, Adapters: []config.ChatAdapter{{
		ID: "telegram", Command: "node telegram.js", Enabled: true,
		Env: map[string]string{"HX_SET_IN_THIS_SHELL": "$HX_SET_IN_THIS_SHELL"},
	}}}

	// The daemon spelling. Plan reads its OWN environment, so only the daemon
	// -- the process that spawns adapters -- may use it.
	if got := Plan(c, "/tmp/adapters", nil).EnvSource; got != EnvSourceDaemon {
		t.Errorf("Plan env source = %q, want %q", got, EnvSourceDaemon)
	}

	cli := PlanFor(EnvSourceCLI, c, "/tmp/adapters", nil)
	if cli.EnvSource != EnvSourceCLI {
		t.Fatalf("env source = %q, want %q", cli.EnvSource, EnvSourceCLI)
	}

	// A client cannot render the resolution without the attribution: the label
	// itself carries it, so there is no bare "(set)" to print.
	e := cli.Adapters[0].Env[0]
	if !e.Resolved {
		t.Fatalf("env entry = %+v, want resolved in this process", e)
	}
	label := e.ResolvedLabel(cli.EnvSource)
	if label == "(set)" || !strings.Contains(label, "shell") {
		t.Errorf("CLI label = %q, want it attributed to this shell", label)
	}
	if daemonLabel := e.ResolvedLabel(EnvSourceDaemon); !strings.Contains(daemonLabel, "daemon") {
		t.Errorf("daemon label = %q, want it attributed to the daemon", daemonLabel)
	}

	// And when a daemon is up, the CLI's answer is explicitly flagged as not
	// being the daemon's -- the two-components-disagreeing case.
	warn := cli.EnvNote(true)
	if !strings.Contains(warn, "NOT the daemon's") {
		t.Errorf("note with a daemon running = %q, want it to deny speaking for the daemon", warn)
	}
	if quiet := cli.EnvNote(false); strings.Contains(quiet, "daemon is running") {
		t.Errorf("note with no daemon = %q, want no claim that one is running", quiet)
	}

	// The attribution has to survive the wire, or the web UI renders the same
	// unowned "set" the CLI used to.
	blob, err := json.Marshal(cli)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"env_source":"cli"`) {
		t.Fatalf("env source did not reach the wire: %s", blob)
	}
	if strings.Contains(string(blob), "value") {
		t.Fatalf("a resolved value reached the wire: %s", blob)
	}
}

// The CLI is a different process from the daemon: it holds the config and has
// never spoken to a supervisor. It used to answer "restarting", which is a
// claim about a supervisor it cannot see -- and it said that about an adapter
// the daemon had REFUSED outright and written to the log as refused. Not
// knowing is a legitimate answer; guessing sends the reader nowhere.
func TestWithoutASupervisorViewTheStateIsUnknownNotRestarting(t *testing.T) {
	c := config.Chat{Enabled: true, Adapters: []config.ChatAdapter{
		{ID: "telegram", Enabled: true, Command: "node telegram.js"},
	}}
	stubLookPath(t, "node")
	dir := "/tmp/adapters"

	cli := PlanFor(EnvSourceCLI, c, dir, nil)
	if len(cli.Adapters) != 1 {
		t.Fatalf("adapters = %d", len(cli.Adapters))
	}
	if got := cli.Adapters[0].State; got != StateUnknown {
		t.Fatalf("CLI state = %q, want %q", got, StateUnknown)
	}

	// The daemon DOES have a view. A live map that simply lacks this adapter
	// means the supervisor has not reached it yet, which really is restarting.
	daemon := PlanFor(EnvSourceDaemon, c, dir, map[string]Status{})
	if got := daemon.Adapters[0].State; got != StateRestarting {
		t.Fatalf("daemon state = %q, want %q", got, StateRestarting)
	}

	// And a refusal the daemon recorded must survive into the summary.
	refused := PlanFor(EnvSourceDaemon, c, dir, map[string]Status{
		"telegram": {ID: "telegram", State: StateRefused, Problems: []string{"TOKEN is not set"}},
	})
	if got := refused.Adapters[0].State; got != StateRefused {
		t.Fatalf("refused state = %q, want %q", got, StateRefused)
	}
}
