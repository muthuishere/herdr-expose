package expose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CloudflareOptions configures the built-in Cloudflare provider — the happy
// path, and the only supported shape: a STATIC hostname on a zone you own.
//
// There is no quick/ephemeral tunnel (C1). Domain is required.
type CloudflareOptions struct {
	Port       int    // loopback port the server listens on
	Domain     string // REQUIRED stable hostname, e.g. herdr.deemwar.com
	TunnelName string // named tunnel to create or reuse
	Bin        string // optional cloudflared path override
	StateDir   string // where credentials + config.yml are written (0700)

	// Comment is the DNS record comment used both to TAG a record we create
	// and to decide whether we are allowed to delete one. Empty means the
	// permanent tag ("herdr-expose"); a share passes ShareDNSComment so that
	// its teardown can never touch the permanent record.
	Comment string
}

// comment is the DNS tag for this options set.
func (o CloudflareOptions) comment() string {
	if c := strings.TrimSpace(o.Comment); c != "" {
		return c
	}
	return dnsComment
}

func (o CloudflareOptions) service() string {
	return fmt.Sprintf("http://127.0.0.1:%d", o.Port)
}

func (o CloudflareOptions) publicURL() string {
	return "https://" + o.Domain
}

func (o CloudflareOptions) tunnelName() string {
	if n := strings.TrimSpace(o.TunnelName); n != "" {
		return n
	}
	return "herdr-expose"
}

// provisioned is the result of the headless API provisioning sequence.
type provisioned struct {
	AccountID  string
	TunnelID   string
	ZoneID     string
	ZoneName   string
	CredsPath  string
	ConfigPath string
	Created    bool // we created the tunnel in this run
	DNSCreated bool // we created (rather than updated) the DNS record
}

// newCloudflare builds the supervised tunnel. Provisioning happens on every
// launch because every step is idempotent — that is what makes `expose start`
// safe to re-run and self-healing after a restart.
func newCloudflare(opts CloudflareOptions, logf func(string, ...any), red *redactor) (tunnel, error) {
	if strings.TrimSpace(opts.Domain) == "" {
		return nil, fmt.Errorf("expose.domain is required: the Cloudflare tunnel is static, on a hostname you own " +
			"(there is no ephemeral *.trycloudflare.com path)")
	}
	bin, err := resolveBinary(opts.Bin, "cloudflared")
	if err != nil {
		return nil, fmt.Errorf("%w\ninstall it with `brew install cloudflared` (macOS) or from "+
			"https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/", err)
	}

	var once sync.Once
	var prov *provisioned
	var provErr error

	build := func(ctx context.Context) (*exec.Cmd, error) {
		// Provision once per Start; restarts reuse the generated config.
		once.Do(func() { prov, provErr = provisionCloudflare(ctx, opts, logf, red) })
		if provErr != nil {
			return nil, provErr
		}
		cmd := exec.CommandContext(ctx, bin, "tunnel", "--no-autoupdate",
			"--config", prov.ConfigPath, "run", prov.TunnelID)
		cmd.Env = minimalEnv()
		return cmd, nil
	}

	return newProcTunnel(procSpec{
		Name:           "cloudflare",
		Mode:           "named",
		Bin:            bin,
		Build:          build,
		StaticURL:      opts.publicURL(),
		Verify:         true, // publish the URL only once /healthz actually answers
		VerifyTimeout:  90 * time.Second,
		Log:            logf,
		Redact:         red,
		HealthInterval: 30 * time.Second,
	}), nil
}

// provisionCloudflare runs the headless sequence from C2. No `cloudflared
// login`, no browser, no interactive cert.pem: the tunnel secret is minted
// here and the credentials file is written by us.
//
// It is idempotent, and it never half-provisions: if DNS fails after we created
// the tunnel, the tunnel is deleted again before returning.
func provisionCloudflare(ctx context.Context, opts CloudflareOptions, logf func(string, ...any), red *redactor) (*provisioned, error) {
	api, err := newCFAPI(red)
	if err != nil {
		return nil, err
	}
	// C3 preflight: is the token valid at all?
	if err := api.verifyToken(ctx); err != nil {
		return nil, err
	}
	account, err := api.accountID(ctx)
	if err != nil {
		return nil, err
	}

	// 1. Zone (also proves Zone:Zone:Read on the target zone).
	zone, err := api.findZone(ctx, opts.Domain)
	if err != nil {
		return nil, err
	}
	// Preflight the DNS read before creating anything, so a missing
	// Zone:DNS:Edit fails before we own any Cloudflare resources.
	existingRec, err := api.findRecord(ctx, zone.ID, opts.Domain)
	if err != nil {
		return nil, err
	}

	// 2. Tunnel: reuse or create.
	name := opts.tunnelName()
	tun, err := api.findTunnel(ctx, account, name)
	if err != nil {
		return nil, err
	}
	p := &provisioned{AccountID: account, ZoneID: zone.ID, ZoneName: zone.Name}

	stateDir, err := cloudflareStateDir(opts.StateDir)
	if err != nil {
		return nil, err
	}

	if tun != nil {
		p.TunnelID = tun.ID
		logf("cloudflare: reusing tunnel %q (%s)", name, tun.ID)
		p.CredsPath = filepath.Join(stateDir, tun.ID+".json")
		if _, err := os.Stat(p.CredsPath); err != nil {
			// The tunnel exists but we have no credentials for it — the secret
			// is only returned at creation time, so it cannot be recovered.
			return nil, fmt.Errorf("cloudflare tunnel %q already exists but its credentials file %s is missing; "+
				"the tunnel secret is only issued at creation, so either restore that file or run "+
				"`herdr-expose expose destroy` and start again (or pick another tunnel_name)", name, p.CredsPath)
		}
	} else {
		secret, err := newTunnelSecret()
		if err != nil {
			return nil, err
		}
		red.add(secret)
		logf("cloudflare: creating tunnel %q", name)
		created, err := api.createTunnel(ctx, account, name, secret)
		if err != nil {
			return nil, err
		}
		p.TunnelID, p.Created = created.ID, true
		p.CredsPath = filepath.Join(stateDir, created.ID+".json")

		// 3. Credentials file — exactly what `cloudflared login` would have
		// produced, written by us at 0600.
		if err := writeCredentials(p.CredsPath, account, created.ID, secret); err != nil {
			_ = api.deleteTunnel(ctx, account, created.ID)
			return nil, err
		}
	}

	// 4. DNS upsert. On failure, undo a tunnel we created in this run.
	target := p.TunnelID + ".cfargotunnel.com"
	rollback := func(cause error) (*provisioned, error) {
		if p.Created {
			logf("cloudflare: rolling back tunnel %s after a failure", p.TunnelID)
			_ = api.deleteTunnel(ctx, account, p.TunnelID)
			_ = os.Remove(p.CredsPath)
		}
		return nil, cause
	}
	switch {
	case existingRec == nil:
		logf("cloudflare: creating CNAME %s -> %s in zone %s", opts.Domain, target, zone.Name)
		p.DNSCreated = true
	case existingRec.Type == "CNAME" && existingRec.Content == target && existingRec.Proxied:
		logf("cloudflare: CNAME %s already points at the tunnel", opts.Domain)
	default:
		logf("cloudflare: updating %s %s -> %s in zone %s", existingRec.Type, opts.Domain, target, zone.Name)
	}
	if existingRec == nil || existingRec.Content != target || !existingRec.Proxied {
		if err := api.upsertCNAME(ctx, zone.ID, opts.Domain, target, opts.comment(), existingRec); err != nil {
			return rollback(err)
		}
	}

	// 5. Ingress config.
	p.ConfigPath = filepath.Join(stateDir, "config.yml")
	if err := writeTunnelConfig(p.ConfigPath, p.TunnelID, p.CredsPath, opts.Domain, opts.service()); err != nil {
		return rollback(err)
	}
	logf("cloudflare: provisioned %s -> %s (tunnel %s)", opts.Domain, opts.service(), p.TunnelID)
	return p, nil
}

// credentialsFile is the on-disk shape cloudflared expects.
type credentialsFile struct {
	AccountTag   string `json:"AccountTag"`
	TunnelID     string `json:"TunnelID"`
	TunnelName   string `json:"TunnelName,omitempty"`
	TunnelSecret string `json:"TunnelSecret"`
}

// writeCredentials writes the secret at 0600, atomically. This file and the
// child process are the only places the tunnel secret ever exists.
func writeCredentials(path, account, tunnelID, secret string) error {
	body, err := json.Marshal(credentialsFile{AccountTag: account, TunnelID: tunnelID, TunnelSecret: secret})
	if err != nil {
		return err
	}
	return writeFileAtomic(path, body, 0o600)
}

// writeTunnelConfig generates cloudflared's config.yml.
func writeTunnelConfig(path, tunnelID, credsPath, hostname, service string) error {
	body := fmt.Sprintf(`# Generated by herdr-expose. Do not edit; it is rewritten on every start.
tunnel: %s
credentials-file: %s
no-autoupdate: true
ingress:
  - hostname: %s
    service: %s
  - service: http_status:404
`, tunnelID, credsPath, hostname, service)
	return writeFileAtomic(path, []byte(body), 0o600)
}

// StateDir is where runtime state lives. Unlike the config path,
// $HERDR_PLUGIN_STATE_DIR IS honoured here (B6).
func StateDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("HERDR_PLUGIN_STATE_DIR")); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "herdr-expose"), nil
}

func cloudflareStateDir(override string) (string, error) {
	base := override
	if strings.TrimSpace(base) == "" {
		d, err := StateDir()
		if err != nil {
			return "", err
		}
		base = d
	}
	dir := filepath.Join(base, "cloudflare")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir %s: %w", dir, err)
	}
	return dir, os.Chmod(dir, 0o700)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// PlanStep is one action a real start would take.
type PlanStep struct {
	Action string `json:"action"` // ok | create | update | run | verify
	Detail string `json:"detail"`
}

// PlanCloudflare performs the READ-ONLY half of provisioning (preflight, zone
// lookup, tunnel lookup, DNS lookup) and reports what a real start would
// change. It creates and modifies nothing.
func PlanCloudflare(ctx context.Context, opts CloudflareOptions) ([]PlanStep, error) {
	if strings.TrimSpace(opts.Domain) == "" {
		return nil, fmt.Errorf("expose.domain is required (no ephemeral tunnels)")
	}
	red := &redactor{}
	var steps []PlanStep
	add := func(action, format string, args ...any) {
		steps = append(steps, PlanStep{action, fmt.Sprintf(format, args...)})
	}

	bin, err := resolveBinary(opts.Bin, "cloudflared")
	if err != nil {
		return nil, err
	}
	add("ok", "cloudflared: %s", bin)

	api, err := newCFAPI(red)
	if err != nil {
		return nil, err
	}
	if err := api.verifyToken(ctx); err != nil {
		return nil, err
	}
	add("ok", "API token verified (read from $%s at point of use)", CloudflareTokenEnv)

	account, err := api.accountID(ctx)
	if err != nil {
		return nil, err
	}
	add("ok", "account %s", account)

	zone, err := api.findZone(ctx, opts.Domain)
	if err != nil {
		return nil, err
	}
	add("ok", "zone %s (%s) — Zone:Zone:Read confirmed", zone.Name, zone.ID)

	stateDir, err := cloudflareStateDir(opts.StateDir)
	if err != nil {
		return nil, err
	}

	name := opts.tunnelName()
	tun, err := api.findTunnel(ctx, account, name)
	if err != nil {
		return nil, err
	}
	target := "<new-tunnel-id>.cfargotunnel.com"
	credsPath := filepath.Join(stateDir, "<new-tunnel-id>.json")
	if tun == nil {
		add("create", "tunnel %q via POST /accounts/%s/cfd_tunnel with a locally generated 32-byte secret "+
			"(config_src=local; no `cloudflared login`, no browser, no cert.pem)", name, account)
		add("create", "credentials file %s, 0600", credsPath)
	} else {
		target = tun.ID + ".cfargotunnel.com"
		credsPath = filepath.Join(stateDir, tun.ID+".json")
		add("ok", "tunnel %q already exists (%s) — would be reused", name, tun.ID)
		if _, err := os.Stat(credsPath); err != nil {
			add("blocked", "credentials file %s is missing and the secret cannot be re-issued; "+
				"start would fail until the tunnel is destroyed or tunnel_name changed", credsPath)
		} else {
			add("ok", "credentials file %s present", credsPath)
		}
	}

	rec, err := api.findRecord(ctx, zone.ID, opts.Domain)
	switch {
	case err != nil:
		return nil, err
	case rec == nil:
		add("create", "CNAME %s -> %s, proxied, comment %q", opts.Domain, target, opts.comment())
	case rec.Type == "CNAME" && rec.Content == target && rec.Proxied:
		add("ok", "CNAME %s already correct", opts.Domain)
	default:
		add("update", "%s %s: %s -> %s (proxied)", rec.Type, opts.Domain, rec.Content, target)
	}

	add("create", "%s with ingress %s -> %s", filepath.Join(stateDir, "config.yml"), opts.Domain, opts.service())
	add("run", "%s tunnel --no-autoupdate --config %s run <tunnel-id>", bin, filepath.Join(stateDir, "config.yml"))
	add("verify", "poll %s/healthz until 200, then report the URL", opts.publicURL())
	return steps, nil
}

// DestroyCloudflare removes what we created: the DNS record (only if it is
// ours) and the named tunnel, plus the local credentials. `expose stop` never
// does this — the domain is static on purpose.
func DestroyCloudflare(ctx context.Context, opts CloudflareOptions, logf func(string, ...any)) error {
	red := &redactor{}
	api, err := newCFAPI(red)
	if err != nil {
		return err
	}
	if err := api.verifyToken(ctx); err != nil {
		return err
	}
	account, err := api.accountID(ctx)
	if err != nil {
		return err
	}
	zone, err := api.findZone(ctx, opts.Domain)
	if err != nil {
		return err
	}
	rec, err := api.findRecord(ctx, zone.ID, opts.Domain)
	if err != nil {
		return err
	}
	switch {
	case rec == nil:
		logf("cloudflare: no DNS record for %s", opts.Domain)
	case rec.Comment != opts.comment():
		// Never delete a record we did not create, and never delete one
		// created under a DIFFERENT tag: this is what keeps a share teardown
		// away from the permanent deployment's hostname.
		logf("cloudflare: leaving %s alone — it is tagged %q, not %q",
			opts.Domain, rec.Comment, opts.comment())
	default:
		logf("cloudflare: deleting CNAME %s", opts.Domain)
		if err := api.deleteRecord(ctx, zone.ID, rec.ID); err != nil {
			return err
		}
	}

	name := opts.tunnelName()
	tun, err := api.findTunnel(ctx, account, name)
	if err != nil {
		return err
	}
	if tun == nil {
		logf("cloudflare: no tunnel named %q", name)
		return nil
	}
	// Cloudflare refuses to delete a tunnel while an edge connection is still
	// registered, and cloudflared takes a few seconds to deregister after it
	// is killed. Retry rather than leave an orphan tunnel behind: a share that
	// deletes its DNS but keeps its tunnel is exactly the half-teardown this
	// is supposed to prevent.
	// Measured: cloudflared's connections are marked inactive by the edge up to
	// ~60s after the process goes away, and the delete is refused until then.
	// The window is therefore generous on purpose — giving up early is how a
	// share ends up with its DNS deleted and its tunnel orphaned.
	var delErr error
	for attempt := 0; attempt < 20; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		if delErr = api.deleteTunnel(ctx, account, tun.ID); delErr == nil {
			break
		}
		logf("cloudflare: tunnel %q not deletable yet (%v); retrying", name, delErr)
	}
	if delErr != nil {
		return delErr
	}
	logf("cloudflare: deleted tunnel %q (%s)", name, tun.ID)
	if dir, err := cloudflareStateDir(opts.StateDir); err == nil {
		_ = os.Remove(filepath.Join(dir, tun.ID+".json"))
	}
	return nil
}

// resolveBinary finds an absolute path for a helper binary. launchd and
// systemd --user start us with a minimal PATH that contains none of the usual
// install dirs (B7), so look in them explicitly instead of trusting PATH.
func resolveBinary(override, name string) (string, error) {
	if o := strings.TrimSpace(override); o != "" {
		if filepath.IsAbs(o) {
			if isExecutable(o) {
				return o, nil
			}
			return "", fmt.Errorf("%s: not an executable file", o)
		}
		if p, err := exec.LookPath(o); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
		return p, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, "bin", name),
		"/opt/homebrew/bin/" + name,
		"/usr/local/bin/" + name,
		"/usr/bin/" + name,
	}
	for _, c := range candidates {
		if isExecutable(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH or in %s", name, strings.Join(candidates, ", "))
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0
}

// minimalEnv is the environment handed to a tunnel child process: enough to
// run, with none of our own secrets in it. Provider credentials are appended
// explicitly by the caller that needs them.
func minimalEnv() []string {
	keep := []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL", "TZ",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}
	env := make([]string, 0, len(keep)+2)
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if os.Getenv("PATH") == "" {
		env = append(env, "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin")
	}
	return env
}

// CheckZoneAccess is the share preflight (G4): prove the token is usable and
// that the domain's zone is reachable by it BEFORE anything is created.
//
// A share that discovers a missing zone halfway through provisioning has
// already minted a tunnel, so failing early is not politeness, it is what stops
// orphaned Cloudflare resources. It returns the zone name it matched and
// whether a record already exists on that exact hostname.
func CheckZoneAccess(ctx context.Context, domain string) (zoneName string, existing RecordState, err error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", existing, fmt.Errorf("--domain is required")
	}
	if !strings.Contains(domain, ".") {
		return "", existing, fmt.Errorf("--domain %q is not a hostname", domain)
	}
	red := &redactor{}
	api, err := newCFAPI(red)
	if err != nil {
		return "", existing, err
	}
	if err := api.verifyToken(ctx); err != nil {
		return "", existing, err
	}
	if _, err := api.accountID(ctx); err != nil {
		return "", existing, err
	}
	zone, err := api.findZone(ctx, domain)
	if err != nil {
		return "", existing, fmt.Errorf("cannot use %s: %w", domain, err)
	}
	rec, err := api.findRecord(ctx, zone.ID, domain)
	if err != nil {
		return zone.Name, existing, err
	}
	if rec != nil {
		existing = RecordState{Exists: true, Comment: rec.Comment, Content: rec.Content}
	}
	return zone.Name, existing, nil
}

// RecordState is what the Cloudflare API says about one hostname right now.
type RecordState struct {
	Exists  bool   `json:"exists"`
	Comment string `json:"comment,omitempty"`
	Content string `json:"content,omitempty"`
}

// VerifyGone re-reads Cloudflare and reports whether the DNS record and the
// named tunnel are ACTUALLY absent.
//
// This exists because a kill switch that reports what it attempted is worse
// than none: teardown must be confirmed against the source of truth, not
// inferred from an API call that returned 200. `dnsGone` is false only when a
// record with OUR comment tag still exists — a record someone else owns on the
// same name was never ours to remove and is reported separately.
func VerifyGone(ctx context.Context, opts CloudflareOptions) (dnsGone, tunnelGone bool, rec RecordState, err error) {
	red := &redactor{}
	api, err := newCFAPI(red)
	if err != nil {
		return false, false, rec, err
	}
	account, err := api.accountID(ctx)
	if err != nil {
		return false, false, rec, err
	}
	zone, err := api.findZone(ctx, opts.Domain)
	if err != nil {
		return false, false, rec, err
	}
	found, err := api.findRecord(ctx, zone.ID, opts.Domain)
	if err != nil {
		return false, false, rec, err
	}
	if found != nil {
		rec = RecordState{Exists: true, Comment: found.Comment, Content: found.Content}
	}
	dnsGone = found == nil || found.Comment != opts.comment()

	tun, err := api.findTunnel(ctx, account, opts.tunnelName())
	if err != nil {
		return dnsGone, false, rec, err
	}
	tunnelGone = tun == nil
	return dnsGone, tunnelGone, rec, nil
}
