package expose

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

// TryCloudflare — the ephemeral quick tunnel (SPEC AMENDMENTS 15).
//
// `cloudflared tunnel --url http://127.0.0.1:<port>` asks Cloudflare's edge for
// a throwaway hostname and serves the loopback port on it. What makes it worth
// having back is everything it does NOT do:
//
//   - no Cloudflare account, so no API token is read here. newCFAPI is never
//     reached from this file: no zone lookup, no DNS call, no credentials file,
//     no named tunnel. That is the feature, not an optimisation — it is what
//     lets someone with no zone of their own run `herdr-expose share`.
//   - no DNS record and no named tunnel exist afterwards, so teardown is
//     "stop the process", and there is nothing left in anybody's account to
//     revoke, orphan or pay for.
//
// The price is stated once, at create time, in the CLI: the hostname is new
// every run, and device tokens are origin-bound, so a paired phone must pair
// again on the next quick share. That is fine for a disposable share and
// unacceptable for the daily driver, which is exactly why the permanent
// deployment still requires a static domain (C1) and cannot reach this code.

// quickURLRe matches the hostname cloudflared prints once the edge has assigned
// one. It is logged inside an ASCII box on stderr, so the match is on the URL
// itself rather than on any surrounding framing, which cloudflared changes.
var quickURLRe = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*(?:-[a-z0-9]+)*\.trycloudflare\.com`)

// newQuickTunnel builds a supervised TryCloudflare quick tunnel.
//
// Only Port and Bin are read from opts; a quick tunnel has no domain, no tunnel
// name, no state dir and no DNS tag, and passing one would be a lie.
func newQuickTunnel(opts CloudflareOptions, logf func(string, ...any), red *redactor) (Provider, error) {
	bin, err := resolveBinary(opts.Bin, "cloudflared")
	if err != nil {
		return nil, fmt.Errorf("%w\ninstall it with `brew install cloudflared` (macOS) or from "+
			"https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/", err)
	}

	build := func(ctx context.Context) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, bin, "tunnel", "--no-autoupdate", "--url", opts.service())
		// minimalEnv carries no credentials, and unlike the named-tunnel path
		// nothing is appended to it: a quick tunnel authenticates with nothing.
		cmd.Env = minimalEnv()
		return cmd, nil
	}

	return newProcTunnel(procSpec{
		Name:    "cloudflare-quick",
		Mode:    string(ModeQuick),
		Bin:     bin,
		Build:   build,
		ScanURL: func(line string) string { return quickURLRe.FindString(line) },
		// Verify with no StaticURL means "probe the hostname you scraped, and
		// publish it only once /healthz answers THROUGH it" — the same
		// verify-before-publish rule as the named tunnel, including the
		// public-DNS resolver in probe(): the host stub resolver has never
		// heard of a name that was minted three seconds ago.
		Verify:         true,
		VerifyTimeout:  90 * time.Second,
		Log:            logf,
		Redact:         red,
		HealthInterval: 30 * time.Second,
		Print:          quickFootprint(),
		// No Teardown, and the Footprint says why: there is no account
		// resource, no DNS record and no named tunnel, so "stop the process"
		// IS the complete teardown. An empty Destroy here is the truth, not an
		// omission.
		Teardown: nil,
	}), nil
}

// quickFootprint declares the whole of what a TryCloudflare tunnel creates.
func quickFootprint() Footprint {
	return Footprint{Provider: "cloudflare-quick", Ephemeral: true}
}

// QuickURLPattern is the argument shape a quick cloudflared is started with,
// derived from the port. Teardown matches stray processes on THIS string
// rather than on the binary's name, so it can never reach the permanent
// deployment's cloudflared (which runs with --config, not --url).
func QuickURLPattern(port int) string {
	return fmt.Sprintf("--url http://127.0.0.1:%d", port)
}
