package expose

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ngrok — the second FIRST-CLASS provider, not a thinner one.
//
// Everything the Cloudflare providers guarantee, this file guarantees, because
// the owner's rule is "cloudflared or ngrok, whatever they want" and a ladder
// whose rungs mean different things depending on the provider is not a ladder:
//
//   - TWO rungs, matching cloudflare's two. A RESERVED domain is the analogue
//     of a named tunnel (stable hostname, C4's static-domain rule), and an
//     EPHEMERAL tunnel is the analogue of `--quick` (random hostname, nothing
//     reserved, nothing to clean up). `share --quick --provider ngrok` and
//     `share --domain X --provider ngrok` land on exactly the rungs their
//     cloudflare spellings do.
//   - VERIFY BEFORE PUBLISH. The URL is published only once it has answered
//     through the public hostname, via the same procTunnel machinery. ngrok's
//     agent prints a URL the moment it registers, which is not the moment it
//     routes, so this matters here for the same reason it matters there.
//   - SUPERVISION AND RESTART with the same backoff, and — for the ephemeral
//     rung — the same re-verification of a NEW hostname after a restart,
//     because an ephemeral ngrok tunnel does not come back on the same name.
//   - THE TOKEN IS READ BY NAME AT POINT OF USE. $NGROK_AUTHTOKEN is read
//     inside Build, for one child process's environment, registered with the
//     redactor, and never stored in config, never written to state, never
//     logged, never returned in a Status and never put in an error message.
//   - TEARDOWN ONLY REMOVES WHAT IT CREATED, which for ngrok is NOTHING: a
//     reserved domain is provisioned in the ngrok dashboard by the user and is
//     not ours to delete, and an ephemeral tunnel dies with the process. The
//     Footprint says so out loud instead of leaving Destroy looking suspiciously
//     empty.

// NgrokTokenEnv is the NAME of the env var carrying the ngrok authtoken. As
// with Cloudflare, the value is read at point of use, handed to the child
// process's environment, and never stored or logged.
const NgrokTokenEnv = "NGROK_AUTHTOKEN"

// NgrokOptions configures the built-in ngrok provider.
type NgrokOptions struct {
	Port int
	// Domain is a RESERVED domain: the analogue of a Cloudflare named tunnel,
	// and required for the domain rung, since random hostnames change on every
	// restart (C4).
	Domain string
	// Ephemeral selects the analogue of `--quick`: a throwaway hostname
	// assigned by ngrok's edge, nothing reserved, nothing to tear down.
	Ephemeral bool
	Bin       string
}

var ngrokURLRe = regexp.MustCompile(`https://[a-zA-Z0-9][a-zA-Z0-9.-]*\.(?:ngrok-free\.app|ngrok\.app|ngrok\.io|ngrok\.dev)`)

// ngrokFootprint declares what ngrok creates: nothing we may delete.
func ngrokFootprint(opts NgrokOptions) Footprint {
	f := Footprint{
		Provider:      "ngrok",
		RemoteAccount: true,
		SecretEnv:     []string{NgrokTokenEnv},
	}
	if opts.Ephemeral {
		f.Provider = "ngrok-quick"
		f.Ephemeral = true
		return f
	}
	// A reserved domain is USED, never created — so it is never deleted.
	f.ReservedName = strings.TrimSpace(opts.Domain)
	return f
}

// ngrokAuthConfigured reports whether the agent has SOME way to authenticate,
// without reading, returning or logging any value. It only ever answers a
// yes/no question about presence.
func ngrokAuthConfigured() bool {
	if _, ok := os.LookupEnv(NgrokTokenEnv); ok {
		return strings.TrimSpace(os.Getenv(NgrokTokenEnv)) != ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, p := range []string{
		filepath.Join(home, ".config", "ngrok", "ngrok.yml"),
		filepath.Join(home, ".ngrok2", "ngrok.yml"),
		filepath.Join(home, "Library", "Application Support", "ngrok", "ngrok.yml"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

func newNgrok(opts NgrokOptions, logf func(string, ...any), red *redactor) (Provider, error) {
	// The ephemeral rung is an EXPLICIT request, never what you get by leaving
	// the domain blank. That asymmetry is the whole of C4 on this provider:
	// a permanent deployment configured `ngrok = true` with no domain is a
	// mistake to be reported, not a licence to hand back a hostname that
	// changes on every restart. `--quick` sets Ephemeral; nothing else does.
	ephemeral := opts.Ephemeral
	if !ephemeral && strings.TrimSpace(opts.Domain) == "" {
		return nil, fmt.Errorf("expose.domain is required for ngrok: reserve a domain in the ngrok dashboard " +
			"and set it (random ngrok hostnames change on every restart and are not a stable endpoint). " +
			"For a throwaway hostname, ask for the ephemeral rung explicitly: `share --quick --provider ngrok`")
	}
	bin, err := resolveBinary(opts.Bin, "ngrok")
	if err != nil {
		return nil, fmt.Errorf("%w\ninstall it with `brew install ngrok` or from https://ngrok.com/download", err)
	}
	if !ngrokAuthConfigured() {
		// Named, never valued. This is the whole error a user gets, and it
		// contains no credential.
		return nil, fmt.Errorf("ngrok has no authtoken: set $%s (read at point of use, never stored or logged) "+
			"or run `ngrok config add-authtoken <token>` once", NgrokTokenEnv)
	}

	static := ""
	if !ephemeral {
		static = "https://" + strings.TrimSpace(opts.Domain)
	}

	build := func(ctx context.Context) (*exec.Cmd, error) {
		args := []string{"http", strconv.Itoa(opts.Port),
			"--log", "stdout", "--log-format", "logfmt", "--log-level", "info"}
		if !ephemeral {
			args = append(args, "--domain", strings.TrimSpace(opts.Domain))
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		env := minimalEnv()
		// POINT OF USE. The value exists here, in this child's environment,
		// and nowhere else: not in the config file, not in state, not in the
		// log, not in Status, not in an error.
		if tok := strings.TrimSpace(os.Getenv(NgrokTokenEnv)); tok != "" {
			red.add(tok)
			env = append(env, NgrokTokenEnv+"="+tok)
		}
		cmd.Env = env
		return cmd, nil
	}

	spec := procSpec{
		Name:  "ngrok",
		Mode:  "ngrok",
		Bin:   bin,
		Build: build,
		// Verify-before-publish, both rungs. With a reserved domain the
		// hostname is known up front, so it is probed directly and no scanner
		// is wired: scraping a hostname we already know only adds a way to
		// publish a DIFFERENT one. On the ephemeral rung the hostname is
		// assigned at start, so the scanner supplies it and a restart's new
		// hostname is re-verified before it replaces the dead one.
		Verify:         true,
		VerifyTimeout:  90 * time.Second,
		Log:            logf,
		Redact:         red,
		HealthInterval: 30 * time.Second,
		Print:          ngrokFootprint(opts),
		// Teardown is nil ON PURPOSE, and the Footprint above states why: a
		// reserved domain belongs to the user's ngrok account and was not
		// created here, and an ephemeral tunnel is gone the moment the process
		// is. Deleting either would be this tool removing something it did not
		// create.
		Teardown: nil,
	}
	if ephemeral {
		spec.Name = "ngrok-quick"
		spec.Mode = string(ModeQuick)
		spec.ScanURL = func(line string) string { return ngrokURLRe.FindString(line) }
	} else {
		spec.StaticURL = static
	}
	return newProcTunnel(spec), nil
}
