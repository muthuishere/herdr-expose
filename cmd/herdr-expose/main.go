// Command herdr-expose bridges a Herdr session to a local web UI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/core"
	"github.com/muthuishere/herdr-expose/internal/expose"
	"github.com/muthuishere/herdr-expose/internal/serve"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "daemon":
		err = cmdDaemon()
	case "status":
		err = cmdStatus(os.Args[2:])
	case "open":
		err = cmdOpen()
	case "stop":
		err = cmdStop()
	case "pair":
		err = cmdPair(os.Args[2:])
	case "install-service":
		err = cmdService([]string{"install"})
	case "uninstall-service":
		err = cmdService([]string{"uninstall"})
	case "devices":
		err = cmdDevices(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "expose":
		err = cmdExpose(os.Args[2:])
	case "share":
		err = cmdShare(os.Args[2:])
	case "panic":
		err = cmdPanic(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("herdr-expose", version)
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-expose:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `herdr-expose — Herdr session over HTTP/WebSocket

  serve                 run the server in the foreground (flock; quiet exit if held)
  daemon                fork-exec serve detached and exit 0 immediately
  status [--watch]      show server, upstream, devices and exposure state
  open                  open the local (or exposed) URL in a browser
  stop                  stop a running server
  pair [--name NAME] [--pane]
                        mint a one-time pairing code and show its QR LOCALLY
  install-service       install and load the launchd / systemd --user unit
  uninstall-service     remove it
  devices [--revoke ID] list or revoke paired devices
  service install|uninstall|status
                        install a supervised unit (launchd / systemd --user)
  expose start|stop|status|plan|destroy
                        tunnel control
  share [--domain X | --lan] [--session NAME] [--pane TARGET] [--hours N|--days N]
                        expose ONE herdr session, time-boxed, self-destructing.
                        --domain = public https via a Cloudflare tunnel,
                        --lan = http://<lan-ip>:<port>, neither = auto (cloudflare
                        if a usable domain is configured, else LAN, reason printed)
  share list|extend <id>|pair <id>|revoke <id>|revoke --all|restore
                        manage shares (every verb takes --json)
  panic                 revoke every share AND stop the main tunnel

  serve also accepts --only <session> / --only-target <session>/<pane> to pin
  an instance to one scope; the scope is enforced in the store, not the client.
`)
}

// originOrNone names the session this process was launched from, for logging.
func originOrNone() string {
	if name, _ := upstream.OriginSession(); name != "" {
		return name
	}
	return "(none)"
}

func newLogger() *slog.Logger {
	lvl := slog.LevelInfo
	if os.Getenv("HERDR_EXPOSE_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// cfgAdapter adapts the config store (workstream C) plus the exposure manager
// onto the narrow interface internal/serve consumes, so neither package depends
// on the other.
//
// Bind and Mode come from the store AFTER expose.Resolution has written them in
// with SetBinding: the resolved mode decides the bind address, never this layer.
// AllowedOrigins is read live from the manager so a DHCP lease change updates
// the LAN origin without a restart.
type cfgAdapter struct {
	store *config.Store
	mgr   *expose.Manager
}

func (a cfgAdapter) Port() int    { return a.store.Port() }
func (a cfgAdapter) Bind() string { return a.store.Bind() }
func (a cfgAdapter) Mode() string { return a.store.Mode() }

func (a cfgAdapter) AllowedOrigins() []string {
	return a.mgr.AllowedOrigins(a.store.Current().Server.AllowedOrigins)
}

func (a cfgAdapter) UI() map[string]any {
	c := a.store.Current()
	return map[string]any{"theme": c.UI.Theme, "default_view": c.UI.DefaultView}
}

// exposeAdapter surfaces the tunnel URL in /v1/config.
type exposeAdapter struct{ mgr *expose.Manager }

func (e exposeAdapter) Status() (string, bool) {
	st := e.mgr.Status()
	return e.mgr.URL(), st.Healthy
}

func cmdServe(args []string) error {
	log := newLogger()

	// G2: an instance may be pinned to ONE session (or specific panes) at
	// startup. The scope lives in the store, so an out-of-scope pane is absent
	// from the tree rather than hidden by the client.
	scope, err := parseScopeFlags(args)
	if err != nil {
		return err
	}

	store, err := config.Open()
	if err != nil {
		return err
	}
	cfg := store.Current()
	if err := cfg.Validate(); err != nil {
		return err
	}
	state, err := StateDir()
	if err != nil {
		return err
	}

	// Resolve the exposure mode FIRST: it decides the bind address (SPEC E1).
	// cloudflare + domain but no cloudflared binary falls back to lan loudly
	// rather than failing.
	mgr := exposeManagerFor(cfg, state)
	defer mgr.Close()
	res := mgr.Resolution()
	if res.FellBack != "" {
		log.Warn("exposure fell back", "reason", res.FellBack, "mode", res.Mode)
	}
	if err := store.SetBinding(res.Mode, res.Bind); err != nil {
		return err
	}
	log.Info("exposure resolved", "mode", res.Mode, "bind", res.Bind,
		"url", res.URL, "secure_context", res.SecureContext)

	addr := fmt.Sprintf("%s:%d", store.Bind(), store.Port())

	// Layer 3: never fight a manager for the port.
	if managed, who := managerInCharge(); managed && os.Getenv("HERDR_EXPOSE_SUPERVISED") == "" {
		log.Info("a service manager supervises herdr-expose; not starting a second copy", "manager", who)
		return nil
	}
	reclaimStrays(state, addr)

	lock, err := acquirePidLock(state)
	if errors.Is(err, ErrAlreadyRunning) {
		// The Herdr startup hook fires on every session restore. Double start
		// must be harmless and silent.
		log.Debug("another instance holds the pidfile; exiting quietly")
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.release()
	recordPID(state, ledgerEntry{PID: os.Getpid(), At: time.Now(), Kind: "serve", Addr: addr})

	// Fail loudly on a missing herdr binary instead of looping forever: a
	// launchd/systemd unit has a minimal PATH and a bare "herdr" will not exec.
	bin, err := upstream.ResolveHerdrBin()
	if err != nil {
		return err
	}
	log.Info("resolved herdr binary", "path", bin)
	sessions, err := upstream.ListSessions(context.Background())
	if err != nil {
		return fmt.Errorf("cannot list herdr sessions: %w", err)
	}
	running := upstream.RunningSessions(sessions)
	names := make([]string, 0, len(running))
	for _, se := range running {
		names = append(names, se.Name)
	}
	log.Info("herdr sessions discovered", "known", len(sessions),
		"running", strings.Join(names, ","), "origin", originOrNone())
	if len(running) == 0 {
		log.Warn("no running herdr session right now; the server will pick them up as they appear")
	}

	adapter := cfgAdapter{store: store, mgr: mgr}
	auth, err := serve.NewAuth(state, log, adapter.AllowedOrigins())
	if err != nil {
		return err
	}
	if tok, created, err := auth.EnsureServerToken(); err != nil {
		return err
	} else if created {
		// Shown exactly once: only the SHA-256 is stored.
		fmt.Fprintf(os.Stderr, "\n  herdr-expose server token (shown once):\n\n    %s\n\n", tok)
		fmt.Fprintf(os.Stderr, "  open: %s/?token=%s\n\n", res.URL, tok)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP re-reads config, re-resolves the mode and the LAN IP.
	go store.WatchSignals(ctx)
	store.OnChange(func(c *config.Config) {
		r := mgr.SetExpose(c.Expose, c.Port())
		if err := store.SetBinding(r.Mode, r.Bind); err != nil {
			log.Warn("rebinding after reload failed", "err", err)
		}
		log.Info("config reloaded", "mode", r.Mode, "url", r.URL)
	})

	// Multi-session: the store discovers every RUNNING Herdr session and keeps
	// one client per session. $HERDR_SOCKET_PATH is just the session we were
	// launched from, and gets no special treatment beyond an `origin` label.
	st := core.NewStore(log)
	st.SetScope(scope)
	go st.Run(ctx)
	hub := core.NewHub(st, log)
	go hub.Run(ctx)

	// Shares are separate processes with their own ports and state dirs, but the
	// MAIN daemon owns their crash safety (G5.3 + AMENDMENTS 10): on startup it
	// reaps any whose deadline passed and respawns any that are still in date
	// with their ORIGINAL deadline, then sweeps on a timer forever. A share
	// whose process died must never leave a live DNS record pointing nowhere,
	// and must never come back with a fresh clock.
	if !scope.Active() {
		go func() {
			restoreShares(log)
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					sweepShares(log)
				}
			}
		}()
	}

	// In lan/local there is nothing to start — the listener IS the exposure.
	if cfg.Expose.ShouldAutostart() || res.Remote {
		go superviseExposure(ctx, mgr, state, log)
	}

	srv, err := serve.New(serve.Options{
		Version:  version,
		Config:   adapter,
		Hub:      hub,
		Auth:     auth,
		Log:      log,
		Static:   webFS(), // nil unless workstream D wired an embedded bundle
		Exposure: exposeAdapter{mgr},
	})
	if err != nil {
		return err
	}
	return srv.Serve(ctx)
}

// cmdDaemon is layer 1: fork-exec serve detached and exit 0 immediately, so a
// one-shot Herdr startup hook completes cleanly.
func cmdDaemon() error {
	state, err := StateDir()
	if err != nil {
		return err
	}
	if managed, who := managerInCharge(); managed {
		fmt.Println("supervised by", who, "- nothing to do")
		return nil
	}
	if pid := readPid(state); pidAlive(pid) {
		fmt.Println("already running, pid", pid)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(state+"/serve.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	recordPID(state, ledgerEntry{PID: cmd.Process.Pid, At: time.Now(), Kind: "daemon"})
	_ = cmd.Process.Release()
	fmt.Println("started, pid", cmd.Process.Pid)
	return nil
}

func cmdStop() error {
	state, err := StateDir()
	if err != nil {
		return err
	}
	if managed, who := managerInCharge(); managed {
		return fmt.Errorf("%s supervises this server; stop it there (or run `herdr-expose service uninstall`)", who)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", cfg.Bind(), cfg.Port())
	reclaimStrays(state, addr)
	fmt.Println("stopped")
	return nil
}

// cmdOpen opens the resolved URL. The Herdr plugin manifest invokes it as
// `hex:open`, and the `hex:expose-url` link handler routes clicked pane URLs
// here.
func cmdOpen() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	url := exposeManager(cfg).Resolution().URL
	fmt.Println(url)
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	}
	return nil
}

// cmdStatus renders status once, or repaints it on a timer with --watch (the
// manifest's `hex:status` overlay pane runs it that way).
func cmdStatus(args []string) error {
	watch := false
	for _, a := range args {
		if a == "--watch" {
			watch = true
		}
	}
	if !watch {
		return statusOnce()
	}
	for {
		// Repaint in place: clear screen, home cursor.
		fmt.Print("\x1b[2J\x1b[H")
		if err := statusOnce(); err != nil {
			fmt.Fprintln(os.Stderr, "status:", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func statusOnce() error {
	state, err := StateDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	mgr := exposeManager(cfg)
	defer mgr.Close()
	res := mgr.Resolution()
	addr := fmt.Sprintf("%s:%d", res.Bind, res.Port)
	fmt.Printf("version   %s\nconfig    %s\nstate     %s\nmode      %s\nlisten    http://%s\nurl       %s\n",
		version, cfg.Path(), state, res.Mode, addr, res.URL)
	if res.FellBack != "" {
		fmt.Println("fallback ", res.FellBack)
	}
	if !res.SecureContext {
		fmt.Println("note      not a secure context: no PWA install / service worker on this URL")
	}
	if u := mgr.URL(); u != "" && u != res.URL {
		fmt.Println("tunnel   ", u)
	}
	if managed, who := managerInCharge(); managed {
		fmt.Println("manager  ", who)
	} else {
		fmt.Println("manager   none (self-daemonized)")
	}
	if pid := readPid(state); pidAlive(pid) {
		fmt.Println("pid      ", pid)
	} else {
		fmt.Println("pid       not running")
	}
	if bin, err := upstream.ResolveHerdrBin(); err == nil {
		fmt.Println("herdr    ", bin)
	} else {
		fmt.Println("herdr     NOT FOUND -", err)
	}
	// Multi-session: every RUNNING session is exposed, not just the one we were
	// launched from.
	if all, err := upstream.ListSessions(context.Background()); err == nil {
		running := upstream.RunningSessions(all)
		fmt.Printf("sessions  %d running of %d known\n", len(running), len(all))
		origin, _ := upstream.OriginSession()
		for _, se := range running {
			mark := ""
			if se.Name == origin {
				mark = "  (origin)"
			}
			fmt.Printf("          %-20s %s%s\n", se.Name, se.SocketPath, mark)
		}
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Println("health    unreachable -", err)
	} else {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var pretty bytes.Buffer
		if json.Indent(&pretty, b, "          ", "  ") == nil {
			fmt.Println("health   ", pretty.String())
		} else {
			fmt.Println("health   ", string(b))
		}
	}
	auth, err := serve.NewAuth(state, newLogger(), nil)
	if err == nil {
		devs := auth.Devices()
		fmt.Printf("devices   %d paired\n", len(devs))
		for _, d := range devs {
			fmt.Printf("          %s  %-16s last seen %s from %s\n",
				d.ID, d.Name, d.LastSeen.Format(time.RFC3339), d.LastIP)
		}
	}
	return nil
}

// cmdPair mints a pairing code and renders its QR HERE, on this machine.
//
// This is a security property, not a UI choice. The code is never sent outbound
// over the tunnel and no HTTP route issues one, so reaching the public URL is
// not enough to pair — an attacker would have to be sitting at this laptop.
// POST /v1/pair only REDEEMS a code that was displayed locally.
func cmdPair(args []string) error {
	name := "device"
	pane := false
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			name = args[i+1]
		}
		if a == "--pane" {
			pane = true
		}
	}
	state, err := StateDir()
	if err != nil {
		return err
	}
	auth, err := serve.NewAuth(state, newLogger(), nil)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	code, exp, err := auth.NewPairingCode(name)
	if err != nil {
		return err
	}

	// Point the phone at whatever origin it can actually reach: the tunnel
	// domain when exposed, the LAN IP on wifi, loopback otherwise.
	mgr := exposeManager(cfg)
	defer mgr.Close()
	res := mgr.Resolution()
	origin := strings.TrimRight(res.URL, "/")
	if u := mgr.URL(); u != "" {
		origin = strings.TrimRight(u, "/")
	}
	if origin == "" {
		if ip := expose.PrimaryLANIP(); ip != "" {
			origin = fmt.Sprintf("http://%s:%d", ip, cfg.Port())
		} else {
			origin = fmt.Sprintf("http://127.0.0.1:%d", cfg.Port())
		}
	}
	url := fmt.Sprintf("%s/?pair=%s", origin, code)

	if pane {
		// Rendered inside the Herdr overlay pane (`hex:pair-qr`): centre the
		// essentials, no log noise.
		fmt.Print("\x1b[2J\x1b[H")
	}
	fmt.Printf("\n  scan this on the phone — shown ONLY here, never sent over the tunnel\n\n")
	printQR(url)
	fmt.Printf("\n  pairing code: %s   (for %q)\n  valid until:  %s\n  url:          %s\n\n",
		code, name, exp.Format(time.Kitchen), url)
	if pane {
		// The overlay pane closes when the command exits; hold it open so the
		// QR stays scannable for the life of the code.
		fmt.Println("  (this pane stays open until the code expires)")
		time.Sleep(time.Until(exp))
	}
	return nil
}

func cmdDevices(args []string) error {
	state, err := StateDir()
	if err != nil {
		return err
	}
	auth, err := serve.NewAuth(state, newLogger(), nil)
	if err != nil {
		return err
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--revoke" {
			if err := auth.RevokeDevice(args[i+1]); err != nil {
				return err
			}
			fmt.Println("revoked", args[i+1])
			return nil
		}
	}
	for _, d := range auth.Devices() {
		fmt.Printf("%s  %-16s  created %s  last seen %s  %s  %s\n",
			d.ID, d.Name, d.CreatedAt.Format(time.RFC3339),
			d.LastSeen.Format(time.RFC3339), d.LastIP, d.UserAgent)
	}
	return nil
}

func cmdService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: herdr-expose service install|uninstall|status")
	}
	switch args[0] {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		state, err := StateDir()
		if err != nil {
			return err
		}
		path, err := installService(exe, state)
		if err != nil {
			return err
		}
		fmt.Println("installed", path)
		return nil
	case "uninstall":
		if err := uninstallService(); err != nil {
			return err
		}
		fmt.Println("uninstalled")
		return nil
	case "status":
		if managed, who := managerInCharge(); managed {
			fmt.Println("supervised by", who)
		} else {
			fmt.Println("no service manager in charge")
		}
		return nil
	}
	return fmt.Errorf("unknown service subcommand %q", args[0])
}

// exposeManagerFor builds a tunnel manager from config.
func exposeManagerFor(cfg *config.Config, state string) *expose.Manager {
	root := os.Getenv("HERDR_PLUGIN_ROOT")
	if root == "" {
		root, _ = os.Getwd()
	}
	return expose.New(expose.Options{
		Port: cfg.Port(), Expose: cfg.Expose, Root: root, StateDir: state,
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
}

func exposeManager(cfg *config.Config) *expose.Manager {
	state, _ := StateDir()
	return exposeManagerFor(cfg, state)
}

func cmdExpose(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	mgr := exposeManager(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	state, err := StateDir()
	if err != nil {
		return err
	}
	switch sub {
	case "start":
		// Clear the halt flag first: the running daemon is what actually owns
		// the tunnel process, and it re-arms within seconds of the flag going.
		if err := os.Remove(haltFilePath(state)); err == nil {
			fmt.Println("cleared the exposure halt flag; the daemon re-arms the tunnel within ~5s")
		}
		st, err := mgr.Start(ctx)
		if err != nil {
			return err
		}
		printJSON(st)
		return nil
	case "stop":
		// Raise the halt flag BEFORE killing anything, or the daemon's
		// supervisor simply restarts cloudflared a second later.
		if err := os.WriteFile(haltFilePath(state), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			return err
		}
		killStrayCloudflared(filepath.Join(state, "cloudflare", "config.yml"))
		if !waitForNoCloudflared(filepath.Join(state, "cloudflare", "config.yml"), 20*time.Second) {
			return errors.New("cloudflared is STILL running for the main tunnel")
		}
		fmt.Println("main tunnel stopped (DNS record and tunnel left in place — static domain)")
		return mgr.Stop()
	case "status":
		printJSON(mgr.Status())
		return nil
	case "plan":
		steps, err := mgr.Plan(ctx)
		if err != nil {
			return err
		}
		printJSON(steps)
		return nil
	case "destroy":
		return mgr.Destroy(ctx)
	}
	return fmt.Errorf("unknown expose subcommand %q", sub)
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

// printQR renders the pairing URL as a QR code in THIS terminal. It is rendered
// in-process so it works inside a Herdr overlay pane with no external binary.
func printQR(url string) {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		fmt.Println("  (could not render QR:", err, ")")
		return
	}
	fmt.Print(q.ToSmallString(false))
}

// parseScopeFlags reads `--only` / `--only-target` off a subcommand's args.
func parseScopeFlags(args []string) (*core.Scope, error) {
	only := ""
	var targets []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--only":
			if i+1 >= len(args) {
				return nil, errors.New("--only needs a session name")
			}
			i++
			only = args[i]
		case "--only-target":
			if i+1 >= len(args) {
				return nil, errors.New("--only-target needs <session>/<pane>")
			}
			i++
			targets = append(targets, args[i])
		}
	}
	return core.ParseScope(only, targets)
}

// superviseExposure owns the main tunnel's up/down state for the life of the
// daemon, driven by a HALT FLAG FILE in the state dir.
//
// The flag exists because `expose stop` and `panic` run in a DIFFERENT process
// from the daemon that supervises cloudflared: killing the child from outside
// just makes the supervisor restart it, and an in-process Stop() on a fresh
// Manager is a no-op that only looks like it worked. The flag is resolved from
// the state dir by construction, the running daemon is the thing that reads it,
// and the daemon reports the result — which is the whole lesson of a halt that
// writes where nothing reads.
func superviseExposure(ctx context.Context, mgr *expose.Manager, state string, log *slog.Logger) {
	halt := haltFilePath(state)
	halted := true // force the first evaluation to act
	first := true
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		_, err := os.Stat(halt)
		present := err == nil
		switch {
		case present && (!halted || first):
			log.Warn("exposure halt flag present; stopping the tunnel", "flag", halt)
			_ = mgr.Stop()
			halted = true
		case !present && (halted || first):
			if _, err := mgr.Start(ctx); err != nil {
				log.Warn("exposure did not start", "err", err)
			}
			halted = false
		}
		first = false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
