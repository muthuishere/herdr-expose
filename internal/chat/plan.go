package chat

import (
	"os"
	"sort"
	"strings"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// Plan turns configuration into the Summary a client renders, WITHOUT starting
// anything.
//
// It is pure except for reading the environment and looking for the command on
// disk, which is the point: the web UI and the CLI both ask "what is
// configured and could it run", and that question must be answerable without
// spawning five chat bots as a side effect of opening a page.
func Plan(c config.Chat, adaptersDir string, running map[string]Status) Summary {
	sum := Summary{Enabled: c.Enabled, AdaptersDir: adaptersDir}

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
			live.Problems = append(live.Problems, st.Problems...)
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
		default:
			// Enabled, runnable, and no supervisor has reported in yet.
			st.State = StateRestarting
		}
		sum.Adapters = append(sum.Adapters, st)
	}
	return sum
}

// lookCommand finds argv[0] the way the supervisor will: an explicit path is
// resolved against the adapters directory (because that is the child's cwd),
// and a bare name is looked up on PATH.
func lookCommand(name, dir string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		p := name
		if !strings.HasPrefix(name, "/") {
			p = dir + string(os.PathSeparator) + name
		}
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return p, nil
	}
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
