package expose

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NgrokTokenEnv is the NAME of the env var carrying the ngrok authtoken. As
// with Cloudflare, the value is read at point of use, handed to the child
// process's environment, and never stored or logged.
const NgrokTokenEnv = "NGROK_AUTHTOKEN"

// NgrokOptions configures the built-in ngrok provider (`ngrok = true`).
type NgrokOptions struct {
	Port   int
	Domain string // REQUIRED reserved domain; random URLs are not supported (C4)
	Bin    string
}

var ngrokURLRe = regexp.MustCompile(`https://[a-zA-Z0-9][a-zA-Z0-9.-]*\.(?:ngrok-free\.app|ngrok\.app|ngrok\.io|ngrok\.dev)`)

func newNgrok(opts NgrokOptions, logf func(string, ...any), red *redactor) (tunnel, error) {
	if strings.TrimSpace(opts.Domain) == "" {
		return nil, fmt.Errorf("expose.domain is required for ngrok: reserve a domain in the ngrok dashboard " +
			"and set it (random ngrok hostnames change on every restart and are not supported)")
	}
	bin, err := resolveBinary(opts.Bin, "ngrok")
	if err != nil {
		return nil, fmt.Errorf("%w\ninstall it with `brew install ngrok` or from https://ngrok.com/download", err)
	}

	static := "https://" + strings.TrimSpace(opts.Domain)

	build := func(ctx context.Context) (*exec.Cmd, error) {
		args := []string{"http", strconv.Itoa(opts.Port),
			"--log", "stdout", "--log-format", "logfmt", "--log-level", "info",
			"--domain", strings.TrimSpace(opts.Domain)}
		cmd := exec.CommandContext(ctx, bin, args...)
		env := minimalEnv()
		if tok := strings.TrimSpace(os.Getenv(NgrokTokenEnv)); tok != "" {
			red.add(tok)
			env = append(env, NgrokTokenEnv+"="+tok)
		}
		cmd.Env = env
		return cmd, nil
	}

	return newProcTunnel(procSpec{
		Name:           "ngrok",
		Mode:           "ngrok",
		Bin:            bin,
		Build:          build,
		StaticURL:      static,
		Verify:         true,
		VerifyTimeout:  90 * time.Second,
		ScanURL:        func(line string) string { return ngrokURLRe.FindString(line) },
		Log:            logf,
		Redact:         red,
		HealthInterval: 30 * time.Second,
	}), nil
}
