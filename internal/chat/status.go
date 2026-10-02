package chat

import "time"

// Adapter states, as a client renders them. Strings, not an enum, because they
// go on the wire and a client must be able to show one it has never heard of
// rather than crash on it.
const (
	// StateOff is configured but switched off -- either [chat].enabled is
	// false or this adapter's own enabled is. It is the SHIPPED state.
	StateOff = "off"
	// StateRunning is up and speaking the protocol.
	StateRunning = "running"
	// StateRestarting exited and is inside its backoff.
	StateRestarting = "restarting"
	// StateDown is the give-up: it failed MaxRestarts times and nothing
	// further will happen until a person acts. This is the one state that
	// needs a human, so it must be visibly different from "off" -- both are
	// "not running", and conflating them hides a broken adapter behind a
	// switch somebody thinks they turned off on purpose.
	StateDown = "down"
	// StateMisconfigured cannot run as written: no command, or its config
	// references an environment variable that is not set. Reported BEFORE
	// anything is spawned, because "it referenced $TELEGRAM_TOKEN and that is
	// not set" is an answer and "it keeps dying" is not.
	StateMisconfigured = "misconfigured"
)

// Status is one adapter, as the web UI and the CLI both see it. There is one
// shape so the two cannot disagree about what "enabled" means.
type Status struct {
	ID    string `json:"id"`
	State string `json:"state"`

	// Enabled is this adapter's own switch; TableEnabled is [chat].enabled.
	// Both are reported because "off" has two causes and the fix differs.
	Enabled      bool `json:"enabled"`
	TableEnabled bool `json:"table_enabled"`

	// Command is the argv as configured. It is shown because an adapter that
	// will not start is usually a command that is not there, and the fastest
	// way to see that is to read it back.
	//
	// It is safe to show: a credential never appears here. Secrets reach an
	// adapter through its ENVIRONMENT, because argv is world-readable via
	// `ps` -- so a token in a command line would already be public to every
	// process on the machine, which is why nothing puts one there.
	Command string `json:"command,omitempty"`

	// Problems are the reasons it cannot run, in a person's words: "token
	// references unset HERDR_EXPOSE_TELEGRAM_TOKEN". Never a secret's value.
	Problems []string `json:"problems,omitempty"`

	// Env is this adapter's configuration, as NAMES AND REFERENCES ONLY.
	//
	// A settings screen has to show what an adapter is configured with, and
	// the one thing it must never show is what the configuration resolved TO:
	// the whole point of writing `token = "$HERDR_EXPOSE_TELEGRAM_TOKEN"` is
	// that the value lives in the environment and reaches the adapter's
	// process, not a web page served to whoever paired a phone.
	//
	// So each entry carries the config key, the literal as WRITTEN (which is
	// a variable name, not a secret), and whether it currently resolves.
	// "token -> $HERDR_EXPOSE_TELEGRAM_TOKEN, set" is everything a person
	// needs to fix it and nothing an onlooker can use.
	Env []EnvEntry `json:"env,omitempty"`

	// Restarts is how many of MaxRestarts have been used, so a client can show
	// "restart 3 of 5" rather than a spinner that means nothing.
	Restarts    int `json:"restarts"`
	MaxRestarts int `json:"max_restarts"`

	// StartedAt is when the current run began, and ABSENT when not running.
	//
	// A pointer because `omitempty` does not omit a zero time.Time: it would
	// put "0001-01-01T00:00:00Z" on the wire for every adapter that is not
	// running, which a client renders as the year 1 or, worse, treats as a
	// real start time and reports an uptime of two thousand years.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// LastExit is why the last run ended, already scrubbed of secrets.
	LastExit string `json:"last_exit,omitempty"`
}

// EnvEntry is one configuration key, safe to render.
//
// There is deliberately no field for the resolved value. Not an omitted one,
// not a masked one -- none: a masked value is still a length and a shape, and
// a field that exists is a field somebody later populates "just for
// debugging".
type EnvEntry struct {
	// Key is the config key, which becomes the child's environment variable.
	Key string `json:"key"`
	// Literal is the value as WRITTEN in config.toml, e.g. "$TELEGRAM_TOKEN"
	// or a plain chat id. A reference is a name and is not secret; a literal
	// somebody typed in the clear is already in a file on disk.
	Literal string `json:"literal"`
	// Reference is true when Literal contains a $NAME, so a UI can show "set"
	// or "not set" rather than implying the literal itself is the value.
	Reference bool `json:"reference,omitempty"`
	// Resolved is whether every referenced name is currently set. Always true
	// for a non-reference.
	Resolved bool `json:"resolved"`
}

// NeedsAttention is true for the states a person has to do something about.
//
// It exists so every client agrees on which ones those are: a dot in the web
// UI and a non-zero exit from the CLI must not be able to drift apart.
func (s Status) NeedsAttention() bool {
	return s.State == StateDown || s.State == StateMisconfigured
}

// Summary is the whole feature's state, which is mostly the answer to "is
// anything running at all".
type Summary struct {
	// Enabled is [chat].enabled.
	Enabled bool `json:"enabled"`
	// Adapters is every CONFIGURED adapter, including the off ones: an
	// adapter you cannot see is one you cannot turn on, and a UI that lists
	// only what is running cannot explain why nothing is.
	Adapters []Status `json:"adapters"`
	// AdaptersDir is where the files live, so the UI can tell somebody where
	// to put a new one without them reading the docs.
	AdaptersDir string `json:"adapters_dir,omitempty"`
}

// NeedsAttention is true when any adapter does.
func (s Summary) NeedsAttention() bool {
	for _, a := range s.Adapters {
		if a.NeedsAttention() {
			return true
		}
	}
	return false
}
