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
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/core"
	"github.com/muthuishere/herdr-expose/internal/expose"
	"github.com/muthuishere/herdr-expose/internal/platform"
	"github.com/muthuishere/herdr-expose/internal/serve"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Started BY a service manager that speaks a control protocol rather than
	// signals (the Windows SCM), this process is the service, not the CLI, and
	// never sees an argv. No-op everywhere else.
	if handled, serr := runUnderServiceManager(); handled {
		if serr != nil {
			fmt.Fprintln(os.Stderr, "error:", serr)
			os.Exit(1)
		}
		return
	}

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
		err = cmdService(append([]string{"install"}, os.Args[2:]...))
	case "uninstall-service":
		err = cmdService(append([]string{"uninstall"}, os.Args[2:]...))
	case "devices":
		err = cmdDevices(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "skill":
		err = cmdSkill(os.Args[2:])
	case "expose":
		err = cmdExpose(os.Args[2:])
	case "share":
		err = cmdShare(os.Args[2:])
	case "panic":
		err = cmdPanic(os.Args[2:])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "logs", "log":
		err = cmdLogs(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
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
  install-service [--force]
                        install and load the launchd / systemd --user unit.
                        Idempotent: already healthy means nothing to do.
                        --force is required to take over a server that is
                        already running without a manager (it names the pid it
                        will stop), and to rewrite a healthy unit.
  uninstall-service     unload and remove it, and say what is left serving
  devices [--revoke ID] list or revoke paired devices
  service install [--force] | uninstall | status
                        install a supervised unit (launchd / systemd --user)
  skill install | uninstall | status
                        link this checkout's skill/ into ~/.claude/skills (and
                        ~/.agents/skills when it exists) as the herdr-share
                        agent skill, so an agent can drive share for you.
                        A SYMLINK, so a rebuild or a git pull updates the skill.
                        Idempotent; refuses to clobber a real directory;
                        uninstall removes only links it made.
  expose start|stop|status|plan|destroy
                        tunnel control
  share [--local | --lan | --quick | --domain X]
        [--session NAME] [--pane TARGET] [--hours N|--days N]
                        expose ONE herdr session, time-boxed, self-destructing.
                        FOUR RUNGS, each one an explicit request:
                        none/--lan = http://<lan-ip>:<port>  (THE DEFAULT),
                        --quick    = public https on a throwaway hostname
                                     (no account, no DNS, nothing to clean up),
                        --domain X = public https on a name you own,
                        --local    = 127.0.0.1 only, an opt-IN for testing.
                        Nothing ever escalates above what you asked for; an
                        explicit tunnel request that cannot be honoured
                        degrades to --lan and says why.
  share --all [rung] [--hours N|--days N] [--yes]
                        expose EVERY running herdr session, not one — the whole
                        herdr, time-boxed and revocable. It names the sessions
                        it is about to expose and asks for a confirmation
                        (--yes pre-answers it). Not combinable with
                        --session/--pane.
  share list|extend <id>|pair <id>|revoke <id>|revoke --all|restore
                        manage shares (every verb takes --json)
  panic                 revoke every share AND stop the main tunnel
  config print-default|path|show|edit
                        the self-documenting config: print-default emits the
                        COMPLETE commented file with every key at its default
  logs [--follow] [-n N] [--share ID] [--json]
                        read the daemon's log — the only way to see what a
                        DETACHED daemon did; --share reads a share's own log
  doctor [--json]       preflight everything and print pass/fail per check

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

	// The log file comes up before anything else does, because everything
	// after this point is something you may later need to explain.
	logPath := logFilePath(cfg, state)
	if l, closer, lerr := buildLogger(cfg, logPath); lerr != nil {
		log.Warn("log file unusable; staying on stderr", "path", logPath, "err", lerr)
	} else {
		log = l
		if closer != nil {
			defer closer.Close()
		}
		// Say on the terminal where the log went, once. Otherwise a foreground
		// `serve` looks like it has gone silent.
		fmt.Fprintf(os.Stderr, "herdr-expose: logging to %s (herdr-expose logs -f)\n", logPath)
	}
	log.Info("server starting", "version", version, "pid", os.Getpid(),
		"config", cfg.Path(), "state", state, "log", logPath,
		"level", cfg.Log.Level, "format", cfg.Log.Format,
		"rotate_mb", cfg.Log.MaxSizeMB, "keep", cfg.Log.Keep)

	// Resolve the exposure mode FIRST: it decides the bind address (SPEC E1).
	// cloudflare + domain but no cloudflared binary falls back to lan loudly
	// rather than failing.
	mgr := exposeManagerLogging(cfg, state, func(f string, a ...any) {
		log.Info("exposure: " + fmt.Sprintf(f, a...))
	})
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

	ctx, stop := shutdownContext(context.Background())
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
	serveErr := srv.Serve(ctx)
	log.Info("server stopped", "mode", store.Mode(), "bind", store.Bind(),
		"port", store.Port(), "err", serveErr)
	return serveErr
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
	// The file the daemon writes and the file `herdr-expose logs` reads are
	// resolved by the SAME function, so a log can never land where nothing
	// reads it.
	logPath := filepath.Join(state, config.DefaultLogFileName)
	if cfg, cerr := config.Load(); cerr == nil {
		logPath = logFilePath(cfg, state)
	}
	if derr := os.MkdirAll(filepath.Dir(logPath), 0o700); derr != nil {
		return derr
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Stdin = nil
	platform.PrepareDetached(cmd)
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	recordPID(state, ledgerEntry{PID: cmd.Process.Pid, At: time.Now(), Kind: "daemon"})
	_ = cmd.Process.Release()
	fmt.Println("started, pid", cmd.Process.Pid)
	fmt.Println("logs:", logPath, "(herdr-expose logs -f)")
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
	case "windows":
		// `start` is a cmd BUILTIN, not a program, so it has to be run through
		// cmd. The empty "" is the window title: without it cmd reads a quoted
		// URL as the title and opens nothing at all.
		return exec.Command("cmd", "/c", "start", "", url).Start()
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
		return errors.New("usage: herdr-expose service install|uninstall|status [--force] [--task]")
	}
	force := hasFlag(args, "--force") || hasFlag(args, "--yes") || hasFlag(args, "-y")
	// --task selects the weaker, no-admin supervisor on Windows. It is a real
	// difference in the promise `service install` makes, so it is opt-in and
	// named, never a silent fallback. Nothing else has a second supervisor.
	task := hasFlag(args, "--task")
	if task && runtime.GOOS != "windows" {
		return errors.New("--task is a Windows-only option (Scheduled Task instead of a Service)")
	}
	switch args[0] {
	case "install":
		return cmdServiceInstall(force, task)
	case "uninstall":
		return cmdServiceUninstall()
	case "status":
		printServiceState(readServiceState())
		return nil
	}
	return fmt.Errorf("unknown service subcommand %q", args[0])
}

func printServiceState(st serviceState) {
	switch {
	case st.Healthy():
		fmt.Printf("supervised by %s (pid %d), unit %s\n", st.Who, st.PID, st.UnitPath)
	case st.Loaded:
		fmt.Printf("unit %s is loaded under %s but nothing is running\n", st.UnitPath, st.Who)
	case st.Installed:
		fmt.Printf("unit %s exists on disk but is not loaded\n", st.UnitPath)
	default:
		fmt.Println("no service manager in charge")
	}
}

// cmdServiceInstall is idempotent and, where it is not, LOUD.
//
// Two things went wrong before, and they are different problems:
//
//  1. Run twice, it rewrote and reloaded a unit that was already healthy —
//     dropping every live WebSocket to reach a state the machine was already
//     in. A second install now reads the manager first and says "nothing to
//     do", because converging on the desired state is the whole job.
//
//  2. Run on a machine with an UNMANAGED daemon, it took the port over in
//     silence. The unit's `serve` calls reclaimStrays, which SIGTERMs every
//     pid in the ledger — on a real machine, the server carrying the owner's
//     live sessions. Taking over is the correct end state (two copies fighting
//     for one port is worse), but it is not something to do behind the
//     operator's back, so it now needs --force and says exactly what it will
//     stop.
func cmdServiceInstall(force, task bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	state, err := StateDir()
	if err != nil {
		return err
	}

	st := readServiceState()
	if st.Healthy() && !force {
		fmt.Printf("already installed and healthy: %s is supervising pid %d\n", st.Who, st.PID)
		fmt.Printf("  unit: %s\n", st.UnitPath)
		fmt.Println("  nothing to do. Re-run with --force to rewrite the unit and restart it " +
			"(that WILL drop every connected client).")
		return nil
	}

	// The takeover. Say it before doing it, and name what will be stopped.
	if pid := unmanagedDaemonPID(state); pid > 0 && !st.Loaded {
		fmt.Printf("a herdr-expose server is already running WITHOUT a service manager (pid %d).\n", pid)
		fmt.Println("  Installing the unit takes that server over: the supervised copy stops it, then")
		fmt.Println("  serves the same port itself. Every connected client is dropped and reconnects.")
		if !force {
			fmt.Println("\n  Refusing to do that silently. Either:")
			fmt.Printf("    herdr-expose service install --force   (stop pid %d and supervise it)\n", pid)
			fmt.Println("    herdr-expose stop                      (stop it yourself first)")
			return errors.New("not installing over a running unmanaged server without --force")
		}
		fmt.Printf("  --force given: taking over from pid %d.\n", pid)
	}

	path, err := installService(exe, state, task)
	if err != nil {
		return err
	}
	fmt.Println("installed", path)

	// VERIFY. A loaded unit is not a running server: the failure mode this
	// misses is a unit that loads and exits instantly, which launchd will
	// happily retry forever while every other start path refuses because a
	// manager is "in charge".
	final := awaitServiceUp(15 * time.Second)
	if !final.Healthy() {
		return fmt.Errorf("the unit was installed at %s but is NOT running (loaded=%v active=%v pid=%d). "+
			"Check `herdr-expose logs` and %s/serve.log; uninstall with `herdr-expose service uninstall`",
			path, final.Loaded, final.Active, final.PID, state)
	}
	fmt.Printf("verified: %s is supervising pid %d\n", final.Who, final.PID)
	return nil
}

// cmdServiceUninstall removes the unit and SAYS what that did to the server.
//
// "uninstalled" on its own is the least useful thing to print here: the
// operator's actual question is whether anything is still serving their
// sessions, and the answer is no — unloading the unit stops the process it was
// supervising. Running it twice is success, because the desired state is
// already reached.
func cmdServiceUninstall() error {
	before := readServiceState()
	if !before.Installed && !before.Loaded {
		fmt.Println("no service unit installed; nothing to do")
		return nil
	}
	if err := uninstallService(); err != nil {
		return err
	}
	after := readServiceState()
	if after.Loaded || after.Installed {
		return fmt.Errorf("the unit is still present after uninstall (loaded=%v on disk=%v at %s)",
			after.Loaded, after.Installed, before.UnitPath)
	}
	fmt.Println("uninstalled", before.UnitPath)
	if before.PID > 0 {
		fmt.Printf("  the supervised server (pid %d) was stopped with the unit.\n", before.PID)
	} else {
		fmt.Println("  the unit was not running anything.")
	}
	fmt.Println("  NOTHING is serving now. Start one by hand with `herdr-expose daemon`,")
	fmt.Println("  or reinstall the unit with `herdr-expose service install`.")
	return nil
}

// exposeManagerFor builds a tunnel manager from config.
func exposeManagerFor(cfg *config.Config, state string) *expose.Manager {
	return exposeManagerLogging(cfg, state, func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, f+"\n", a...)
	})
}

// exposeManagerLogging is the same manager with its narration routed
// somewhere specific. The daemon sends it to the LOG — tunnel provisioning
// steps and teardown are exactly what you want to read back afterwards — and
// `doctor` sends it to a no-op, because doctor's output is the report.
func exposeManagerLogging(cfg *config.Config, state string, logf func(string, ...any)) *expose.Manager {
	root := os.Getenv("HERDR_PLUGIN_ROOT")
	if root == "" {
		root, _ = os.Getwd()
	}
	return expose.New(expose.Options{
		Port: cfg.Port(), Expose: cfg.Expose, Root: root, StateDir: state,
		Logf: logf,
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
		// supervisor simply restarts the tunnel a second later. Writing it
		// twice is harmless, which is the whole of `stop` being idempotent:
		// the flag is a desired state, not an event.
		if err := os.WriteFile(haltFilePath(state), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			return err
		}
		if err := stopMainTunnel(cfg, state); err != nil {
			return err
		}
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
		// STOP BEFORE DESTROY, and halt before both.
		//
		// Without the halt flag this verb is actively harmful rather than
		// merely non-idempotent: the running daemon's supervisor re-provisions
		// within five seconds, so `destroy` deletes the tunnel and the DNS
		// record and then watches them be recreated — leaving a NEW tunnel id
		// and a rewritten record, which is worse than having done nothing.
		// With it, destroy converges: run it twice and the second run finds
		// nothing to delete and says so.
		if err := os.WriteFile(haltFilePath(state), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Println("raised the exposure halt flag so the daemon cannot re-provision underneath this")
		if err := stopMainTunnel(cfg, state); err != nil {
			return err
		}
		fp := mgr.Footprint()
		fmt.Println("destroying:", fp.Describe())
		if err := mgr.Destroy(ctx); err != nil {
			return err
		}
		// Verify against the source of truth rather than reporting intent.
		if fp.NamedTunnel != "" || fp.DNSRecord != "" {
			dnsGone, tunGone, rec, verr := expose.VerifyGone(ctx, mgr.CloudflareOptions())
			if verr != nil {
				return fmt.Errorf("destroy ran but could not be verified: %w", verr)
			}
			if !dnsGone || !tunGone {
				return fmt.Errorf("destroy did NOT complete: dns_gone=%v tunnel_gone=%v (record %+v)",
					dnsGone, tunGone, rec)
			}
			fmt.Printf("verified against the API: DNS record %s gone, tunnel %q gone\n",
				fp.DNSRecord, fp.NamedTunnel)
		}
		// The generated config and credentials are part of the footprint, so
		// they go too. Symmetry is the point: what a start created, a destroy
		// removes, and nothing else.
		_ = os.RemoveAll(filepath.Join(state, "cloudflare"))
		fmt.Println("re-arm with: herdr-expose expose start")
		return nil
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
// stopMainTunnel takes down whatever process carries the PERMANENT deployment,
// and is provider-aware rather than assuming cloudflared.
//
// Only the cloudflare provider leaves a process this CLI can identify and kill
// from outside (matched on the config path WE generated, never on a binary
// name, so it can never reach somebody else's tunnel). For a JS adapter the
// child belongs to the daemon, and the halt flag the caller has already raised
// is what stops it — within one supervisor tick. Saying which of the two
// happened beats a cheerful message that fits only one provider.
//
// It is idempotent: with nothing running, every branch is already in the
// desired state and returns nil.
func stopMainTunnel(cfg *config.Config, state string) error {
	cfConfig := filepath.Join(state, "cloudflare", "config.yml")
	if cfg.Expose.Provider() == config.ProviderCloudflare || countCloudflared(cfConfig) > 0 {
		killStrayCloudflared(cfConfig)
		if !waitForNoCloudflared(cfConfig, 20*time.Second) {
			return errors.New("cloudflared is STILL running for the main tunnel")
		}
		fmt.Println("main tunnel stopped (DNS record and tunnel left in place — static domain)")
		return nil
	}
	fmt.Printf("halt flag raised; the daemon stops the %s exposure within ~5s "+
		"(check with `herdr-expose status`)\n", cfg.Expose.Provider())
	return nil
}

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
			log.Info("tunnel provisioning", "mode", mgr.Resolution().Mode, "url", mgr.Resolution().URL)
			if st, err := mgr.Start(ctx); err != nil {
				log.Warn("exposure did not start", "err", err)
			} else {
				log.Info("tunnel provisioned", "url", st.URL, "healthy", st.Healthy)
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
