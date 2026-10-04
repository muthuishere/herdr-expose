package chat

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// Plan turns configuration into the Summary a client renders, WITHOUT starting
// anything, attributing the environment it read to the DAEMON.
//
// It is pure except for reading the environment and looking for the command on
// disk, which is the point: the web UI and the CLI both ask "what is
// configured and could it run", and that question must be answerable without
// spawning five chat bots as a side effect of opening a page.
//
// It reads os.Getenv in the CALLING process, so only the daemon may use this
// spelling: the daemon is the process that will spawn the adapters, so its
// environment is the one that decides. Any other process is guessing about the
// daemon's environment and must say so by calling PlanFor with its own source
// -- `herdr-expose chat status` does.
func Plan(c config.Chat, adaptersDir string, running map[string]Status) Summary {
	return PlanFor(EnvSourceDaemon, c, adaptersDir, running)
}

// PlanFor is Plan with the environment attribution spelled out.
//
// envSource travels into Summary.EnvSource and from there into every rendering
// of "set"/"not set", because the resolutions below come from THIS process's
// environment and an unattributed "set" let the CLI imply the daemon could see
// a variable that, running under launchd/systemd without a login shell, it
// could not.
func PlanFor(envSource string, c config.Chat, adaptersDir string, running map[string]Status) Summary {
	sum := Summary{Enabled: c.Enabled, AdaptersDir: adaptersDir, EnvSource: envSource}

	for _, a := range c.Adapters {
		st := Status{
			ID:           strings.TrimSpace(a.ID),
			Enabled:      a.Enabled,
			TableEnabled: c.Enabled,
			Command:      a.Command,
			MaxRestarts:  MaxRestarts,
		}
		if st.ID == "" {
			st.ID = "(unnamed)"
			st.Problems = append(st.Problems,
				"no id, so it cannot be named, inspected or turned off")
		}

		// Configuration problems are reported BEFORE anything is spawned:
		// "it references $TOKEN and that is not set" is an answer, whereas
		// "it keeps dying" is a symptom.
		argv, err := config.SplitCommand(a.Command)
		if err != nil {
			st.Problems = append(st.Problems, err.Error())
		} else if _, lookErr := lookCommand(argv[0], adaptersDir); lookErr != nil {
			st.Problems = append(st.Problems, argv[0]+" is not installed or not on PATH")
		}

		res := a.ResolveEnv()
		st.Problems = append(st.Problems, res.MissingReport()...)
		st.Env = envEntries(a, res)

		// A live supervisor's numbers win over anything inferred: it knows
		// whether the process is actually up.
		if live, ok := running[st.ID]; ok {
			live.Enabled, live.TableEnabled = a.Enabled, c.Enabled
			live.Command, live.MaxRestarts = a.Command, MaxRestarts
			// Deduplicated: a refused adapter already carries the reason the
			// manager refused it, and we have just derived the same sentence
			// from the same config and the same environment. Printing "token
			// is not set" twice reads like two separate faults.
			live.Problems = appendNewProblems(live.Problems, st.Problems...)
			// A supervisor reports process facts and knows nothing about the
			// config table, so the env rows have to come from here or a live
			// (and a refused) adapter renders with no configuration at all --
			// and "refused: TOKEN is not set" with an empty env list is half
			// an answer.
			live.Env = st.Env
			sum.Adapters = append(sum.Adapters, live)
			continue
		}

		switch {
		case len(st.Problems) > 0:
			// Misconfigured outranks off. An adapter that is switched off AND
			// broken should say it is broken, or turning the switch on becomes
			// the only way to discover it never could have worked.
			st.State = StateMisconfigured
		case !c.Enabled || !a.Enabled:
			st.State = StateOff
		case running == nil:
			// No supervisor view AT ALL: this is the CLI, holding the config
			// and nothing else. A missing ENTRY in a live map means the
			// supervisor has not got to this adapter yet, which is genuinely
			// "restarting"; a missing MAP means we never asked anyone.
			st.State = StateUnknown
		default:
			// Enabled, runnable, and no supervisor has reported in yet.
			st.State = StateRestarting
		}
		sum.Adapters = append(sum.Adapters, st)
	}
	return sum
}

// appendNewProblems appends the problems that are not already reported, so one
// fault seen by both the manager and Plan is one line.
func appendNewProblems(have []string, more ...string) []string {
	seen := make(map[string]bool, len(have))
	for _, p := range have {
		seen[p] = true
	}
	for _, p := range more {
		if !seen[p] {
			have = append(have, p)
			seen[p] = true
		}
	}
	return have
}

// lookCommand finds argv[0] the way the supervisor will: an explicit path is
// resolved against the adapters directory (because that is the child's cwd),
// and a bare name is looked up on PATH.
func lookCommand(name, dir string) (string, error) {
	// A PATH lookup, or a path resolved against the adapters directory (which
	// is the child's working directory).
	//
	// Both separators count, because a config file is written by a person and
	// "./adapters/x.js" is what a person types on Windows too. filepath.IsAbs
	// rather than a leading "/" test: on Windows an absolute path is C:\... ,
	// so the slash test would quietly join it onto the adapters directory and
	// then report the result missing.
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		p := name
		if !filepath.IsAbs(name) {
			p = filepath.Join(dir, name)
		}
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return p, nil
	}
	// A bare name goes to PATH, where LookPath applies PATHEXT on Windows --
	// which is what makes `node` find node.exe.
	return execLookPath(name)
}

// envEntries renders an adapter's config for display: keys and the literals as
// WRITTEN, never a resolved value. See chat.EnvEntry for why there is no field
// for one.
func envEntries(a config.ChatAdapter, res config.Resolved) []EnvEntry {
	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]EnvEntry, 0, len(keys))
	for _, k := range keys {
		literal := a.Env[k]
		out = append(out, EnvEntry{
			Key:       k,
			Literal:   literal,
			Reference: strings.Contains(literal, "$"),
			Resolved:  len(res.Missing[k]) == 0,
		})
	}
	return out
}
