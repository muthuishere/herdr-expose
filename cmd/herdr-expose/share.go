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
	Mode          string `json:"mode"` // local | lan | quick | cloudflare
	Bind          string `json:"bind"`
	SecureContext bool   `json:"secure_context"`
	FellBack      string `json:"fell_back,omitempty"`

	// Provider is WHICH implementation serves this share's rung. Today the
	// only one a share can be created with is "cloudflare"; the field stays
	// because the rung and the provider are independent — Mode says how far
	// this reaches, Provider says who carries it — and because teardown needs
	// BOTH: only a cloudflare DOMAIN share creates anything that has to be
	// deleted. It also keeps records written by older versions (the built-in
	// ngrok provider, removed in AMENDMENTS 18 / ADR 0034) readable, so that
	// `share list` and `share revoke` can still see and wipe them.
	Provider string `json:"provider,omitempty"`

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
	ID   string `json:"id"`
	Mode string `json:"mode"`
	// Provider is which implementation carries the rung. "cloudflare" for
	// anything this version creates; an older value is reported as written.
	Provider string `json:"provider"`
	Scope    string `json:"scope"`
	Session  string `json:"session"`
	Domain   string `json:"domain"`
	URL      string `json:"url"`
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
		ID: r.ID, Mode: r.mode(), Provider: r.provider(), Scope: r.Scope, Session: r.Session, Domain: r.Domain,
		URL: r.CurrentURL(), SecureContext: r.SecureContext, FellBack: r.FellBack,
		State: r.State, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
		Remaining: left.Round(time.Second).String(), Seconds: int64(left.Seconds()),
		PID: r.PID, Alive: pidAlive(r.PID), Expired: time.Now().After(r.ExpiresAt),
		Error: r.Error,
	}
}

// mode defaults an old record (written before LAN shares existed) to
// cloudflare. That is the one place in this file where the ABSENCE of an
// answer means the top rung rather than the bottom, and it is correct: this is
// read by teardown, and a record from before modes existed really did have a
// DNS record and a named tunnel behind it. Guessing "local" there would leave
// a real hostname resolving with nothing to clean it up.
func (r *shareRecord) mode() string {
	if r.Mode == "" {
		return string(config.ModeCloudflare)
	}
	return r.Mode
}

// rung places this share on the exposure ladder, for the lines that have to
// state its reach plainly.
func (r *shareRecord) rung() config.Rung { return config.RungOf(config.Mode(r.mode())) }

// isLocal reports the bottom rung (AMENDMENTS 16 L1): a loopback-only share.
// Nothing off this machine can reach it, no tunnel process exists, and — like
// LAN and quick — there is nothing in anybody's Cloudflare account to tear
// down. It is still scoped, still pairing-gated and still time-boxed: the rung
// decides REACH, never the other guarantees.
func (r *shareRecord) isLocal() bool { return r.mode() == string(config.ModeLocal) }

// isLAN reports whether this share has no tunnel and no DNS to tear down.
func (r *shareRecord) isLAN() bool { return r.mode() == string(config.ModeLAN) }

// isQuick reports a TryCloudflare share (AMENDMENTS 15): a public https URL
// with nothing behind it in anybody's Cloudflare account — no zone, no DNS
// record, no named tunnel, no credentials file. Teardown is therefore the same
// shape as LAN's (stop the process, free the port, wipe the state) even though
// the share was on the public internet.
func (r *shareRecord) isQuick() bool { return r.mode() == string(config.ModeQuick) }

// hasCloudflareResources reports whether this share created anything in the
// Cloudflare account that teardown has to delete. Only a named-tunnel share on
// a real zone does.
func (r *shareRecord) hasCloudflareResources() bool {
	if r.isLocal() || r.isLAN() || r.isQuick() {
		return false
	}
	// A share carried by anything other than the built-in Cloudflare provider
	// created nothing in anybody's Cloudflare account, so there is nothing
	// here to delete — teardown is "stop the process", exactly as that
	// provider's Footprint declares. The test is POSITIVE (is it cloudflare?)
	// rather than a list of what it is not, so a record naming a provider this
	// build has never heard of is never handed a Cloudflare teardown.
	return r.provider() == "cloudflare"
}

// provider names the implementation behind this share. An empty field means a
// record written before providers were interchangeable, and those were all
// cloudflare.
func (r *shareRecord) provider() string {
	if p := strings.TrimSpace(r.Provider); p != "" {
		return p
	}
	return "cloudflare"
}

// CurrentURL re-resolves a LAN share's address every time it is asked for.
//
// The LAN IP moves under a DHCP lease, and a URL that was true at mint time is
// worse than no URL — the daemon already re-resolves the main listener every
// 30s, so `share list` does the same rather than printing history.
func (r *shareRecord) CurrentURL() string {
	if r.isLocal() {
		return fmt.Sprintf("http://127.0.0.1:%d", r.Port)
	}
	if !r.isLAN() {
		// A quick share's hostname is whatever the edge assigned this run; the
		// instance writes it back to share.json as soon as it verifies, so the
		// record IS the current truth for both tunnel modes.
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

// --- the share's run lock ---------------------------------------------------
//
// `share restore` (and the daemon's startup reconciliation) respawns a share
// whose process is gone. Run it twice in the second before the child has
// written its new pid and you get TWO instances for one share id: two
// processes racing for one port, two teardowns, two tunnels. pidAlive cannot
// close that window — the pid in share.json is stale by definition at exactly
// the moment it matters, and pids are reused.
//
// An advisory flock held by the child for its whole life answers the question
// without the race and without trusting anything on disk: the kernel drops it
// when the process dies, however it dies, so "is this share running?" is
// decided by the same primitive that decides who is allowed to run it. The
// second spawn takes the lock, fails, and exits — which is what makes a double
// restore converge on ONE instance rather than compound into two.

// tryShareRunLock takes the share's run lock, or reports that someone holds it.
func tryShareRunLock(dir string) (*os.File, bool) {
	f, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

// shareIsRunning reports whether a live instance holds this share's run lock.
// It takes the lock and immediately drops it, so it is a read, not a claim.
func shareIsRunning(dir string) bool {
	f, ok := tryShareRunLock(dir)
	if !ok {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
	return false
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

  share [--local | --lan | --quick | --domain X]
        [--session NAME] [--pane TARGET] [--hours N] [--days N] [--json]
        expose ONE herdr session (default: this one) for N hours (default 1)

        FOUR RUNGS. Every rung above the default is an explicit request.
          (none)      http://<lan-ip>:<port>   — this machine and this
                      network. THE DEFAULT: a share exists to be opened from
                      somewhere else, and this goes no further than the wifi.
          --lan       the same, said out loud (overrides [share] default_mode)
          --quick     https://<random>.trycloudflare.com — anyone on the
                      internet; no account, no zone, no DNS, no API token,
                      nothing to clean up
          --domain X  https://X — the internet, on a name you own
          --local     http://127.0.0.1:<port>  — this machine only; an opt-IN
                      for testing, not reachable by anybody you send it to

        A failed EXPLICIT request degrades LOUDLY and says why (--quick with no
        cloudflared installed falls back to --lan). Nothing ever escalates
        above what you asked for: a bare 'share' never becomes a tunnel just
        because [expose] has a domain configured.

        Cloudflare carries every tunnel rung. Any other transport — ngrok,
        tailscale, a corporate proxy — is a JS adapter configured under
        [expose] (see adapters/template.js); there is no provider flag,
        because there is no second built-in to choose between.
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
	// The four rungs of AMENDMENTS 16 L1. At most ONE may be set, and when
	// none is, the share is LAN: this machine and this network, and not one
	// hop further. Every rung above that is an explicit request, and `--local`
	// is the explicit way DOWN, for testing.
	local   bool
	lan     bool
	quick   bool
	domain  string
	session string
	panes   []string
	hours   float64
	days    float64
	name    string
	asJSON  bool

	// hoursSet records that --hours was passed explicitly, so that the
	// [share] default does not overwrite it.
	hoursSet bool
}

func parseShareFlags(args []string) (shareFlags, error) {
	f := shareFlags{name: "share"}
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
				f.hoursSet = true
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
		case "--local":
			f.local = true
		case "--lan":
			f.lan = true
		case "--quick":
			f.quick = true
		case "--provider":
			// Removed with the second built-in provider (AMENDMENTS 18 /
			// ADR 0034). A one-value flag that pretends to be a choice is
			// worse than no flag, and silently accepting `--provider ngrok`
			// would hand back a Cloudflare tunnel under an ngrok name. Say
			// what happened and where the transport went instead.
			_, _ = next()
			err = errors.New("--provider was removed: Cloudflare is the only built-in transport, " +
				"so the rung flags (--lan / --quick / --domain / --local) are the whole choice. " +
				"For ngrok, tailscale or anything else, write a JS adapter and select it with " +
				"`adapter = \"<id>\"` under [expose] — see adapters/template.js")
		case "--json":
			f.asJSON = true
		default:
			err = fmt.Errorf("unknown flag %q", args[i])
		}
		if err != nil {
			return f, err
		}
	}
	if !f.hoursSet && f.days == 0 {
		f.hours = 1 // G1 default
	}
	return f, nil
}

// namedARung reports whether the command line asked for a specific rung.
// `--local` counts: saying "local" out loud is how somebody with a different
// `[share] default_mode` gets back to loopback for one share.
func (f shareFlags) namedARung() bool {
	return f.local || f.lan || f.quick || strings.TrimSpace(f.domain) != ""
}

// rung is the exposure the user ASKED for — the ceiling for this share.
// Resolution may land below it (a loud, explained fallback) and may never land
// above it (AMENDMENTS 16 L2).
func (f shareFlags) rung() config.Rung {
	switch {
	case strings.TrimSpace(f.domain) != "":
		return config.RungDomain
	case f.quick:
		return config.RungQuick
	case f.local:
		return config.RungLocal
	default:
		// Including `--lan`, and including no flag at all. The two are the
		// same request; the flag only exists so it can be said explicitly.
		return config.RungLAN
	}
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

// applyConfigDefaults fills the flags the user did not pass from the [share]
// table, and enforces max_concurrent.
//
// The hours default lives here rather than in parseShareFlags so that the
// parser stays a pure function of its arguments and remains testable without a
// config file on disk.
func (f *shareFlags) applyConfigDefaults() error {
	cfg, err := config.Load()
	if err != nil {
		// A share must still work when the config is unreadable; the built-in
		// one-hour default already applied in parseShareFlags.
		return nil
	}
	if !f.hoursSet && f.days == 0 && cfg.Share.DefaultHours > 0 {
		f.hours = cfg.Share.DefaultHours
	}
	// The rung. AMENDMENTS 16 L1: a transport flag is an explicit request and
	// the config never overrides one, so this whole block is skipped the
	// moment the person named a rung on the command line.
	//
	// When they did NOT, `[share] default_mode` decides — and it ships as
	// "lan". It is deliberately the ONLY config key that can move a bare
	// `share` off the LAN: the withdrawn "auto" ladder used to consult
	// `[expose] domain`, which meant an unrelated key set months ago for the
	// permanent deployment silently put a session on the public internet.
	// Setting default_mode is asking, in the same sense that typing --quick is
	// asking; reading someone else's domain key is guessing.
	if !f.namedARung() {
		switch config.ShareRung(cfg.Share.DefaultMode) {
		case config.RungLAN:
			f.lan = true
		case config.RungQuick:
			f.quick = true
		case config.RungLocal:
			f.local = true
		case config.RungDomain:
			// `share --name review` with a configured suffix means
			// review.<suffix>. Only reachable from default_mode = "domain":
			// the suffix is a NAMING convention, and on its own it must never
			// be what promotes a share to the public internet.
			if suffix := strings.TrimSpace(cfg.Share.DomainSuffix); suffix != "" && f.name != "" && f.name != "share" {
				f.domain = f.name + "." + suffix
			} else if d := strings.TrimSpace(cfg.Expose.Domain); d != "" {
				f.domain = d
			} else {
				return errors.New(`[share] default_mode = "domain" but no hostname is configured: ` +
					`set [share] domain_suffix (and pass --name) or [expose] domain, or pass --domain X`)
			}
		default:
			f.lan = true
		}
	}
	if n := cfg.Share.MaxConcurrent; n > 0 {
		if recs, err := listShareRecords(); err == nil && len(recs) >= n {
			return fmt.Errorf("%d shares are already live and share.max_concurrent is %d; "+
				"revoke one with `herdr-expose share revoke <id>` or raise the limit under [share]",
				len(recs), n)
		}
	}
	return nil
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

// shareBinaryFound is the helper-binary probe expose.Resolve uses. It is a
// package var ONLY so a test can say "pretend cloudflared is not installed"
// and watch an explicit --quick degrade to lan instead of climbing anywhere.
// nil means the real check.
var shareBinaryFound func(string) bool

// resolveShareMode turns the requested rung into a Resolution, through
// expose.Resolve — the same resolver the main daemon uses (E1). There is
// deliberately no second set of rules here: this function only chooses which
// [expose] table to hand it, and explains the choice.
//
// AMENDMENTS 16 L1 — THE LADDER IS CLIMBED, NEVER GUESSED:
//
//	no flag / --lan  ->  lan     http://<lan-ip>:<port>  this machine + this network
//	--quick          ->  quick   https://<rand>.trycloudflare.com
//	--domain X       ->  domain  https://X
//	--local          ->  local   127.0.0.1:<port>        explicit opt-IN, for testing
//
// LAN is the default because a SHARE exists to be reached from somewhere else.
// Sitting at this machine you would just open the daemon on localhost:21118,
// so a loopback-only share is the one rung that makes the feature pointless —
// and binding 0.0.0.0 covers loopback and the network in one. (The daemon's
// own [expose] default stays LOCAL, and that asymmetry is deliberate: same
// principle, least exposure that still does the job, different job.)
//
// There is no auto branch. The withdrawn AMENDMENTS 15 ladder resolved a bare
// `share` as domain -> quick -> lan by consulting `[expose] domain`, which
// meant the tool could publish a session to the public INTERNET because a
// config key happened to be set for something else entirely. This binary runs
// arbitrary commands inside the owner's agent sessions; the blast radius of
// each rung differs by orders of magnitude — this machine, this room, the
// entire internet — and the step from "this room" to "the entire internet"
// must be a flag the person typed.
//
// L2 — FALLBACK ONLY EVER GOES DOWN. An explicit --quick or --domain with no
// cloudflared resolvable degrades to lan with a printed reason, because the
// person DID ask to be reachable and a LAN address is the nearest honest
// answer. Nothing may move the other way, and the guard at the bottom of this
// function enforces that by comparing rungs rather than trusting the branches.
func resolveShareMode(ctx context.Context, f shareFlags, port int, tunnelName string) (expose.Resolution, config.Expose, error) {
	res, e, err := resolveShareModeUnchecked(ctx, f, port, tunnelName)
	if err != nil {
		return expose.Resolution{}, config.Expose{}, err
	}
	// The invariant, checked rather than assumed: whatever the branches above
	// decided, this share does not reach further than what was asked for. A
	// future edit that reintroduces an auto-escalation fails HERE, loudly, at
	// the one place every path passes through, instead of quietly publishing a
	// session.
	if got, want := config.RungOf(res.Mode), f.rung(); got > want {
		return expose.Resolution{}, config.Expose{}, fmt.Errorf(
			"refusing to expose further than you asked: requested %s (%s), resolved %s (%s). "+
				"This is a bug in herdr-expose, not something you did — exposure never escalates (AMENDMENTS 16 L2)",
			want, want.Reach(), got, got.Reach())
	}
	return res, e, nil
}

func resolveShareModeUnchecked(ctx context.Context, f shareFlags, port int, tunnelName string) (expose.Resolution, config.Expose, error) {
	var none config.Expose
	// Four rungs, exactly one of them.
	named := 0
	for _, on := range []bool{f.local, f.lan, f.quick, strings.TrimSpace(f.domain) != ""} {
		if on {
			named++
		}
	}
	if named > 1 {
		return expose.Resolution{}, none, errors.New(
			"--local, --lan, --quick and --domain are mutually exclusive: pick loopback, " +
				"a LAN address, a throwaway *.trycloudflare.com hostname, or a hostname on your own zone")
	}

	// lan: fall back to a LAN address for an explicit tunnel request that
	// cannot be honoured. Never reached from a bare `share` — that is the whole
	// asymmetry of L2.
	lanFallback := func(why string) (expose.Resolution, config.Expose, error) {
		lan := config.Expose{LAN: true}
		r := expose.Resolve(lan, port, shareBinaryFound)
		r.FellBack = why
		return r, lan, nil
	}

	switch {
	// RUNG 1 — LOCAL, by explicit `--local` only. The zero Expose is local by
	// construction, so this branch hands over a table with nothing switched
	// on: no tunnel process, no DNS, no 0.0.0.0 bind. Kept as an opt-IN for
	// testing; it is not the default, because a share nobody else can open is
	// not a share. On loopback the F1 local-mode bypass applies — see
	// internal/serve/localmode.go, where Origin allowlisting and Host pinning
	// replace the device token rather than removing it.
	case f.local:
		e := config.Expose{}
		return expose.Resolve(e, port, shareBinaryFound), e, nil

	// RUNG 2 — LAN. The DEFAULT, and the case for a bare `share`. Cannot fail,
	// and must never be "upgraded": a person who asked for the wifi — or who
	// asked for nothing at all — did not ask for the internet. Binding
	// 0.0.0.0 covers loopback too, so the owner's own browser still works.
	case f.lan || (!f.quick && strings.TrimSpace(f.domain) == ""):
		e := config.Expose{LAN: true}
		return expose.Resolve(e, port, shareBinaryFound), e, nil

	// RUNG 3 — QUICK. No token is read, no zone is looked up, no DNS call is
	// made; the ONLY precondition is cloudflared. Missing it degrades to lan
	// with the reason printed, because the ask was "make this reachable".
	case f.quick:
		// The ephemeral rung: it creates nothing in any account, so the only
		// precondition is the binary that carries it; a missing one degrades
		// to lan with the reason printed.
		e := config.Expose{Quick: true}
		res := expose.Resolve(e, port, shareBinaryFound)
		if res.Mode != config.ModeQuick {
			return lanFallback("you asked for a quick tunnel, but " + res.FellBack)
		}
		return res, e, nil
	}

	// RUNG 4 — DOMAIN. cloudflared first (a missing binary is an environment
	// problem and degrades to lan), then the zone. A hostname that belongs to
	// something else is a mistake in the ARGUMENT, so it is a hard error with
	// the fix in it: silently putting that share on the wifi instead would not
	// be what the person meant by `--domain`.
	d := strings.TrimSpace(f.domain)

	e := config.Expose{Cloudflare: true, Domain: d, TunnelName: tunnelName}
	res := expose.Resolve(e, port, shareBinaryFound)
	if res.Mode != config.ModeCloudflare {
		return lanFallback(fmt.Sprintf("you asked to share on %s, but %s", d, res.FellBack))
	}
	if _, rec, err := expose.CheckZoneAccess(ctx, d); err != nil {
		return expose.Resolution{}, none, err
	} else if rec.Exists && rec.Comment != expose.ShareDNSComment {
		return expose.Resolution{}, none, fmt.Errorf("%s already has a DNS record tagged %q — a share only ever creates and deletes "+
			"records tagged %q, so it will not take this hostname over. Pick another hostname",
			d, rec.Comment, expose.ShareDNSComment)
	}
	return res, e, nil
}

// hostnameOwner reports the id of a LIVE share already holding this hostname.
//
// THE `share --domain X` TWICE RULE, decided and enforced here: REUSE NOTHING,
// REFUSE. Two shares on one hostname is the worst of the three options — the
// second PATCHes the DNS record onto its own tunnel, silently breaking the
// first, and then whichever is revoked first deletes the record out from under
// the other. Creating a second share on a different hostname is not what the
// person typed. So the answer is a refusal that names the share in the way and
// the two commands that resolve it (`share extend` to keep it, `share revoke`
// to replace it), which is deterministic and loses nothing.
//
// An ORPHANED record — tagged as ours but with no live share record behind it —
// is not a collision: it is the residue of a crash, and the upsert adopts it,
// which is what makes an interrupted `share --domain` converge on a re-run
// instead of blocking that hostname forever.
func hostnameOwner(domain, exceptID string) string {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if domain == "" {
		return ""
	}
	recs, err := listShareRecords()
	if err != nil {
		return ""
	}
	for _, r := range recs {
		if r.ID == exceptID || !strings.EqualFold(strings.TrimSpace(r.Domain), domain) {
			continue
		}
		if time.Now().After(r.ExpiresAt) {
			continue // already due for reaping; the sweep will take it
		}
		return r.ID
	}
	return ""
}

func cmdShareCreate(args []string) (err error) {
	f, err := parseShareFlags(args)
	if err != nil {
		return err
	}
	// [share] supplies the defaults for anything not passed on the flags.
	// They are DEFAULTS, never an escape from time-boxing: every path still
	// goes through ttl(), which refuses a non-positive or unbounded lifetime.
	if err := f.applyConfigDefaults(); err != nil {
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

	// `share --domain X` twice: REFUSE, before a port is taken or a directory
	// is made. See hostnameOwner for why refusing beats reusing or creating a
	// second one. This runs before ANY resource exists, so the refusal leaves
	// the machine exactly as it found it.
	if d := strings.TrimSpace(f.domain); d != "" {
		if owner := hostnameOwner(d, ""); owner != "" {
			return fmt.Errorf("share %s is already live on %s. Two shares on one hostname would fight over "+
				"the same DNS record and the first revoke would break the other, so this is refused rather "+
				"than guessed at.\n  keep it:    herdr-expose share extend %s --hours 1\n"+
				"  replace it: herdr-expose share revoke %s   (then run this again)\n"+
				"  or share on a different hostname with --domain", owner, d, owner, owner)
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
		// ONE clear line naming the rung actually chosen and the reason. A
		// fallback only ever goes DOWN (L2), so this line always reports LESS
		// reach than was asked for — never more — and the owner never has to
		// guess which rung he got.
		fmt.Fprintf(os.Stderr, "\n  NOTE: falling back to %s (%s) — %s\n\n",
			shareTransportName(res.Mode), res.Reach(), res.FellBack)
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
		ID: id, Mode: string(res.Mode), Provider: "cloudflare", Bind: res.Bind,
		SecureContext: res.SecureContext, FellBack: res.FellBack,
		Port: port, Session: f.session, Panes: scope.Panes, Scope: scope.String(),
		CreatedAt: now, ExpiresAt: now.Add(ttl), State: "starting",
	}
	if res.Mode == config.ModeCloudflare {
		share.Domain, share.TunnelName = strings.TrimSuffix(strings.TrimPrefix(res.URL, "https://"), "/"), shareTunnelName(id)
	} else {
		// A fallback landed us below the rung that uses a hostname; make sure
		// no stale domain survives into the record and into teardown.
		share.Domain, share.TunnelName = "", ""
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

	audit, auditClose := auditLogger()
	if auditClose != nil {
		defer auditClose.Close()
	}
	audit.Info("share created", "share", id, "session", f.session, "scope", scope.String(),
		"mode", string(res.Mode), "domain", share.Domain, "port", port,
		"expires_at", share.ExpiresAt, "ttl", ttl.String(), "log", shareLogPath(dir))
	_ = codeExp

	if err := spawnShare(id, dir); err != nil {
		audit.Warn("share failed to spawn", "share", id, "err", err)
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
		audit.Warn("share failed to start, tearing down", "share", id, "err", final.Error)
		_ = revokeShare(id)
		return fmt.Errorf("share %s failed to start: %s", id, final.Error)
	}
	audit.Info("share live", "share", id, "url", final.CurrentURL(), "expires_at", final.ExpiresAt)

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
	// The RUNG and its reach, stated plainly, at the top of the block. Which
	// of the four ladders this share is on is the single most consequential
	// fact about it, so it is never left to be inferred from the URL.
	fmt.Printf("\n  url:           %s\n  exposure:      %s via %s — %s\n  pairing code:  %s  (valid until %s)\n",
		url, final.mode(), final.provider(), final.rung().Reach(), code, codeExp.Format(time.Kitchen))
	fmt.Printf("  scope:         %s  (nothing else on this machine is in this instance's tree)\n", scope.String())
	switch {
	case final.isLocal():
		fmt.Printf("  expires:       %s  (in %s) — the instance exits and its state is wiped then\n",
			final.ExpiresAt.Format(time.RFC1123), time.Until(final.ExpiresAt).Round(time.Minute))
		fmt.Printf("  reachable by:  nothing off this machine. No tunnel was started, no DNS record\n" +
			"                 exists, and the port is bound to 127.0.0.1 — not to the network.\n")
		fmt.Printf("  still guarded: a web page you visit CANNOT drive this. Local mode swaps the\n" +
			"                 device token for a strict Origin allowlist and Host pinning (F1),\n" +
			"                 both enforced here exactly as on any other rung.\n")
		fmt.Printf("  note:          --local is an opt-IN for testing, so this link works for NOBODY\n" +
			"                 you send it to. Drop the flag for the LAN default, or:\n" +
			"                 herdr-expose share --quick   (anyone on the internet)\n" +
			"                 herdr-expose share --domain X\n")
	case final.isLAN():
		fmt.Printf("  expires:       %s  (in %s) — the instance exits and its state is wiped then\n",
			final.ExpiresAt.Format(time.RFC1123), time.Until(final.ExpiresAt).Round(time.Minute))
		fmt.Printf("  reachable by:  anyone on this network who ALSO has the pairing code above.\n" +
			"                 NOT the internet: no tunnel was started and no DNS record exists.\n")
		fmt.Printf("  not installable: plain HTTP on a LAN IP is not a secure context, so no PWA install\n" +
			"                   and no service worker (that is the mode, not a bug)\n")
		fmt.Printf("  note:          this QR goes stale if the DHCP lease changes the LAN IP —\n"+
			"                 `herdr-expose share pair %s` re-issues one at the current address\n", id)
	case final.isQuick():
		fmt.Printf("  expires:       %s  (in %s) — the process is stopped and the state wiped then;\n"+
			"                 the tunnel dies with it and there is nothing to delete in Cloudflare\n",
			final.ExpiresAt.Format(time.RFC1123), time.Until(final.ExpiresAt).Round(time.Minute))
		fmt.Printf("  reachable by:  anyone on the internet who ALSO has the pairing code above —\n" +
			"                 a quick tunnel is public, so it is no more trusted than a domain share\n")
		// AMENDMENTS 15 K3: say the trade-off ONCE, here, rather than letting
		// it be discovered by re-pairing.
		fmt.Printf("  trade-off:     this hostname is NEW every time. Device tokens are origin-bound,\n" +
			"                 so nothing you paired before carries over and the next quick share\n" +
			"                 needs a fresh scan. Fine for a throwaway, wrong for a daily driver —\n" +
			"                 which is why [expose] keeps a static domain.\n")
	default:
		fmt.Printf("  expires:       %s  (in %s) — DNS record and tunnel are deleted then\n",
			final.ExpiresAt.Format(time.RFC1123), time.Until(final.ExpiresAt).Round(time.Minute))
	}
	fmt.Printf("  share id:      %s   (herdr-expose share extend %s --hours 1 | share revoke %s)\n\n",
		id, id, id)
	return nil
}

// shareTransportName names a resolved mode the way the four flags do, so the
// one-line explanation reads as an answer to "which rung did I get?".
func shareTransportName(m config.Mode) string {
	switch m {
	case config.ModeLocal:
		return "this machine only (--local)"
	case config.ModeQuick:
		return "a quick tunnel (--quick)"
	case config.ModeLAN:
		return "this network (--lan)"
	case config.ModeCloudflare:
		return "your own domain (--domain)"
	default:
		return string(m)
	}
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
	rec, err := loadShare(id)
	switch {
	case err == nil && rec.isLAN():
		fmt.Fprintf(os.Stderr, "  starting the scoped instance on %s…\n", rec.CurrentURL())
	case err == nil && rec.isQuick():
		fmt.Fprintf(os.Stderr, "  asking cloudflare for a throwaway hostname and waiting for it to answer…\n")
	default:
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
	// A quick tunnel's hostname is four dictionary words: the URL column is
	// wide enough for one, or the one thing you need to copy wraps.
	fmt.Printf("%-10s %-11s %-26s %-52s %-9s %-7s %s\n",
		"ID", "MODE", "SCOPE", "URL", "STATE", "PID", "EXPIRES IN")
	for _, r := range recs {
		v := r.view()
		state := v.State
		if !v.Alive {
			state += "(dead)"
		}
		fmt.Printf("%-10s %-11s %-26s %-52s %-9s %-7d %s\n",
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
	// `extend` is CUMULATIVE by definition — that is what the verb means, and
	// AMENDMENTS 10 requires it to be an explicit, logged, revocable act. So
	// running it twice deliberately extends twice; the idempotency that
	// matters here is a different one: no number of extensions may add up to a
	// share that is effectively permanent. The ceiling is the same one ttl()
	// enforces on a single call, applied to the RESULT.
	const maxLifetime = 365 * 24 * time.Hour
	var clamped bool
	rec, err := updateShare(id, func(r *shareRecord) {
		base := time.Now()
		if r.ExpiresAt.After(base) {
			base = r.ExpiresAt
		}
		want := base.Add(d)
		if ceiling := time.Now().Add(maxLifetime); want.After(ceiling) {
			want, clamped = ceiling, true
		}
		r.ExpiresAt = want
	})
	if err != nil {
		return err
	}
	if clamped {
		fmt.Fprintf(os.Stderr, "  NOTE: clamped to a year out — every share is time-boxed and there is no "+
			"permanent one (AMENDMENTS 10)\n")
	}
	// The tokens must move WITH the share, or they expire underneath a share
	// that is still running. The live instance re-reads share.json and does the
	// same thing; doing it here too makes `extend` correct even if the
	// instance is momentarily wedged.
	dir, _ := shareDirFor(id)
	if auth, err := serve.NewAuth(dir, newLogger(), nil); err == nil {
		_ = auth.SetDeadline(rec.ExpiresAt)
	}
	// Extending is an explicit, LOGGED, revocable act (AMENDMENTS 10).
	if audit, closer := auditLogger(); audit != nil {
		audit.Info("share extended", "share", id, "expires_at", rec.ExpiresAt)
		if closer != nil {
			closer.Close()
		}
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
	Provider    string `json:"provider"`
	Domain      string `json:"domain"`
	ProcessGone bool   `json:"process_gone"`
	// DNSGone / TunnelGone are true when there is nothing left in Cloudflare.
	// For a LAN or quick share that is true BY CONSTRUCTION — nothing was ever
	// created — which CloudflareNA says explicitly, so a reader is not left
	// wondering whether a delete silently succeeded.
	DNSGone      bool   `json:"dns_gone"`
	TunnelGone   bool   `json:"tunnel_gone"`
	CloudflareNA bool   `json:"cloudflare_not_applicable,omitempty"`
	PortFree     bool   `json:"port_free"`
	StateWiped   bool   `json:"state_wiped"`
	Error        string `json:"error,omitempty"`
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
			// The record is THERE but unreadable — a share.json truncated by a
			// crash mid-write, say. Refusing here would make that share
			// unrevocable forever, which is the opposite of what `revoke` is
			// for, so the directory is wiped instead. There is nothing else to
			// go on: with no parseable record there is no tunnel name and no
			// hostname to address, and anything this tool DID create carries
			// its own tag and is reachable from `share revoke --all` on the
			// records that survived.
			dir, derr := shareDirFor(id)
			if derr != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "share %s: its record is unreadable (%v); wiping the state dir\n", id, err)
			if rerr := os.RemoveAll(dir); rerr != nil {
				return rerr
			}
			if asJSON {
				printJSON(map[string]any{"revoked": []teardownResult{{ID: id, StateWiped: true}}, "ok": true})
			}
			return nil
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
	if r.Domain != "" {
		fmt.Printf("share %s (%s, %s)\n", r.ID, r.Mode, r.Domain)
	} else {
		fmt.Printf("share %s (%s)\n", r.ID, r.Mode)
	}
	fmt.Printf("  process      %s\n", mark(r.ProcessGone))
	switch {
	case r.Mode == string(config.ModeLocal):
		fmt.Printf("  dns/tunnel   n/a (local share: it never left this machine)\n")
	case r.Mode == string(config.ModeLAN):
		fmt.Printf("  dns/tunnel   n/a (LAN share: nothing was ever created)\n")
	case r.Mode == string(config.ModeQuick):
		fmt.Printf("  dns/tunnel   n/a (quick tunnel: no DNS record and no named tunnel ever existed)\n")
	case r.CloudflareNA:
		fmt.Printf("  dns/tunnel   n/a (%s created nothing in any account: a reserved domain\n"+
			"               belongs to you, not to this tool, so it is never deleted)\n", r.Provider)
	default:
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
	if audit, closer := auditLogger(); audit != nil {
		audit.Info("share revoked", "share", rec.ID, "mode", rec.mode(),
			"domain", rec.Domain, "expires_at", rec.ExpiresAt,
			"expired", time.Now().After(rec.ExpiresAt))
		if closer != nil {
			defer closer.Close()
		}
	}

	res := teardownResult{ID: rec.ID, Mode: rec.mode(), Provider: rec.provider(), Domain: rec.Domain}
	dir, err := shareDirFor(rec.ID)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	// 1. the process
	res.ProcessGone = killShareProcess(rec.PID)

	// A LOCAL, LAN or QUICK share has no DNS record and no named tunnel, so
	// the Cloudflare path is SKIPPED rather than run and no-opped:
	// no API call, no token read, nothing that can fail. Teardown is exactly
	// "process gone, port free, state wiped" — and `revoke --all` / `panic`
	// therefore handle a mix of local + lan + quick + domain shares in one
	// pass, without one rung's failure aborting the others.
	//
	// A quick tunnel additionally leaves a cloudflared behind if the instance
	// was SIGKILLed, so it is matched on the --url argument derived from this
	// share's own port and killed. Matching on a derived argument rather than
	// on the binary name is what keeps this away from the permanent
	// deployment's cloudflared, which runs with --config.
	if !rec.hasCloudflareResources() {
		res.DNSGone, res.TunnelGone, res.CloudflareNA = true, true, true
		bindAddr := fmt.Sprintf("0.0.0.0:%d", rec.Port)
		switch {
		case rec.isLocal():
			bindAddr = fmt.Sprintf("127.0.0.1:%d", rec.Port)
		case rec.isQuick() && rec.provider() == "cloudflare":
			pattern := expose.QuickURLPattern(rec.Port)
			killStrayCloudflared(pattern)
			waitForNoCloudflared(pattern, 10*time.Second)
			bindAddr = fmt.Sprintf("127.0.0.1:%d", rec.Port)
		case !rec.isLAN():
			// A tunnel share carried by something other than the built-in
			// Cloudflare provider — a JS adapter, or a record from a build
			// that still had the ngrok provider. Its child process dies with
			// the share process, and nothing it used was created here, so
			// there is nothing remote to delete: the name stays exactly where
			// its owner put it.
			bindAddr = fmt.Sprintf("127.0.0.1:%d", rec.Port)
		}
		res.PortFree = rec.Port == 0 || portFree(bindAddr)
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
		if dir, err := shareDirFor(rec.ID); err == nil && shareIsRunning(dir) {
			alive = true
		}
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
		// The authoritative check, after the cheap one: a share that holds its
		// run lock is running, whatever share.json's pid field says. This is
		// what makes `share restore` twice in a row converge on one instance.
		if shareIsRunning(dir) {
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
	// ONE instance per share, enforced by the kernel. A second spawn — from a
	// double `share restore`, from the daemon reconciling while an operator
	// runs it by hand — exits here quietly instead of fighting the first for
	// the port. Quietly, and with status 0, because the desired state has
	// already been reached: that is what makes restore safe to run twice.
	runLock, ok := tryShareRunLock(dir)
	if !ok {
		fmt.Fprintf(os.Stderr, "share %s is already running; nothing to do\n", id)
		return nil
	}
	defer runLock.Close()

	// A share logs to its OWN file inside its own state dir, so revoking the
	// share takes the log with it.
	log, logCloser := shareLogger(dir, id)
	if logCloser != nil {
		defer logCloser.Close()
	}
	log.Info("share starting", "session", rec.Session, "mode", rec.mode(),
		"port", rec.Port, "domain", rec.Domain, "expires_at", rec.ExpiresAt,
		"log", shareLogPath(dir))

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
	// the same rung it was created with. This is where "never escalate"
	// survives a RESTART: a local share comes back on loopback, a LAN share
	// never tries a tunnel, and a tunnel share never silently degrades to LAN.
	// Nothing here consults the config file, so editing [expose] under a live
	// share cannot move it up the ladder either.
	// The PROVIDER is checked against the record too, not just the rung: a
	// share restored after a reboot must come back on the same binary as well
	// as the same rung, or its teardown and its footprint no longer describe
	// what is actually running. A record naming a provider this build no
	// longer has (the built-in ngrok, removed in AMENDMENTS 18 / ADR 0034) is
	// therefore REFUSED rather than quietly re-pointed at Cloudflare, which
	// would aim a tunnel at a hostname in somebody else's account.
	if p := rec.provider(); p != "cloudflare" && !rec.isLocal() && !rec.isLAN() {
		return fmt.Errorf("share %s was created with the %q provider, which is no longer built in: "+
			"revoke it (`herdr-expose share revoke %s`) and create a new one, or carry that transport "+
			"with a JS adapter under [expose]", rec.ID, p, rec.ID)
	}
	exp := config.Expose{LAN: true}
	switch {
	case rec.isLocal():
		// The zero table: loopback, no tunnel, nothing to start.
		exp = config.Expose{}
	case rec.isQuick():
		// Quick is reconstructed from the record too, so a restored share
		// stays quick and never silently acquires a domain — and, just as
		// importantly, a domain share can never silently degrade to an
		// ephemeral hostname on restart.
		exp = config.Expose{Quick: true}
	case !rec.isLAN():
		exp = config.Expose{Cloudflare: true, Domain: rec.Domain, TunnelName: rec.TunnelName}
	}
	mgr := expose.New(expose.Options{
		Port:       rec.Port,
		Expose:     exp,
		StateDir:   dir,
		DNSComment: expose.ShareDNSComment,
		// A share owns `herdr-expose-share-<id>` outright, so it is allowed to
		// delete and recreate that tunnel when a crash lost its credentials.
		// The permanent deployment is not, and does not set this.
		Exclusive: true,
		Logf:      func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) },
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

		// A quick tunnel that is restarted by the supervisor comes back on a
		// DIFFERENT hostname. `share list` must report the one that works, not
		// the one it was born with, so the record follows the live URL.
		if !rec.isQuick() {
			return
		}
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cur := mgr.URL()
				if cur == "" || cur == url {
					continue
				}
				url = cur
				_, _ = updateShare(id, func(r *shareRecord) { r.URL = cur })
				log.Info("quick tunnel hostname changed", "url", cur,
					"note", "device tokens are origin-bound and do not follow it; re-pair")
			}
		}
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
