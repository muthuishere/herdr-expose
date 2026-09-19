package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/expose"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// `doctor` turns "it doesn't work" into one line of output.
//
// Everything this binary depends on is external and can be individually
// missing: a herdr binary that is not on a launchd unit's PATH, a socket that
// moved, a port a stray process kept, a Cloudflare token without Zone:DNS:Edit,
// a DNS record that never propagated, a share whose deadline passed. Each of
// those fails in its own way at its own moment, and the user sees one symptom:
// nothing loads. One command that checks all of them, in order, is worth more
// than any amount of error-message polish.
//
// Rules it follows:
//   - never print a credential. The Cloudflare token is checked by USING it
//     and reporting the outcome, never by echoing it.
//   - a missing optional thing is a skip, not a failure: not configuring
//     Cloudflare is a valid way to run.
//   - every failure carries a hint that names the fix.

type checkStatus string

const (
	statusPass checkStatus = "pass"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
	statusSkip checkStatus = "skip"
)

type check struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
	Hint   string      `json:"hint,omitempty"`
}

type doctorReport struct {
	Version string  `json:"version"`
	OK      bool    `json:"ok"`
	Checks  []check `json:"checks"`
}

type doctorRun struct {
	ctx    context.Context
	checks []check
}

func (d *doctorRun) add(name string, st checkStatus, detail string, hint ...string) {
	c := check{Name: name, Status: st, Detail: detail}
	if len(hint) > 0 {
		c.Hint = hint[0]
	}
	d.checks = append(d.checks, c)
}

func cmdDoctor(args []string) error {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "help", "--help", "-h":
			fmt.Fprint(os.Stderr, "herdr-expose doctor [--json]\n"+
				"  preflight every dependency and print pass/fail per check\n")
			return nil
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	d := &doctorRun{ctx: ctx}

	cfg := d.checkConfig()
	state := d.checkStateDir()
	d.checkHerdrBinary()
	sessions := d.checkSessions()
	d.checkSocket(sessions)
	d.checkPort(cfg, state)
	d.checkServiceManager()
	d.checkExposure(cfg, state)
	d.checkCloudflared(cfg)
	d.checkCloudflareToken(cfg)
	d.checkDNS(cfg)
	d.checkShares()
	d.checkLogging(cfg, state)

	ok := true
	for _, c := range d.checks {
		if c.Status == statusFail {
			ok = false
		}
	}
	rep := doctorReport{Version: version, OK: ok, Checks: d.checks}

	if asJSON {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	} else {
		printDoctor(rep)
	}
	if !ok {
		os.Exit(1)
	}
	return nil
}

func printDoctor(rep doctorReport) {
	fmt.Printf("herdr-expose doctor  (version %s)\n\n", rep.Version)
	width := 0
	for _, c := range rep.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range rep.Checks {
		mark := map[checkStatus]string{
			statusPass: "PASS", statusWarn: "WARN", statusFail: "FAIL", statusSkip: "skip",
		}[c.Status]
		fmt.Printf("  %-4s  %-*s  %s\n", mark, width, c.Name, c.Detail)
		if c.Hint != "" {
			fmt.Printf("        %-*s  -> %s\n", width, "", c.Hint)
		}
	}
	fmt.Println()
	if rep.OK {
		fmt.Println("  everything that must work, works.")
	} else {
		fmt.Println("  at least one check FAILED — see the hints above.")
	}
}

// --- individual checks ------------------------------------------------------

func (d *doctorRun) checkConfig() *config.Config {
	path, err := config.DefaultPath()
	if err != nil {
		d.add("config", statusFail, "cannot resolve the config path: "+err.Error())
		return config.Defaults()
	}
	existed := true
	if _, serr := os.Stat(path); os.IsNotExist(serr) {
		existed = false
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		d.add("config", statusFail, err.Error(),
			"fix the file, or run `herdr-expose config print-default` and start from that")
		return config.Defaults()
	}
	detail := fmt.Sprintf("valid — %s (port %d, mode %s)", path, cfg.Port(), cfg.Mode())
	if !existed {
		detail += " [created just now with the full commented defaults]"
	}
	d.add("config", statusPass, detail)
	return cfg
}

func (d *doctorRun) checkStateDir() string {
	state, err := StateDir()
	if err != nil {
		d.add("state dir", statusFail, err.Error())
		return ""
	}
	probe := filepath.Join(state, ".doctor-write-probe")
	if err := os.MkdirAll(state, 0o700); err != nil {
		d.add("state dir", statusFail, state+": "+err.Error(),
			"the daemon keeps its pidfile, auth store and share records here")
		return state
	}
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		d.add("state dir", statusFail, state+" is not writable: "+err.Error())
		return state
	}
	os.Remove(probe)
	d.add("state dir", statusPass, state+" (writable)")
	return state
}

func (d *doctorRun) checkHerdrBinary() {
	bin, err := upstream.ResolveHerdrBin()
	if err != nil {
		d.add("herdr binary", statusFail, err.Error(),
			"install herdr, or set $HERDR_BIN_PATH — a launchd/systemd unit has a minimal PATH "+
				"and a bare `herdr` will not exec there")
		return
	}
	ver := "version unknown"
	cctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	out, verr := exec.CommandContext(cctx, bin, "--version").CombinedOutput()
	if verr == nil {
		ver = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	}
	d.add("herdr binary", statusPass, fmt.Sprintf("%s (%s)", bin, ver))
}

func (d *doctorRun) checkSessions() []upstream.Session {
	all, err := upstream.ListSessions(d.ctx)
	if err != nil && len(all) == 0 {
		d.add("herdr sessions", statusFail, "cannot list sessions: "+err.Error(),
			"is the herdr server running? try `herdr session list`")
		return nil
	}
	running := upstream.RunningSessions(all)
	names := make([]string, 0, len(running))
	for _, s := range running {
		names = append(names, s.Name)
	}
	detail := fmt.Sprintf("%d running of %d known", len(running), len(all))
	if len(names) > 0 {
		detail += ": " + strings.Join(names, ", ")
	}
	if len(running) == 0 {
		d.add("herdr sessions", statusWarn, detail,
			"nothing to expose yet; the server picks sessions up as they appear")
		return running
	}
	d.add("herdr sessions", statusPass, detail)
	return running
}

func (d *doctorRun) checkSocket(running []upstream.Session) {
	if len(running) == 0 {
		d.add("herdr socket", statusSkip, "no running session to talk to")
		return
	}
	target := running[0]
	if origin, _ := upstream.OriginSession(); origin != "" {
		for _, s := range running {
			if s.Name == origin {
				target = s
			}
		}
	}
	cctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	pong, err := upstream.NewSessionClient(target).Ping(cctx)
	if err != nil {
		d.add("herdr socket", statusFail,
			fmt.Sprintf("%s (%s): %v", target.Name, target.SocketPath, err),
			"the socket is listed but not answering; the session may have died without cleaning up")
		return
	}
	d.add("herdr socket", statusPass, fmt.Sprintf("%s — herdr %s, protocol %d",
		target.SocketPath, pong.Version, pong.Protocol))
}

// checkPort answers the question that actually bites: is the port free, or is
// it OUR daemon holding it, or is it something else entirely?
func (d *doctorRun) checkPort(cfg *config.Config, state string) {
	port := cfg.Port()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	pid := readPid(state)
	ours := pidAlive(pid)

	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		if ours {
			d.add("port", statusWarn, fmt.Sprintf("%d is free, but our pidfile claims pid %d is alive", port, pid),
				"a stale pidfile; `herdr-expose stop` clears it")
			return
		}
		d.add("port", statusPass, fmt.Sprintf("%d is free (nothing serving yet)", port))
		return
	}

	// Held. Is it us?
	client := &http.Client{Timeout: 3 * time.Second}
	resp, herr := client.Get("http://" + addr + "/healthz")
	if herr == nil {
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		who := "herdr-expose"
		if v, ok := body["version"].(string); ok {
			who += " " + v
		}
		if ours {
			d.add("port", statusPass, fmt.Sprintf("%d held by us — %s, pid %d", port, who, pid))
		} else {
			d.add("port", statusPass, fmt.Sprintf("%d held by %s (no pidfile for it here)", port, who))
		}
		return
	}
	d.add("port", statusFail, fmt.Sprintf("%d is held by something that is not herdr-expose (%v)", port, herr),
		fmt.Sprintf("find it with `lsof -nP -iTCP:%d -sTCP:LISTEN`, or change `port` under [server]", port))
}

func (d *doctorRun) checkServiceManager() {
	if managed, who := managerInCharge(); managed {
		d.add("service manager", statusPass, who+" supervises herdr-expose")
		return
	}
	d.add("service manager", statusSkip, "none — the daemon is self-supervised",
		"`herdr-expose service install` makes it survive a reboot")
}

func (d *doctorRun) checkExposure(cfg *config.Config, state string) {
	// Quiet: doctor's output IS the report, so the manager must not narrate
	// onto stderr in the middle of it.
	mgr := exposeManagerLogging(cfg, state, func(string, ...any) {})
	defer mgr.Close()
	res := mgr.Resolution()

	detail := fmt.Sprintf("mode %s, binds %s:%d, url %s", res.Mode, res.Bind, res.Port, res.URL)
	switch {
	case res.FellBack != "":
		d.add("exposure", statusWarn, detail, "fell back: "+res.FellBack)
	case res.Mode == config.ModeLAN:
		d.add("exposure", statusPass, detail,
			"LAN mode: anyone on this network can reach the port. Auth is still required, and plain "+
				"HTTP on a LAN IP is not a secure context — the web app works but the PWA cannot be installed")
	default:
		d.add("exposure", statusPass, detail)
	}
}

func (d *doctorRun) checkCloudflared(cfg *config.Config) {
	if cfg.Expose.Provider() != config.ProviderCloudflare {
		d.add("cloudflared", statusSkip, "cloudflare is not enabled under [expose]")
		return
	}
	path, err := exec.LookPath("cloudflared")
	if err != nil {
		d.add("cloudflared", statusFail, "not on PATH",
			"install it (`brew install cloudflared`) — without it the tunnel cannot start and "+
				"herdr-expose falls back to LAN mode")
		return
	}
	cctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(cctx, path, "--version").CombinedOutput()
	d.add("cloudflared", statusPass, fmt.Sprintf("%s (%s)", path, strings.TrimSpace(string(out))))
}

// checkCloudflareToken verifies the token BY USING IT and reports only the
// outcome. The value never appears in the output, in an error message or in a
// state file — the same rule as every other credential here.
func (d *doctorRun) checkCloudflareToken(cfg *config.Config) {
	domain := strings.TrimSpace(cfg.Expose.Domain)
	if cfg.Expose.Provider() != config.ProviderCloudflare || domain == "" {
		d.add("cloudflare token", statusSkip, "no Cloudflare domain configured")
		return
	}
	if strings.TrimSpace(os.Getenv(expose.CloudflareTokenEnv)) == "" &&
		strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN")) == "" {
		d.add("cloudflare token", statusFail, "$"+expose.CloudflareTokenEnv+" is not set",
			"export a token with Account:Cloudflare Tunnel:Edit, Zone:Zone:Read and Zone:DNS:Edit")
		return
	}
	cctx, cancel := context.WithTimeout(d.ctx, 20*time.Second)
	defer cancel()
	zone, rec, err := expose.CheckZoneAccess(cctx, domain)
	if err != nil {
		d.add("cloudflare token", statusFail, redactSecrets("cannot reach the zone for "+domain+": "+err.Error()),
			"the token must have Zone:Zone:Read and Zone:DNS:Edit on that zone, and the zone must be "+
				"in an account the token can see")
		return
	}
	detail := fmt.Sprintf("valid for zone %s (Zone:Read + DNS:Edit confirmed)", zone)
	if rec.Exists {
		detail += fmt.Sprintf("; %s already has a record tagged %q", domain, rec.Comment)
	} else {
		detail += fmt.Sprintf("; %s has no record yet", domain)
	}
	d.add("cloudflare token", statusPass, detail)
}

func (d *doctorRun) checkDNS(cfg *config.Config) {
	domain := strings.TrimSpace(cfg.Expose.Domain)
	if domain == "" {
		d.add("dns", statusSkip, "no domain configured under [expose]")
		return
	}
	cctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(cctx, domain)
	if err != nil {
		d.add("dns", statusFail, domain+" does not resolve: "+err.Error(),
			"the proxied CNAME may not be created yet — `herdr-expose expose start` creates it")
		return
	}
	d.add("dns", statusPass, fmt.Sprintf("%s -> %s", domain, strings.Join(ips, ", ")))
}

func (d *doctorRun) checkShares() {
	recs, err := listShareRecords()
	if err != nil {
		d.add("shares", statusWarn, "cannot read the share records: "+err.Error())
		return
	}
	if len(recs) == 0 {
		d.add("shares", statusPass, "none active")
		return
	}
	now := time.Now()
	var lines []string
	overdue := 0
	for _, r := range recs {
		v := r.view()
		left := time.Until(r.ExpiresAt).Round(time.Second)
		state := "alive"
		if !pidAlive(r.PID) {
			state = "process gone"
		}
		if r.ExpiresAt.Before(now) {
			state = "EXPIRED"
			overdue++
		}
		lines = append(lines, fmt.Sprintf("%s %s expires %s (%s left, %s)",
			v.ID, v.URL, r.ExpiresAt.Format(time.RFC3339), left, state))
	}
	st := statusPass
	hint := ""
	if overdue > 0 {
		st = statusWarn
		hint = "run `herdr-expose share restore` to reap them — an expired share must not leave a live hostname"
	}
	d.add("shares", st, fmt.Sprintf("%d: %s", len(recs), strings.Join(lines, "; ")), hint)
}

// checkLogging is one of the first things to look at when something is wrong:
// if the log is not where you think it is, or is not writable, you are about
// to debug blind.
func (d *doctorRun) checkLogging(cfg *config.Config, state string) {
	path := logFilePath(cfg, state)
	detail := fmt.Sprintf("level=%s format=%s rotate=%dMB keep=%d -> %s",
		cfg.Log.Level, cfg.Log.Format, cfg.Log.MaxSizeMB, cfg.Log.Keep, path)

	// Writable is the check that matters: a log the daemon cannot open is a
	// daemon whose failures are invisible.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		d.add("log", statusFail, detail+" — cannot create its directory: "+err.Error())
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		d.add("log", statusFail, detail+" — not writable: "+err.Error(),
			"set `file` under [log] to a path this user owns")
		return
	}
	st, serr := f.Stat()
	f.Close()
	if serr != nil || st.Size() == 0 {
		d.add("log", statusWarn, detail+" (writable, empty so far)",
			"the daemon writes it on start; read it with `herdr-expose logs -f`")
		return
	}
	rotated := 0
	for i := 1; i <= cfg.Log.Keep; i++ {
		if _, err := os.Stat(fmt.Sprintf("%s.%d", path, i)); err == nil {
			rotated++
		}
	}
	d.add("log", statusPass, fmt.Sprintf("%s (writable, %s, %d rotated, last written %s)",
		detail, humanBytes(st.Size()), rotated, st.ModTime().Format(time.RFC3339)))
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// redactSecrets is a last-ditch scrub of anything that looks like a token in a
// third-party error string. Nothing should put one there, and this is the belt
// that makes sure a change elsewhere cannot start leaking one through doctor.
func redactSecrets(s string) string {
	for _, env := range []string{expose.CloudflareTokenEnv, "CLOUDFLARE_API_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "[redacted $"+env+"]")
		}
	}
	return s
}
