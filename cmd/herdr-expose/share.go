package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	crand "crypto/rand"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/core"
	"github.com/muthuishere/herdr-expose/internal/expose"
	"github.com/muthuishere/herdr-expose/internal/serve"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// `herdr-expose share` — one scoped, time-boxed, self-destructing tunnel
// (SPEC AMENDMENTS 9).
//
// The shape is deliberate:
//
//   - A share is a SEPARATE detached process on its own free port with its own
//     state dir, its own auth store and `--only` set. The long-lived daemon on
//     21118 is never touched, so nothing about sharing one agent session can
//     take down, re-scope or re-auth the permanent deployment.
//   - EVERY share has a deadline. There is no permanent mode; "a month" is
//     `--days 30`. One code path means there is no branch in which a share
//     outlives its token, escapes the sweep, or skips teardown.
//   - At expiry the share DESTROYS its DNS record and tunnel. That is the one
//     exception to the static-domain rule (C1), and it is safe only because
//     share records are tagged `herdr-expose-share` while the permanent record
//     is tagged `herdr-expose` — a share teardown cannot address it.
//
// Paths are resolved BY CONSTRUCTION from the state dir and the share id.
// Nothing here takes a directory as an argument: a kill switch that writes to
// a path nobody reads looks like it worked and is worse than no kill switch.

// shareRecord is `<state>/shares/<id>/share.json`: the crash-safe deadline
// (G5.3) plus everything needed to tear the share down from OUTSIDE, without
// the share's own process being alive to ask.
type shareRecord struct {
	ID string `json:"id"`
	// Mode is the exposure mode this share resolved to, and it decides
	// everything downstream: the bind address, the URL, and whether there is
	// any Cloudflare teardown to do at all. It comes from expose.Resolve —
	// the SAME resolver the main daemon uses (AMENDMENTS 5 / E1), not a
	// second copy of the rules.
	Mode          string `json:"mode"` // cloudflare | lan
	Bind          string `json:"bind"`
	SecureContext bool   `json:"secure_context"`
	FellBack      string `json:"fell_back,omitempty"`

	Domain     string    `json:"domain,omitempty"`
	TunnelName string    `json:"tunnel_name"`
	Port       int       `json:"port"`
	Session    string    `json:"session"`
	Panes      []string  `json:"panes,omitempty"`
	Scope      string    `json:"scope"`
	URL        string    `json:"url,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	PID        int       `json:"pid,omitempty"`
	State      string    `json:"state"` // starting | ready | failed
	Error      string    `json:"error,omitempty"`
}

// shareView is the stable `--json` projection. Field names here are an API:
// an agent skill wraps every verb and parses this, so they do not churn.
type shareView struct {
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Scope   string `json:"scope"`
	Session string `json:"session"`
	Domain  string `json:"domain"`
	URL     string `json:"url"`
	// SecureContext is false for a LAN share: plain HTTP on 192.168.x.x is not
	// a secure context (localhost is exempt, a LAN IP is not), so the browser
	// grants no service worker and no PWA install there. Surfaced because the
	// person who receives the URL will otherwise wonder why it will not install.
	SecureContext bool      `json:"secure_context"`
	FellBack      string    `json:"fell_back,omitempty"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Remaining     string    `json:"remaining"`
	Seconds       int64     `json:"remaining_seconds"`
	PID           int       `json:"pid"`
	Alive         bool      `json:"alive"`
	Expired       bool      `json:"expired"`
	Error         string    `json:"error,omitempty"`
}

func (r *shareRecord) view() shareView {
	left := time.Until(r.ExpiresAt)
	if left < 0 {
		left = 0
	}
	return shareView{
		ID: r.ID, Mode: r.mode(), Scope: r.Scope, Session: r.Session, Domain: r.Domain,
		URL: r.CurrentURL(), SecureContext: r.SecureContext, FellBack: r.FellBack,
		State: r.State, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
		Remaining: left.Round(time.Second).String(), Seconds: int64(left.Seconds()),
		PID: r.PID, Alive: pidAlive(r.PID), Expired: time.Now().After(r.ExpiresAt),
		Error: r.Error,
	}
}

// mode defaults an old record (written before LAN shares existed) to cloudflare.
func (r *shareRecord) mode() string {
	if r.Mode == "" {
		return string(config.ModeCloudflare)
	}
	return r.Mode
}

// isLAN reports whether this share has no tunnel and no DNS to tear down.
func (r *shareRecord) isLAN() bool { return r.mode() == string(config.ModeLAN) }

// CurrentURL re-resolves a LAN share's address every time it is asked for.
//
// The LAN IP moves under a DHCP lease, and a URL that was true at mint time is
// worse than no URL — the daemon already re-resolves the main listener every
// 30s, so `share list` does the same rather than printing history.
func (r *shareRecord) CurrentURL() string {
	if !r.isLAN() {
		return r.URL
	}
	if ip := expose.PrimaryLANIP(); ip != "" {
		return fmt.Sprintf("http://%s:%d", ip, r.Port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", r.Port)
}

// --- paths, all derived, none accepted from a caller ------------------------

func sharesRoot() (string, error) {
	state, err := StateDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(state, "shares")
	return root, os.MkdirAll(root, 0o700)
}

// shareDirFor is the ONLY way a share directory is named. `share revoke` and
// the sweep both go through it, so neither can be pointed somewhere harmless.
func shareDirFor(id string) (string, error) {
	if !validShareID(id) {
		return "", fmt.Errorf("invalid share id %q", id)
	}
	root, err := sharesRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, id), nil
}

func validShareID(id string) bool {
	if len(id) < 4 || len(id) > 32 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func newShareID() string {
	b := make([]byte, 4)
	if _, err := crand.Read(b); err != nil {
		return fmt.Sprintf("%08x", rand.Int31()) //nolint:gosec // fallback id only
	}
	return hex.EncodeToString(b)
}

// shareTunnelName is derived from the id, never configurable: two shares must
// never collide on a Cloudflare tunnel, and a share must never be able to name
// the permanent tunnel.
func shareTunnelName(id string) string { return "herdr-expose-share-" + id }

func loadShare(id string) (*shareRecord, error) {
	dir, err := shareDirFor(id)
	if err != nil {
		return nil, err
	}
	return readShareFile(dir)
}

func readShareFile(dir string) (*shareRecord, error) {
	b, err := os.ReadFile(filepath.Join(dir, "share.json"))
	if err != nil {
		return nil, err
	}
	var r shareRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "share.json"), err)
	}
	return &r, nil
}

func writeShareFile(dir string, r *shareRecord) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".share.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "share.json"))
}

// updateShare is a locked read-modify-write. The share's own process and the
// CLI (`share extend`) both touch this file, so the deadline is never updated
// by a blind overwrite.
func updateShare(id string, fn func(*shareRecord)) (*shareRecord, error) {
	dir, err := shareDirFor(id)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "share.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	rec, err := readShareFile(dir)
	if err != nil {
		return nil, err
	}
	fn(rec)
	if err := writeShareFile(dir, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// listShareRecords reads every share on disk, newest first.
func listShareRecords() ([]*shareRecord, error) {
	root, err := sharesRoot()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []*shareRecord
	for _, e := range ents {
		if !e.IsDir() || !validShareID(e.Name()) {
			continue
		}
		rec, err := readShareFile(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		rec.ID = e.Name() // the directory is the truth, not the file's field
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// shareCFOptions rebuilds the Cloudflare view of a share from its record.
// Every teardown path uses it, so the hostname, tunnel name, state dir AND the
// `herdr-expose-share` tag are all fixed by construction.
func shareCFOptions(rec *shareRecord) expose.CloudflareOptions {
	dir, _ := shareDirFor(rec.ID)
	name := rec.TunnelName
	if name == "" {
		name = shareTunnelName(rec.ID)
	}
	return expose.CloudflareOptions{
		Port:       rec.Port,
		Domain:     rec.Domain,
		TunnelName: name,
		StateDir:   dir,
		Comment:    expose.ShareDNSComment,
	}
}

// --- command dispatch -------------------------------------------------------

func cmdShare(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "list", "ls":
			return cmdShareList(args[1:])
		case "extend":
			return cmdShareExtend(args[1:])
		case "revoke", "stop", "destroy":
			return cmdShareRevoke(args[1:])
		case "restore":
			return cmdShareRestore(args[1:])
		case "pair":
			return cmdSharePair(args[1:])
		case "run-internal":
			return cmdShareRun(args[1:])
		case "help", "--help", "-h":
			shareUsage()
			return nil
		}
	}
	return cmdShareCreate(args)
}

func shareUsage() {
	fmt.Fprint(os.Stderr, `herdr-expose share — one scoped, time-boxed, self-destructing tunnel

  share [--domain X | --lan] [--session NAME] [--pane TARGET] [--hours N] [--days N] [--json]
        expose ONE herdr session (default: this one) for N hours (default 1)
          --domain X  public https on X through a Cloudflare named tunnel
          --lan       http://<lan-ip>:<port>, no DNS and no tunnel
          neither     auto: cloudflare when a usable domain is configured and
                      cloudflared resolves, else LAN — the reason is printed
  share list [--json]              list shares; reaps any whose deadline passed
  share extend <id> --hours N|--days N [--json]
  share pair <id> [--name NAME] [--json]
                                   mint another pairing code for a live share
  share revoke <id> [--json]       stop, delete DNS + tunnel, wipe state
  share revoke --all [--json]      the same, for every share
  share restore [--json]           reap expired shares and respawn crashed ones
                                   (the main daemon does this on startup too)
  panic [--json]                   revoke --all AND stop the main tunnel

Every share expires. There is no permanent share: a long one is --days 30.
`)
}

// --- create -----------------------------------------------------------------

type shareFlags struct {
	lan     bool
	domain  string
	session string
	panes   []string
	hours   float64
	days    float64
	name    string
	asJSON  bool
}

func parseShareFlags(args []string) (shareFlags, error) {
	f := shareFlags{name: "share"}
	hoursSet := false
	for i := 0; i < len(args); i++ {
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", args[i])
			}
			i++
			return args[i], nil
		}
		var err error
		switch args[i] {
		case "--domain", "-d":
			f.domain, err = next()
		case "--session", "-s":
			f.session, err = next()
		case "--pane", "--only-target", "-p":
			var v string
			if v, err = next(); err == nil {
				f.panes = append(f.panes, v)
			}
		case "--hours":
			var v string
			if v, err = next(); err == nil {
				if _, e := fmt.Sscanf(v, "%f", &f.hours); e != nil {
					err = fmt.Errorf("--hours %q is not a number", v)
				}
				hoursSet = true
			}
		case "--days":
			var v string
			if v, err = next(); err == nil {
				if _, e := fmt.Sscanf(v, "%f", &f.days); e != nil {
					err = fmt.Errorf("--days %q is not a number", v)
				}
			}
		case "--name":
			f.name, err = next()
		case "--lan":
			f.lan = true
		case "--json":
			f.asJSON = true
		default:
			err = fmt.Errorf("unknown flag %q", args[i])
		}
		if err != nil {
			return f, err
		}
	}
	if !hoursSet && f.days == 0 {
		f.hours = 1 // G1 default
	}
	return f, nil
}

// ttl is the share's lifetime. It is always finite and always > 0.
func (f shareFlags) ttl() (time.Duration, error) {
	d := time.Duration(f.hours*float64(time.Hour)) + time.Duration(f.days*float64(24*time.Hour))
	if d <= 0 {
		return 0, errors.New("the share TTL must be positive: use --hours N or --days N")
	}
	if d > 365*24*time.Hour {
		return 0, errors.New("the share TTL must be under a year; a share is time-boxed by design")
	}
	return d, nil
}

// defaultSessionName is the session the command is RUNNING IN: $HERDR_SESSION,
// else derived from $HERDR_SOCKET_PATH (G1).
func defaultSessionName() string {
	if n := strings.TrimSpace(os.Getenv("HERDR_SESSION")); n != "" {
		return n
	}
	if n, _ := upstream.OriginSession(); n != "" {
		return n
	}
	return ""
}

// resolveShareMode decides cloudflare vs lan through expose.Resolve — the same
// three-mode resolver the main daemon uses (E1). There is deliberately no
// second set of rules here: this function only chooses which [expose] table to
// hand it, and explains the choice.
//
// Auto mode will NOT reuse the configured domain when that hostname already
// belongs to something else — in practice the permanent deployment, whose
// record is tagged `herdr-expose`. A share may only ever create and delete
// records tagged `herdr-expose-share`, so taking the daemon's hostname is not
// on the table; it falls back to LAN and says why.
func resolveShareMode(ctx context.Context, f shareFlags, port int, tunnelName string) (expose.Resolution, config.Expose, error) {
	var none config.Expose
	if f.lan && strings.TrimSpace(f.domain) != "" {
		return expose.Resolution{}, none, errors.New("--lan and --domain are mutually exclusive: pick a LAN address or a public hostname")
	}

	if f.lan {
		e := config.Expose{LAN: true}
		return expose.Resolve(e, port, nil), e, nil
	}

	// Explicit --domain: a failure here is a hard error, never a silent
	// downgrade. The person asked for a public hostname.
	if d := strings.TrimSpace(f.domain); d != "" {
		if _, rec, err := expose.CheckZoneAccess(ctx, d); err != nil {
			return expose.Resolution{}, none, err
		} else if rec.Exists && rec.Comment != expose.ShareDNSComment {
			return expose.Resolution{}, none, fmt.Errorf("%s already has a DNS record tagged %q — a share only ever creates and deletes "+
				"records tagged %q, so it will not take this hostname over. Pick another hostname",
				d, rec.Comment, expose.ShareDNSComment)
		}
		e := config.Expose{Cloudflare: true, Domain: d, TunnelName: tunnelName}
		res := expose.Resolve(e, port, nil)
		if res.Mode != config.ModeCloudflare {
			return expose.Resolution{}, none, fmt.Errorf("cannot share on %s: %s (pass --lan to expose on this network instead)",
				d, res.FellBack)
		}
		return res, e, nil
	}

	// Auto.
	lan := config.Expose{LAN: true}
	fallback := func(why string) (expose.Resolution, config.Expose, error) {
		r := expose.Resolve(lan, port, nil)
		r.FellBack = why
		return r, lan, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return fallback("could not read the config, so no public domain is known")
	}
	domain := strings.TrimSpace(cfg.Expose.Domain)
	if domain == "" {
		return fallback("no domain is configured under [expose], so there is no public hostname to put a share on")
	}
	if res := expose.Resolve(config.Expose{Cloudflare: true, Domain: domain}, port, nil); res.Mode != config.ModeCloudflare {
		return fallback(res.FellBack)
	}
	_, rec, err := expose.CheckZoneAccess(ctx, domain)
	if err != nil {
		return fallback(fmt.Sprintf("the zone for %s is not reachable with the configured Cloudflare token (%v)", domain, err))
	}
	if rec.Exists && rec.Comment != expose.ShareDNSComment {
		return fallback(fmt.Sprintf("%s is the permanent deployment's hostname (record tagged %q), which a share must never take over; "+
			"pass --domain <other-host> for a public share", domain, rec.Comment))
	}
	e := config.Expose{Cloudflare: true, Domain: domain, TunnelName: tunnelName}
	return expose.Resolve(e, port, nil), e, nil
}

func cmdShareCreate(args []string) error {
	f, err := parseShareFlags(args)
	if err != nil {
		return err
	}
	ttl, err := f.ttl()
	if err != nil {
		return err
	}
	if f.session == "" {
		f.session = defaultSessionName()
	}
	if f.session == "" {
		return errors.New("cannot tell which herdr session to share: run this inside a herdr pane, " +
			"or pass --session NAME")
	}
	// Normalise --pane values: accept `w1:p1` or `<session>/w1:p1`.
	var onlyTargets []string
	for _, p := range f.panes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if core.SessionOf(p) == "" {
			p = core.JoinTarget(f.session, p)
		}
		onlyTargets = append(onlyTargets, p)
	}
	scope, err := core.ParseScope(f.session, onlyTargets)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The session must actually exist, or the share comes up empty and the
	// human blames the tunnel.
	if sessions, err := upstream.ListSessions(ctx); err == nil {
		found := false
		var names []string
		for _, s := range upstream.RunningSessions(sessions) {
			names = append(names, s.Name)
			if s.Name == f.session {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("no running herdr session named %q (running: %s)",
				f.session, strings.Join(names, ", "))
		}
	}

	// Each share gets its own ephemeral port whatever the mode; the bind
	// address is then decided by the RESOLVED mode, never by a flag.
	port, err := freeLoopbackPort()
	if err != nil {
		return err
	}
	id := newShareID()
	res, _, err := resolveShareMode(ctx, f, port, shareTunnelName(id))
	if err != nil {
		return err
	}
	if res.FellBack != "" {
		// ONE clear line. The owner must never have to guess whether he got a
		// public URL or a LAN one.
		fmt.Fprintf(os.Stderr, "\n  NOTE: no public tunnel — %s\n        exposing on this network instead.\n\n", res.FellBack)
	}

	dir, err := shareDirFor(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	now := time.Now()
	share := &shareRecord{
		ID: id, Mode: string(res.Mode), Bind: res.Bind,
		SecureContext: res.SecureContext, FellBack: res.FellBack,
		Port: port, Session: f.session, Panes: scope.Panes, Scope: scope.String(),
		CreatedAt: now, ExpiresAt: now.Add(ttl), State: "starting",
	}
	if res.Mode == config.ModeCloudflare {
		share.Domain, share.TunnelName = strings.TrimSuffix(strings.TrimPrefix(res.URL, "https://"), "/"), shareTunnelName(id)
	}
	if err := writeShareFile(dir, share); err != nil {
		return err
	}

	// Pairing is minted HERE, in the local process, before the server exists —
	// exactly as `herdr-expose pair` does. No endpoint mints one, the code is
	// shown only on this machine, and it cannot outlive the share.
	//
	// It applies to a LAN share exactly as it does to a tunnel: coffee-shop
	// wifi is a LAN, and "on our network" is not an identity.
	auth, err := serve.NewAuth(dir, newLogger(), nil)
	if err != nil {
		return err
	}
	if err := auth.SetDeadline(share.ExpiresAt); err != nil {
		return err
	}
	if _, _, err := auth.EnsureServerToken(); err != nil {
		return err
	}
	code, codeExp, err := auth.NewPairingCode(f.name)
	if err != nil {
		return err
	}

	if err := spawnShare(id, dir); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}

	final, err := waitForShare(id, 150*time.Second)
	if err != nil {
		// Teardown is the child's job, but if it never got that far, do it here
		// rather than leaving a half-provisioned hostname behind.
		_ = revokeShare(id)
		return err
	}
	if final.State == "failed" {
		_ = revokeShare(id)
		return fmt.Errorf("share %s failed to start: %s", id, final.Error)
	}

	url := final.CurrentURL()
	if f.asJSON {
		printJSON(map[string]any{
			"share": final.view(), "pairing_code": code, "pairing_expires_at": codeExp,
			"pair_url": url + "/?pair=" + code,
		})
		return nil
	}
	fmt.Printf("\n  shared %s on %s\n\n", scope.String(), url)
	printQR(url + "/?pair=" + code)
	fmt.Printf("\n  url:           %s\n  mode:          %s\n  pairing code:  %s  (valid until %s)\n",
		url, final.mode(), code, codeExp.Format(time.Kitchen))
	fmt.Printf("  scope:         %s  (nothing else on this machine is in this instance's tree)\n", scope.String())
	if final.isLAN() {
		fmt.Printf("  expires:       %s  (in %s) — the instance exits and its state is wiped then\n",
			final.ExpiresAt.Format(time.RFC1123), time.Until(final.ExpiresAt).Round(time.Minute))
		fmt.Printf("  reachable by:  anyone on this network who ALSO has the pairing code above\n")
		fmt.Printf("  not installable: plain HTTP on a LAN IP is not a secure context, so no PWA install\n" +
			"                   and no service worker (that is the mode, not a bug)\n")
		fmt.Printf("  note:          this QR goes stale if the DHCP lease changes the LAN IP —\n"+
			"                 `herdr-expose share pair %s` re-issues one at the current address\n", id)
	} else {
		fmt.Printf("  expires:       %s  (in %s) — DNS record and tunnel are deleted then\n",
			final.ExpiresAt.Format(time.RFC1123), time.Until(final.ExpiresAt).Round(time.Minute))
	}
	fmt.Printf("  share id:      %s   (herdr-expose share extend %s --hours 1 | share revoke %s)\n\n",
		id, id, id)
	return nil
}

// freeLoopbackPort asks the kernel for a port nobody is using. A share must
// never collide with the daemon on 21118 or with another share.
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// spawnShare fork-execs the detached scoped instance. It inherits the
// environment so that $CLOUDFLARE_ALLPURPOSE_TOKEN reaches the child's
// provisioning code by NAME; the value is never read, printed or stored here.
func spawnShare(id, dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "share.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "share", "run-internal", "--id", id)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// waitForShare blocks until the child publishes a verified URL or fails.
func waitForShare(id string, timeout time.Duration) (*shareRecord, error) {
	deadline := time.Now().Add(timeout)
	if rec, err := loadShare(id); err == nil && rec.isLAN() {
		fmt.Fprintf(os.Stderr, "  starting the scoped instance on %s…\n", rec.CurrentURL())
	} else {
		fmt.Fprintf(os.Stderr, "  provisioning tunnel and waiting for the hostname to answer…\n")
	}
	for time.Now().Before(deadline) {
		rec, err := loadShare(id)
		if err == nil && (rec.State == "ready" || rec.State == "failed") {
			return rec, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("share %s did not come up within %s (see the log in its state dir)", id, timeout)
}

// --- list -------------------------------------------------------------------

func cmdShareList(args []string) error {
	asJSON := hasFlag(args, "--json")
	// G5.3: listing is also a reaper. A share whose deadline passed and whose
	// process is gone is destroyed here, so a crashed share cannot leave a live
	// hostname resolving to a tunnel that no longer exists.
	reaped := sweepShares(nil)
	recs, err := listShareRecords()
	if err != nil {
		return err
	}
	if asJSON {
		views := make([]shareView, 0, len(recs))
		for _, r := range recs {
			views = append(views, r.view())
		}
		printJSON(map[string]any{"shares": views, "reaped": reaped})
		return nil
	}
	if len(reaped) > 0 {
		fmt.Printf("reaped %d expired share(s): %s\n", len(reaped), strings.Join(reaped, ", "))
	}
	if len(recs) == 0 {
		fmt.Println("no shares")
		return nil
	}
	fmt.Printf("%-10s %-11s %-26s %-30s %-9s %-7s %s\n",
		"ID", "MODE", "SCOPE", "URL", "STATE", "PID", "EXPIRES IN")
	for _, r := range recs {
		v := r.view()
		state := v.State
		if !v.Alive {
			state += "(dead)"
		}
		fmt.Printf("%-10s %-11s %-26s %-30s %-9s %-7d %s\n",
			v.ID, v.Mode, v.Scope, v.URL, state, v.PID, v.Remaining)
	}
	for _, r := range recs {
		if v := r.view(); !v.SecureContext {
			fmt.Printf("\nnote: %s is plain HTTP on a LAN IP — not a secure context, so no PWA install\n"+
				"      and no service worker there. The URL is re-resolved on every listing, so it\n"+
				"      follows a DHCP lease change; a printed QR does not.\n", v.ID)
		}
	}
	return nil
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

// --- extend -----------------------------------------------------------------

func cmdShareExtend(args []string) error {
	var id string
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && id == "" && !isFlagValue(args, a) {
			id = a
			continue
		}
		rest = append(rest, a)
	}
	if id == "" {
		return errors.New("usage: herdr-expose share extend <id> --hours N | --days N")
	}
	f, err := parseShareFlags(rest)
	if err != nil {
		return err
	}
	d, err := f.ttl()
	if err != nil {
		return err
	}
	rec, err := updateShare(id, func(r *shareRecord) {
		base := time.Now()
		if r.ExpiresAt.After(base) {
			base = r.ExpiresAt
		}
		r.ExpiresAt = base.Add(d)
	})
	if err != nil {
		return err
	}
	// The tokens must move WITH the share, or they expire underneath a share
	// that is still running. The live instance re-reads share.json and does the
	// same thing; doing it here too makes `extend` correct even if the
	// instance is momentarily wedged.
	dir, _ := shareDirFor(id)
	if auth, err := serve.NewAuth(dir, newLogger(), nil); err == nil {
		_ = auth.SetDeadline(rec.ExpiresAt)
	}
	if f.asJSON {
		printJSON(rec.view())
		return nil
	}
	fmt.Printf("share %s now expires %s (in %s)\n", id,
		rec.ExpiresAt.Format(time.RFC1123), time.Until(rec.ExpiresAt).Round(time.Minute))
	return nil
}

// isFlagValue reports whether v appears immediately after a value-taking flag.
func isFlagValue(args []string, v string) bool {
	for i := 1; i < len(args); i++ {
		if args[i] != v {
			continue
		}
		switch args[i-1] {
		case "--hours", "--days", "--domain", "--session", "--pane", "--name", "-d", "-s", "-p":
			return true
		}
	}
	return false
}

// cmdSharePair mints ANOTHER pairing code for a running share — a second phone,
// or a first one after the original code expired.
//
// Same rules as everywhere: the code is minted by the local CLI, displayed only
// on this machine, and here it additionally cannot outlive the share (the auth
// store's deadline clamps it).
func cmdSharePair(args []string) error {
	var id string
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && id == "" && !isFlagValue(args, a) {
			id = a
			continue
		}
		rest = append(rest, a)
	}
	if id == "" {
		return errors.New("usage: herdr-expose share pair <id> [--name NAME]")
	}
	f, err := parseShareFlags(rest)
	if err != nil {
		return err
	}
	rec, err := loadShare(id)
	if err != nil {
		return err
	}
	if time.Now().After(rec.ExpiresAt) {
		return fmt.Errorf("share %s has expired", id)
	}
	dir, err := shareDirFor(id)
	if err != nil {
		return err
	}
	auth, err := serve.NewAuth(dir, newLogger(), nil)
	if err != nil {
		return err
	}
	if err := auth.SetDeadline(rec.ExpiresAt); err != nil {
		return err
	}
	code, exp, err := auth.NewPairingCode(f.name)
	if err != nil {
		return err
	}
	// Re-issued at the CURRENT address: a LAN share's IP moves with the lease,
	// and the whole point of minting a new code is to hand someone a QR that
	// works now.
	url := rec.CurrentURL()
	if url == "" {
		url = "https://" + rec.Domain
	}
	if f.asJSON {
		printJSON(map[string]any{"share": rec.view(), "pairing_code": code,
			"pairing_expires_at": exp, "pair_url": url + "/?pair=" + code})
		return nil
	}
	fmt.Printf("\n  scan this on the device — shown ONLY here, never sent over the tunnel\n\n")
	printQR(url + "/?pair=" + code)
	fmt.Printf("\n  pairing code: %s\n  valid until:  %s\n  url:          %s\n\n",
		code, exp.Format(time.Kitchen), url)
	return nil
}

// --- revoke -----------------------------------------------------------------

// teardownResult is one share's verified teardown outcome.
type teardownResult struct {
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	Domain      string `json:"domain"`
	ProcessGone bool   `json:"process_gone"`
	DNSGone     bool   `json:"dns_gone"`
	TunnelGone  bool   `json:"tunnel_gone"`
	PortFree    bool   `json:"port_free"`
	StateWiped  bool   `json:"state_wiped"`
	Error       string `json:"error,omitempty"`
}

func (t teardownResult) ok() bool {
	return t.ProcessGone && t.DNSGone && t.TunnelGone && t.PortFree && t.StateWiped
}

func cmdShareRevoke(args []string) error {
	asJSON := hasFlag(args, "--json")
	all := hasFlag(args, "--all")
	var id string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			id = a
		}
	}
	if !all && id == "" {
		return errors.New("usage: herdr-expose share revoke <id> | --all")
	}

	var targets []*shareRecord
	if all {
		recs, err := listShareRecords()
		if err != nil {
			return err
		}
		targets = recs
	} else {
		rec, err := loadShare(id)
		if err != nil {
			if os.IsNotExist(err) {
				// Idempotent: revoking something already gone is success.
				if asJSON {
					printJSON(map[string]any{"revoked": []teardownResult{}, "ok": true})
				} else {
					fmt.Printf("share %s: already gone\n", id)
				}
				return nil
			}
			return err
		}
		targets = []*shareRecord{rec}
	}

	results := make([]teardownResult, 0, len(targets))
	failed := 0
	for _, rec := range targets {
		res := revokeShareRecord(rec)
		if !res.ok() {
			failed++
		}
		results = append(results, res)
	}
	if asJSON {
		printJSON(map[string]any{"revoked": results, "ok": failed == 0})
	} else {
		for _, r := range results {
			printTeardown(r)
		}
		if len(results) == 0 {
			fmt.Println("no shares to revoke")
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d share(s) did NOT tear down completely", failed, len(results))
	}
	return nil
}

func printTeardown(r teardownResult) {
	mark := func(ok bool) string {
		if ok {
			return "gone"
		}
		return "STILL THERE"
	}
	where := r.Domain
	if where == "" {
		where = "lan"
	}
	fmt.Printf("share %s (%s, %s)\n", r.ID, r.Mode, where)
	fmt.Printf("  process      %s\n", mark(r.ProcessGone))
	if r.Mode == string(config.ModeLAN) {
		fmt.Printf("  dns/tunnel   n/a (LAN share: nothing was ever created)\n")
	} else {
		fmt.Printf("  dns record   %s\n", mark(r.DNSGone))
		fmt.Printf("  tunnel       %s\n", mark(r.TunnelGone))
	}
	fmt.Printf("  port         %s\n", map[bool]string{true: "released", false: "STILL BOUND"}[r.PortFree])
	fmt.Printf("  state dir    %s\n", map[bool]string{true: "wiped", false: "STILL THERE"}[r.StateWiped])
	if r.Error != "" {
		fmt.Printf("  error        %s\n", r.Error)
	}
}

// revokeShare tears one share down by id, ignoring a missing share.
func revokeShare(id string) error {
	rec, err := loadShare(id)
	if err != nil {
		return nil //nolint:nilerr // already gone is success
	}
	res := revokeShareRecord(rec)
	if !res.ok() {
		return fmt.Errorf("share %s did not tear down completely: %+v", id, res)
	}
	return nil
}

// revokeShareRecord is the whole teardown, and it works with NO live process:
// everything it needs is in share.json, so a crashed share cleans up exactly
// like a healthy one.
//
// It then RE-READS Cloudflare and the port and reports what is genuinely gone,
// because a teardown that reports its own intentions is how you end up with a
// hostname that still resolves.
func revokeShareRecord(rec *shareRecord) teardownResult {
	res := teardownResult{ID: rec.ID, Mode: rec.mode(), Domain: rec.Domain}
	dir, err := shareDirFor(rec.ID)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	// 1. the process
	res.ProcessGone = killShareProcess(rec.PID)

	// A LAN share has no DNS and no tunnel, so the Cloudflare path is SKIPPED
	// rather than run and no-opped: no API call, no token read, nothing to
	// fail. Teardown is exactly "process gone, port free, state wiped" — and
	// `revoke --all` / `panic` therefore handle a mix of LAN and tunnel shares
	// without either kind failing the other.
	if rec.isLAN() {
		res.DNSGone, res.TunnelGone = true, true
		res.PortFree = rec.Port == 0 || portFree(fmt.Sprintf("0.0.0.0:%d", rec.Port))
		if err := os.RemoveAll(dir); err != nil {
			res.Error = err.Error()
		}
		_, statErr := os.Stat(dir)
		res.StateWiped = os.IsNotExist(statErr)
		return res
	}

	// 2. cloudflared, DNS and the tunnel — tagged `herdr-expose-share` only, so
	//    this can never reach the permanent deployment's record.
	ctx0, cancel0 := context.WithTimeout(context.Background(), 30*time.Second)
	opts0 := shareCFOptions(rec)
	dnsGone0, tunGone0, _, err0 := expose.VerifyGone(ctx0, opts0)
	cancel0()
	if err0 == nil && dnsGone0 && tunGone0 {
		// Already clean (a retry of a partial teardown): do not spend a
		// 100-second delete window proving it again.
		res.DNSGone, res.TunnelGone = true, true
		res.PortFree = rec.Port == 0 || portFree(fmt.Sprintf("127.0.0.1:%d", rec.Port))
		_ = os.RemoveAll(dir)
		_, statErr := os.Stat(dir)
		res.StateWiped = os.IsNotExist(statErr)
		return res
	}

	cfConfig := filepath.Join(dir, "cloudflare", "config.yml")
	killStrayCloudflared(cfConfig)
	// Cloudflare refuses to delete a tunnel that still has active
	// connections, so WAIT for cloudflared to actually be gone rather than
	// racing it.
	waitForNoCloudflared(cfConfig, 20*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	opts := shareCFOptions(rec)
	var msgs []string
	if err := expose.DestroyCloudflare(ctx, opts, func(f string, a ...any) {
		msgs = append(msgs, fmt.Sprintf(f, a...))
	}); err != nil {
		res.Error = err.Error()
	}

	// 3. VERIFY against the API, do not assume.
	if dnsGone, tunGone, state, err := expose.VerifyGone(ctx, opts); err != nil {
		if res.Error == "" {
			res.Error = "verification failed: " + err.Error()
		}
	} else {
		res.DNSGone, res.TunnelGone = dnsGone, tunGone
		if state.Exists && !dnsGone {
			res.Error = strings.TrimSpace(res.Error + fmt.Sprintf(
				" DNS record for %s still present (comment %q)", rec.Domain, state.Comment))
		}
	}

	// 4. the port must actually be released
	res.PortFree = rec.Port == 0 || portFree(fmt.Sprintf("127.0.0.1:%d", rec.Port))

	// 5. the state dir. Credentials go IMMEDIATELY and unconditionally — the
	//    hashed device tokens, the server token and the tunnel secret are gone
	//    whether or not the rest succeeded.
	for _, f := range []string{"auth.json", "auth.json.tmp", "share.lock"} {
		_ = os.Remove(filepath.Join(dir, f))
	}
	_ = os.RemoveAll(filepath.Join(dir, "cloudflare"))

	// The RECORD only goes once Cloudflare is confirmed clean. A leftover
	// share.json is recoverable — the next `share list` or daemon sweep picks
	// it up and retries — while a deleted record with a surviving tunnel is
	// an orphan nothing will ever clean up. So a partial teardown keeps its
	// paperwork and says so.
	if res.DNSGone && res.TunnelGone {
		if err := os.RemoveAll(dir); err != nil {
			res.Error = strings.TrimSpace(res.Error + " " + err.Error())
		}
		_, statErr := os.Stat(dir)
		res.StateWiped = os.IsNotExist(statErr)
		return res
	}
	res.StateWiped = false
	if res.Error == "" {
		res.Error = "teardown incomplete; the share record is kept so the next sweep retries"
	}
	_, _ = updateShare(rec.ID, func(r *shareRecord) {
		r.State, r.Error, r.PID, r.URL = "orphaned", res.Error, 0, ""
	})
	return res
}

// killShareProcess SIGTERMs the scoped instance, waits for it, then SIGKILLs.
//
// It refuses to kill THIS process. The share tears itself down through the very
// same function at expiry, and signalling its own pid there means it SIGKILLs
// itself halfway through — the DNS record goes, the tunnel survives, and the
// next `share list` finds a dead process with nothing left to clean up. Hit for
// real on the first live test.
func killShareProcess(pid int) bool {
	if pid <= 0 || pid == os.Getpid() || !pidAlive(pid) {
		return true
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGKILL)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// killStrayCloudflared kills any cloudflared still running against a specific
// generated config path. The match is on a path we DERIVED, never on a name, so
// it cannot hit another tunnel by accident.
func killStrayCloudflared(configPath string) int {
	if configPath == "" {
		return 0
	}
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return 0
	}
	killed := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, configPath) || !strings.Contains(line, "cloudflared") {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
			killed++
		}
	}
	return killed
}

// --- sweep / restore (G5.3 + crash and reboot safety) -----------------------

// sweepShares reaps every share whose deadline has passed and whose process is
// gone, and also a share that is past its deadline but whose process refuses to
// exit. Called by `share list` and by the MAIN daemon on a timer.
func sweepShares(log *slog.Logger) []string {
	recs, err := listShareRecords()
	if err != nil {
		return nil
	}
	var reaped []string
	for _, rec := range recs {
		if time.Now().Before(rec.ExpiresAt) {
			continue
		}
		alive := pidAlive(rec.PID)
		// Give a live instance a grace period to destroy itself properly.
		if alive && time.Since(rec.ExpiresAt) < 2*time.Minute {
			continue
		}
		if log != nil {
			log.Warn("reaping expired share", "id", rec.ID, "domain", rec.Domain,
				"expired", rec.ExpiresAt, "process_alive", alive)
		}
		res := revokeShareRecord(rec)
		reaped = append(reaped, rec.ID)
		if log != nil && !res.ok() {
			log.Warn("expired share did not tear down completely",
				"id", rec.ID, "dns_gone", res.DNSGone, "tunnel_gone", res.TunnelGone, "err", res.Error)
		}
	}
	return reaped
}

// restoreShares is what makes a long share survive a reboot or a daemon
// restart (AMENDMENTS 10): a share whose process is gone but whose deadline has
// NOT passed is respawned with its ORIGINAL deadline — never a refreshed one —
// and one whose deadline has passed is reaped instead. A share must never come
// back with a fresh clock, and must never leave a live DNS record behind.
func restoreShares(log *slog.Logger) {
	sweepShares(log)
	recs, err := listShareRecords()
	if err != nil {
		return
	}
	for _, rec := range recs {
		if time.Now().After(rec.ExpiresAt) || pidAlive(rec.PID) {
			continue
		}
		dir, err := shareDirFor(rec.ID)
		if err != nil {
			continue
		}
		log.Info("restoring share after restart", "id", rec.ID, "domain", rec.Domain,
			"expires_at", rec.ExpiresAt)
		if err := spawnShare(rec.ID, dir); err != nil {
			log.Warn("could not restore share", "id", rec.ID, "err", err)
		}
	}
}

// cmdShareRestore runs the startup reconciliation by hand. The main daemon does
// exactly this on boot; having it as a verb makes it testable and gives an
// operator a way to recover shares after a crash without restarting anything.
func cmdShareRestore(args []string) error {
	log := newLogger()
	restoreShares(log)
	// A respawned instance records its new pid a moment after it starts; give
	// it that moment so the listing reports the live process rather than the
	// dead one it just replaced.
	time.Sleep(2 * time.Second)
	recs, err := listShareRecords()
	if err != nil {
		return err
	}
	views := make([]shareView, 0, len(recs))
	for _, r := range recs {
		views = append(views, r.view())
	}
	if hasFlag(args, "--json") {
		printJSON(map[string]any{"shares": views})
		return nil
	}
	for _, v := range views {
		fmt.Printf("%s  %s  pid=%d alive=%v expires in %s\n", v.ID, v.Domain, v.PID, v.Alive, v.Remaining)
	}
	if len(views) == 0 {
		fmt.Println("no shares")
	}
	return nil
}

// --- the scoped instance itself ---------------------------------------------

// shareServeConfig is the serve.Config for a share. It is built from the share
// record, not from ~/.config/herdr-expose/config.toml: a share must not be able
// to pick up the daemon's port, and a change to the daemon's config must not
// re-point a running share.
// It is derived ENTIRELY from the share's expose.Manager, so the bind address,
// the mode and the Origin allowlist come from the same Resolution the main
// daemon uses — including the LAN origin, which the browser must see in the
// allowlist or it refuses the WebSocket, and which moves with the DHCP lease.
type shareServeConfig struct{ mgr *expose.Manager }

func (c shareServeConfig) Port() int    { return c.mgr.Resolution().Port }
func (c shareServeConfig) Bind() string { return c.mgr.Resolution().Bind }
func (c shareServeConfig) Mode() string { return string(c.mgr.Resolution().Mode) }
func (c shareServeConfig) AllowedOrigins() []string {
	return c.mgr.AllowedOrigins(nil)
}
func (c shareServeConfig) UI() map[string]any {
	return map[string]any{"theme": "auto", "default_view": "focus"}
}

// cmdShareRun is the detached scoped instance. Not a user-facing verb.
func cmdShareRun(args []string) error {
	id := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--id" {
			id = args[i+1]
		}
	}
	dir, err := shareDirFor(id)
	if err != nil {
		return err
	}
	rec, err := readShareFile(dir)
	if err != nil {
		return err
	}
	log := newLogger().With("share", id)

	if time.Now().After(rec.ExpiresAt) {
		log.Warn("share is already past its deadline; destroying instead of starting")
		revokeShareRecord(rec)
		return nil
	}

	scope, err := core.ParseScope(rec.Session, qualify(rec.Session, rec.Panes))
	if err != nil {
		return err
	}

	// The [expose] table is rebuilt from the RECORD, so the instance resolves
	// the same mode it was created with — a LAN share never tries a tunnel and
	// a tunnel share never silently degrades to LAN on restart.
	exp := config.Expose{LAN: true}
	if !rec.isLAN() {
		exp = config.Expose{Cloudflare: true, Domain: rec.Domain, TunnelName: rec.TunnelName}
	}
	mgr := expose.New(expose.Options{
		Port:       rec.Port,
		Expose:     exp,
		StateDir:   dir,
		DNSComment: expose.ShareDNSComment,
		Logf:       func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) },
	})
	cfg := shareServeConfig{mgr: mgr}
	res := mgr.Resolution()
	log.Info("share exposure resolved", "mode", res.Mode, "bind", res.Bind,
		"url", res.URL, "secure_context", res.SecureContext)

	auth, err := serve.NewAuth(dir, log, cfg.AllowedOrigins())
	if err != nil {
		return err
	}
	if err := auth.SetDeadline(rec.ExpiresAt); err != nil {
		return err
	}
	if _, _, err := auth.EnsureServerToken(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	st := core.NewStore(log)
	st.SetScope(scope)
	go st.Run(ctx)
	hub := core.NewHub(st, log)
	go hub.Run(ctx)

	if _, err := updateShare(id, func(r *shareRecord) { r.PID = os.Getpid() }); err != nil {
		log.Warn("could not record pid", "err", err)
	}

	// Bring the exposure up, then publish the URL. In cloudflare mode
	// procTunnel only reports the URL once https://<domain>/healthz actually
	// answers (C2 step 7); in LAN mode there is nothing to start — the
	// listener IS the exposure — and Start says so and returns.
	go func() {
		sctx, scancel := context.WithTimeout(ctx, 2*time.Minute)
		defer scancel()
		if _, err := mgr.Start(sctx); err != nil {
			log.Warn("share tunnel did not start", "err", err)
			_, _ = updateShare(id, func(r *shareRecord) {
				r.State, r.Error = "failed", err.Error()
			})
			cancel()
			return
		}
		url := mgr.URL()
		_, _ = updateShare(id, func(r *shareRecord) {
			r.State, r.URL, r.Error = "ready", url, ""
			// A LAN share's URL is re-resolved on every read (the lease can
			// move), so what is stored here is only the last known value.
		})
		log.Info("share is live", "url", url, "scope", scope.String(), "expires_at", rec.ExpiresAt)
	}()

	// G5.1: the in-process timer. It re-reads share.json so `share extend`
	// moves the deadline under a running instance, and it moves the auth
	// deadline with it so tokens never expire underneath a live share.
	go func() {
		deadline := rec.ExpiresAt
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(deadline)):
				if cur, err := readShareFile(dir); err == nil && cur.ExpiresAt.After(deadline) {
					deadline = cur.ExpiresAt
					continue
				}
				log.Info("share expired; destroying")
				cancel()
				return
			case <-t.C:
				cur, err := readShareFile(dir)
				if err != nil || cur.ExpiresAt.Equal(deadline) {
					continue
				}
				deadline = cur.ExpiresAt
				_ = auth.SetDeadline(deadline)
				log.Info("share deadline moved", "expires_at", deadline)
			}
		}
	}()

	srv, err := serve.New(serve.Options{
		Version:  version,
		Config:   cfg,
		Hub:      hub,
		Auth:     auth,
		Log:      log,
		Static:   webFS(),
		Exposure: exposeAdapter{mgr},
	})
	if err != nil {
		cancel()
		return err
	}
	serveErr := srv.Serve(ctx)

	// Whatever brought us here — expiry, SIGTERM, a failed start — the share
	// destroys itself: cloudflared down, DNS record deleted, tunnel deleted,
	// state dir and its tokens wiped (G4).
	log.Info("tearing down share")
	// The supervised cloudflared runs under a context deliberately detached
	// from ours (it must survive a transient parent cancel), so stopping it is
	// an explicit act — and it has to happen before the tunnel can be deleted.
	_ = mgr.Stop()
	if cur, err := readShareFile(dir); err == nil {
		res := revokeShareRecord(cur)
		log.Info("share torn down", "dns_gone", res.DNSGone, "tunnel_gone", res.TunnelGone,
			"state_wiped", res.StateWiped, "err", res.Error)
	} else {
		_ = mgr.Close()
	}
	return serveErr
}

func qualify(session string, panes []string) []string {
	out := make([]string, 0, len(panes))
	for _, p := range panes {
		out = append(out, core.JoinTarget(session, p))
	}
	return out
}

// --- panic ------------------------------------------------------------------

// haltFilePath is the main daemon's exposure kill switch. It is resolved from
// the state dir BY CONSTRUCTION and the running daemon polls for it, so — unlike
// a typed STOP path — there is no way to write it somewhere nothing reads.
func haltFilePath(state string) string { return filepath.Join(state, "EXPOSE-HALT") }

// cmdPanic is the everything switch: every share revoked and destroyed, plus
// the main tunnel stopped. After it returns, nothing of this tool is reachable
// from the internet — only the loopback listener remains.
//
// It deliberately does NOT delete the permanent DNS record or tunnel: the
// static-domain rule (C1) still holds for the permanent deployment, which is
// stopped, not destroyed.
func cmdPanic(args []string) error {
	asJSON := hasFlag(args, "--json")
	state, err := StateDir()
	if err != nil {
		return err
	}

	recs, _ := listShareRecords()
	results := make([]teardownResult, 0, len(recs))
	failed := 0
	for _, rec := range recs {
		res := revokeShareRecord(rec)
		if !res.ok() {
			failed++
		}
		results = append(results, res)
	}

	// The main tunnel: raise the halt flag FIRST so the daemon's supervisor
	// does not simply respawn cloudflared, then kill what is running.
	halt := haltFilePath(state)
	haltErr := os.WriteFile(halt, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
	mainConfig := filepath.Join(state, "cloudflare", "config.yml")
	killStrayCloudflared(mainConfig)
	mainStopped := waitForNoCloudflared(mainConfig, 20*time.Second)
	if !mainStopped {
		failed++
	}

	if asJSON {
		printJSON(map[string]any{
			"shares_revoked": results,
			"main_tunnel": map[string]any{
				"stopped":   mainStopped,
				"halt_file": halt,
				"note":      "stopped, not destroyed: the permanent DNS record and tunnel are left in place",
			},
			"ok": failed == 0 && haltErr == nil,
		})
	} else {
		fmt.Println("PANIC — tearing down every exposure")
		for _, r := range results {
			printTeardown(r)
		}
		if len(results) == 0 {
			fmt.Println("  no shares were running")
		}
		fmt.Printf("main tunnel\n  halt flag    %s\n", halt)
		if mainStopped {
			fmt.Println("  cloudflared  stopped (DNS record and tunnel LEFT IN PLACE — static domain)")
		} else {
			fmt.Println("  cloudflared  STILL RUNNING — check `herdr-expose status`")
		}
		fmt.Println("\nre-arm with: herdr-expose expose start")
	}
	if haltErr != nil {
		return haltErr
	}
	if failed > 0 {
		return fmt.Errorf("%d component(s) did NOT tear down", failed)
	}
	return nil
}

// waitForNoCloudflared verifies, rather than assumes, that no cloudflared is
// still running against a generated config.
func waitForNoCloudflared(configPath string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if countCloudflared(configPath) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			killStrayCloudflared(configPath)
			return countCloudflared(configPath) == 0
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func countCloudflared(configPath string) int {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, configPath) && strings.Contains(line, "cloudflared") {
			n++
		}
	}
	return n
}
