package expose

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
)

// Provider is the ONE interface every exposure implementation satisfies:
// cloudflare-named, cloudflare-quick and a goja/JS adapter. There is no "the
// built-in one plus an escape hatch that gets a different deal" — the Manager
// only ever holds a Provider, and every guarantee in this package is stated on
// this interface rather than on one implementation.
//
// This interface OUTLIVES any particular provider, and deliberately so. The
// built-in ngrok provider was removed in AMENDMENTS 18 / ADR 0034, and the
// interface did not shrink with it: it is not ngrok scaffolding, it is what
// makes teardown symmetric with creation (Footprint), makes idempotency
// testable without a network, and gives a JS adapter — now the answer for
// ngrok, tailscale or anything else — exactly the same contract as the
// built-in. Do not collapse it back into Cloudflare-specific code.
//
// The method set is exactly the lifecycle of an exposure:
//
//	Footprint()   what this provider CREATES, declared before anything runs.
//	              Teardown is generated from it, so it is symmetric with
//	              creation by construction rather than by a matching pair of
//	              hand-written functions that drift.
//	Launch()      start the transport. Returns as soon as the first process
//	              exists; it is NOT a claim that anything works yet.
//	WaitForURL()  verify-before-publish. A URL comes back only once it has
//	              actually answered, for EVERY provider — a started process is
//	              not a working URL, and the Cloudflare edge will happily
//	              serve 502/530 through a tunnel that is not routing.
//	Snapshot()    credential-free status, including supervision state
//	              (running, healthy, restarts, pid).
//	Stop()        idempotent. Twice, or never started, is a no-op returning nil.
//	Destroy()     idempotent teardown of exactly what Footprint declares, and
//	              nothing else. A provider that creates nothing remote (every
//	              ephemeral one) has a Destroy that is honestly a no-op, and
//	              says so through Footprint.Ephemeral rather than by silently
//	              doing nothing.
//
// Supervision and restart are not on the interface because they are not
// optional: procTunnel supervises every process-backed provider, and the JS
// adapter's spawned children are tracked and killed by the host. A provider
// cannot opt out of being restarted, or of being killed.
type Provider interface {
	// Name is the provider identity, e.g. "cloudflare", "cloudflare-quick",
	// "js:<id>".
	Name() string
	// Mode is the transport shape, e.g. "named" | "quick" | "js".
	Mode() string
	// Footprint declares what this provider creates outside this process.
	Footprint() Footprint

	Launch(ctx context.Context) error
	WaitForURL(ctx context.Context, timeout time.Duration) (string, error)
	Snapshot() Status
	Stop() error

	// Destroy removes what Footprint declares and nothing else. It must be
	// safe with no process running, safe to call twice, and safe to call
	// after a crash halfway through creation.
	Destroy(ctx context.Context, logf func(string, ...any)) error
}

// Footprint is a provider's declaration of what it creates in the world — the
// input to teardown, and the answer to "what is left if this process dies right
// now?".
//
// It is DECLARED rather than discovered because the dangerous case is the
// interrupted one: a process killed between "tunnel created" and "DNS written"
// leaves something behind that nothing remembers. The footprint is known before
// the first API call, so teardown can address it whether or not creation
// finished.
type Footprint struct {
	// Provider is the provider name this footprint belongs to.
	Provider string `json:"provider"`
	// Ephemeral means everything this provider creates dies with the process:
	// no account resource, nothing to delete, nothing to orphan, nothing to
	// pay for. Quick tunnels (both flavours) are ephemeral.
	Ephemeral bool `json:"ephemeral"`
	// RemoteAccount is true when the provider touches an account the user
	// owns (a Cloudflare zone). False for anything that needs no account.
	RemoteAccount bool `json:"remote_account"`
	// SecretEnv lists the environment variable NAMES this provider reads at
	// point of use. Names only — a value never appears in a Footprint, a
	// Status, a log line or an error, here or anywhere else in this package.
	SecretEnv []string `json:"secret_env,omitempty"`
	// NamedTunnel is the tunnel this provider creates and therefore deletes.
	NamedTunnel string `json:"named_tunnel,omitempty"`
	// DNSRecord is the hostname whose record it creates, and DNSTag is the
	// comment it writes on that record. Teardown deletes a record ONLY when
	// the live record carries DNSTag, which is what keeps a share's teardown
	// away from the permanent deployment's hostname.
	DNSRecord string `json:"dns_record,omitempty"`
	DNSTag    string `json:"dns_tag,omitempty"`
	// ReservedName is a hostname the provider USES but did not create — a
	// name reserved in somebody else's account, typically by a JS adapter. It
	// is listed so teardown can state plainly that it is leaving it alone,
	// rather than leaving a suspiciously empty Destroy to be read as a bug.
	ReservedName string `json:"reserved_name,omitempty"`
	// StateFiles are the local files it writes, all under the state dir.
	StateFiles []string `json:"state_files,omitempty"`
}

// Creates renders the footprint as the lines a human should read before
// agreeing to any of this, and after a teardown to check the symmetry.
func (f Footprint) Creates() []string {
	var out []string
	if f.NamedTunnel != "" {
		out = append(out, "named tunnel "+f.NamedTunnel)
	}
	if f.DNSRecord != "" {
		out = append(out, fmt.Sprintf("DNS record %s (tagged %q)", f.DNSRecord, f.DNSTag))
	}
	for _, s := range f.StateFiles {
		out = append(out, "local file "+s)
	}
	if len(out) == 0 {
		out = append(out, "nothing outside this process")
	}
	sort.Strings(out)
	return out
}

// Describe is the one-line teardown contract.
func (f Footprint) Describe() string {
	s := f.Provider + " creates: " + strings.Join(f.Creates(), ", ")
	if f.ReservedName != "" {
		s += fmt.Sprintf("; uses %s, which it did NOT create and will never delete", f.ReservedName)
	}
	if f.Ephemeral {
		s += "; ephemeral — the exposure dies with the process"
	}
	return s
}

// providerPlan is the single place that answers, for one Options, BOTH
// "which provider is this?" and "how is it torn down?".
//
// Keeping them together is the point. `expose destroy` runs with no provider
// object alive — after a crash there may never have been one — so teardown
// cannot be a method that only exists once creation succeeded. It is derived
// from the same configuration that creation was derived from, which is what
// makes the two symmetric even when creation was interrupted.
type providerPlan struct {
	Kind      config.Mode
	Footprint Footprint
	// New builds the live provider. nil for lan/local, which have no process.
	New func() (Provider, error)
	// Destroy removes the footprint. Never nil; a no-op for ephemeral
	// providers, which say so rather than pretending to delete something.
	Destroy func(ctx context.Context, logf func(string, ...any)) error
}

func noDestroy(context.Context, func(string, ...any)) error { return nil }
