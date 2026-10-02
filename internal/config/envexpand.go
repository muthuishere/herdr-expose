package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// envRefRe matches $NAME and ${NAME}, and $$ as an escape for a literal $.
//
// A bare $ followed by anything else is left alone: a chat_id of "$" or a
// message template containing "costs $5" must survive a config file unharmed,
// so this expands references and not every dollar sign it can find.
var envRefRe = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// ExpandEnv resolves $NAME and ${NAME} in a config value against the process
// environment, and reports which referenced names were not set.
//
// This is how a credential reaches an adapter without being written down. The
// config file holds `"$HERDR_EXPOSE_TELEGRAM_TOKEN"` -- a NAME, which is not
// secret and can be committed, read aloud or pasted into an issue -- and the
// value is fetched at the moment it is used. The alternative, a token typed
// into config.toml, is a plaintext credential in a file that gets backed up,
// synced and screenshotted.
//
// A missing name expands to EMPTY and is reported rather than silently
// dropped. Both halves matter: expanding to the literal "$TOKEN" would send
// that string to an API as if it were a credential and produce a baffling
// 401, while expanding to empty without saying so produces an adapter that
// does nothing for no visible reason.
//
// found lists the names that resolved, so a caller can register their values
// with a redactor. It is names, never values: this function's own return is
// the only place a value appears.
func ExpandEnv(value string) (expanded string, found []string, missing []string) {
	seenFound := map[string]bool{}
	seenMissing := map[string]bool{}

	expanded = envRefRe.ReplaceAllStringFunc(value, func(m string) string {
		if m == "$$" {
			return "$"
		}
		name := strings.Trim(m, "${}")
		v, ok := os.LookupEnv(name)
		if !ok {
			seenMissing[name] = true
			return ""
		}
		seenFound[name] = true
		return v
	})

	for n := range seenFound {
		found = append(found, n)
	}
	for n := range seenMissing {
		missing = append(missing, n)
	}
	sort.Strings(found)
	sort.Strings(missing)
	return expanded, found, missing
}

// Resolved is one adapter's config after expansion, plus the bookkeeping a
// caller needs to redact and to explain itself.
type Resolved struct {
	// Values is what the adapter sees as ctx.config.
	Values map[string]string
	// Secrets are the expanded values that came from the environment. They are
	// handed to the redactor so they cannot reach a log line -- including the
	// log line that would otherwise announce them.
	Secrets []string
	// Missing maps a config key to the env var names it referenced that are
	// not set, so an adapter that will not work says why BEFORE it runs.
	Missing map[string][]string
}

// ResolveEnv expands every value in an adapter's env table.
func (a ChatAdapter) ResolveEnv() Resolved {
	r := Resolved{Values: map[string]string{}, Missing: map[string][]string{}}
	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, found, missing := ExpandEnv(a.Env[k])
		r.Values[k] = v
		if len(missing) > 0 {
			r.Missing[k] = missing
		}
		// Only a value that CAME FROM the environment is treated as secret. A
		// literal in the config file is not one: the author wrote it there in
		// the open, and redacting it would scrub ordinary words like a chat id
		// out of every log line that happened to contain them.
		if len(found) > 0 && strings.TrimSpace(v) != "" {
			r.Secrets = append(r.Secrets, v)
		}
	}
	return r
}

// MissingReport renders Missing as one line per key, for a CLI that has to
// explain why an adapter is configured but cannot run.
func (r Resolved) MissingReport() []string {
	keys := make([]string, 0, len(r.Missing))
	for k := range r.Missing {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s references unset %s", k, strings.Join(r.Missing[k], ", ")))
	}
	return out
}
