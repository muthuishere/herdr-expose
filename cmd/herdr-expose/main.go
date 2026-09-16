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
		err = cmdServe()
	case "daemon":
		err = cmdDaemon()
	case "status":
		err = cmdStatus()
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
  status                show server, upstream, devices and exposure state
  stop                  stop a running server
  pair [--name NAME]    mint a one-time pairing code and show its QR LOCALLY
  install-service       install and load the launchd / systemd --user unit
  uninstall-service     remove it
  devices [--revoke ID] list or revoke paired devices
  service install|uninstall|status
                        install a supervised unit (launchd / systemd --user)
  expose start|stop|status|plan|destroy
                        tunnel control
`)
}

func newLogger() *slog.Logger {
	lvl := slog.LevelInfo
	if os.Getenv("HERDR_EXPOSE_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// cfgAdapter adapts internal/config (workstream C) onto the narrow interface
// internal/serve consumes, so neither package depends on the other.
type cfgAdapter struct{ c *config.Config }

func (a cfgAdapter) Port() int    { return a.c.Port() }
func (a cfgAdapter) Bind() string { return a.c.Bind() }
func (a cfgAdapter) AllowedOrigins() []string {
	return a.c.Server.AllowedOrigins
}
func (a cfgAdapter) UI() map[string]any {
	return map[string]any{"theme": a.c.UI.Theme, "default_view": a.c.UI.DefaultView}
}

func cmdServe() error {
	log := newLogger()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	state, err := StateDir()
	if err != nil {
		return err
	}

	// Layer 3: never fight a manager for the port.
	addr := fmt.Sprintf("%s:%d", cfg.Bind(), cfg.Port())
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
	if upstream.SocketPath() == "" {
		return errors.New("HERDR_SOCKET_PATH is not set; run inside a Herdr session or export it")
	}

	auth, err := serve.NewAuth(state, log, cfg.Server.AllowedOrigins)
	if err != nil {
		return err
	}
	if tok, created, err := auth.EnsureServerToken(); err != nil {
		return err
	} else if created {
		// Shown exactly once: only the SHA-256 is stored.
		fmt.Fprintf(os.Stderr, "\n  herdr-expose server token (shown once):\n\n    %s\n\n", tok)
		fmt.Fprintf(os.Stderr, "  open: http://%s/?token=%s\n\n", addr, tok)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := upstream.NewClient("")
	store := core.NewStore(client, log)
	go store.Run(ctx)
	hub := core.NewHub(store, log)
	go hub.Run(ctx)

	srv, err := serve.New(serve.Options{
		Version: version,
		Config:  cfgAdapter{cfg},
		Hub:     hub,
		Auth:    auth,
		Log:     log,
		Static:  webFS(), // nil unless workstream D wired an embedded bundle
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

func cmdStatus() error {
	state, err := StateDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", cfg.Bind(), cfg.Port())
	fmt.Printf("version   %s\nconfig    %s\nstate     %s\nlisten    http://%s\n",
		version, cfg.Path(), state, addr)
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
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--name" {
			name = args[i+1]
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

	// Point the phone at the tunnel origin when there is one, so the scan lands
	// straight on the PWA. Fall back to loopback for same-machine pairing.
	origin := fmt.Sprintf("http://%s:%d", cfg.Bind(), cfg.Port())
	if cfg.Expose.Domain != "" {
		origin = "https://" + cfg.Expose.Domain
	} else if mgr := exposeManager(cfg); mgr != nil {
		if u := mgr.URL(); u != "" {
			origin = strings.TrimRight(u, "/")
		}
	}
	url := fmt.Sprintf("%s/?pair=%s", origin, code)

	fmt.Printf("\n  scan this on the phone — it is shown ONLY here, never over the tunnel\n\n")
	printQR(url)
	fmt.Printf("\n  pairing code: %s   (for %q)\n  valid until:  %s\n  url:          %s\n\n",
		code, name, exp.Format(time.Kitchen), url)
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

// exposeManager builds a tunnel manager from config.
func exposeManager(cfg *config.Config) *expose.Manager {
	state, _ := StateDir()
	root := os.Getenv("HERDR_PLUGIN_ROOT")
	if root == "" {
		root, _ = os.Getwd()
	}
	return expose.New(expose.Options{
		Port: cfg.Port(), Expose: cfg.Expose, Root: root, StateDir: state,
		Logf: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})
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

	switch sub {
	case "start":
		st, err := mgr.Start(ctx)
		if err != nil {
			return err
		}
		printJSON(st)
		return nil
	case "stop":
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
