package upstream

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/herdr-expose/internal/platform"
)

// A Herdr SESSION is a whole server with its own socket, its own workspace tree
// and its own pane id space. The owner runs many concurrently and wants all of
// them in one UI, so the plugin is NOT bound to $HERDR_SOCKET_PATH — that is
// merely the session it happened to be launched from.
//
// There is no session-listing method on the socket API (verified against the
// 0.9.0 schema: the only `session.*` request is session.snapshot, which returns
// ONE session's tree). So discovery shells out to the resolved absolute herdr
// binary: `herdr session list --json` ->
//
//	{"sessions":[{"default":true,"name":"default","running":false,
//	  "session_dir":"...","socket_path":"/Users/me/.config/herdr/herdr.sock"}, ...]}
//
// A filesystem scan is the fallback when the CLI is unavailable.

// Session is one Herdr server: a name, a socket and whether it is up.
type Session struct {
	Name       string `json:"name"`
	SocketPath string `json:"socket_path"`
	SessionDir string `json:"session_dir"`
	Running    bool   `json:"running"`
	Default    bool   `json:"default"`
}

type sessionListResult struct {
	Sessions []Session `json:"sessions"`
}

// ListSessions enumerates every Herdr session known to this machine.
func ListSessions(ctx context.Context) ([]Session, error) {
	bin, err := ResolveHerdrBin()
	if err != nil {
		return scanSessions(), err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "session", "list", "--json")
	// A named session in the environment must not bias a machine-wide listing.
	cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH=")
	out, err := cmd.Output()
	if err != nil {
		if s := scanSessions(); len(s) > 0 {
			return s, nil
		}
		return nil, err
	}
	var res sessionListResult
	if err := json.Unmarshal(out, &res); err != nil {
		if s := scanSessions(); len(s) > 0 {
			return s, nil
		}
		return nil, err
	}
	// Ground truth for "running" is a socket that accepts a connection. If the
	// CLI reports nothing running, check the sockets ourselves before believing
	// it — the CLI resolves its session root from $HOME, so a service unit or a
	// relocated config dir can leave it looking in the wrong place while the
	// servers are up and answering.
	if len(RunningSessions(res.Sessions)) == 0 {
		if scanned := RunningSessions(scanSessions()); len(scanned) > 0 {
			return mergeSessions(res.Sessions, scanned), nil
		}
	}
	sortSessions(res.Sessions)
	return res.Sessions, nil
}

// mergeSessions overlays probed-live sessions onto a CLI listing.
func mergeSessions(base, live []Session) []Session {
	out := append([]Session(nil), base...)
	for _, l := range live {
		found := false
		for i := range out {
			if out[i].Name == l.Name {
				out[i] = l
				found = true
				break
			}
		}
		if !found {
			out = append(out, l)
		}
	}
	sortSessions(out)
	return out
}

func sortSessions(s []Session) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Running != s[j].Running {
			return s[i].Running // running first
		}
		return s[i].Name < s[j].Name
	})
}

// RunningSessions filters a listing down to the live servers.
func RunningSessions(all []Session) []Session {
	out := make([]Session, 0, len(all))
	for _, s := range all {
		if s.Running && s.SocketPath != "" {
			out = append(out, s)
		}
	}
	return out
}

// sessionsRoot is where named sessions live: ~/.config/herdr/sessions/<name>/.
// The unnamed default is ~/.config/herdr/herdr.sock.
func sessionsRoot() (root, def string) {
	base := os.Getenv("HERDR_CONFIG_DIR")
	if base == "" {
		// Herdr's own config dir, which is NOT ~/.config/herdr everywhere:
		// its src/config/io.rs uses %APPDATA%\herdr on Windows.
		b, err := platform.HerdrConfigDir()
		if err != nil {
			return "", ""
		}
		base = b
	}
	return filepath.Join(base, "sessions"), base
}

// scanSessions is the no-CLI fallback: walk the session dirs and probe each
// socket by dialling it. A stale socket file is common after a crash, so
// "the file exists" is not "the server is up".
func scanSessions() []Session {
	root, base := sessionsRoot()
	if base == "" {
		return nil
	}
	var out []Session
	if sock := filepath.Join(base, "herdr.sock"); fileExists(sock) {
		out = append(out, Session{Name: "default", SocketPath: sock,
			SessionDir: base, Default: true, Running: socketAlive(sock)})
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		sock := filepath.Join(dir, "herdr.sock")
		if !fileExists(sock) {
			continue
		}
		out = append(out, Session{Name: e.Name(), SocketPath: sock,
			SessionDir: dir, Running: socketAlive(sock)})
	}
	sortSessions(out)
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func socketAlive(path string) bool {
	c, err := platform.DialControlTimeout(path, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// OriginSession is the session this plugin process was launched from, derived
// from $HERDR_SOCKET_PATH. It is ONE entry among many and gets no privileges;
// the UI only labels it.
func OriginSession() (name, socket string) {
	socket = SocketPath()
	if socket == "" {
		return "", ""
	}
	return SessionNameForSocket(socket), socket
}

// SessionNameForSocket maps a socket path back to its session name.
// ~/.config/herdr/sessions/<name>/herdr.sock -> <name>
// ~/.config/herdr/herdr.sock                 -> default
func SessionNameForSocket(socket string) string {
	dir := filepath.Dir(socket)
	name := filepath.Base(dir)
	if name == "herdr" || name == "." || name == string(filepath.Separator) {
		return "default"
	}
	// guard against ~/.config/herdr/sessions/herdr.sock oddities
	if strings.TrimSpace(name) == "" {
		return "default"
	}
	return name
}

// RegistryInterval is the discovery cadence. Deliberately slow: a session
// appearing or disappearing is a human-scale event, and each poll is a process
// spawn.
const RegistryInterval = 3 * time.Second

// Registry polls for Herdr sessions and reports the current listing.
//
// It owns no clients and no streams — it is discovery only, so a session that
// dies can never disturb the others. The store reacts to the listing.
type Registry struct {
	log      *slog.Logger
	interval time.Duration
	onChange func([]Session)
	// List is the discovery source. Overridable so the store can be tested
	// without a live Herdr on the machine.
	List func(context.Context) ([]Session, error)

	mu  sync.Mutex
	cur []Session
}

// NewRegistry builds a registry. onChange is called on every CHANGE of the
// listing (and once immediately at startup), on the poller goroutine.
func NewRegistry(log *slog.Logger, onChange func([]Session)) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{log: log, interval: RegistryInterval, onChange: onChange, List: ListSessions}
}

// SetInterval overrides the poll cadence (tests).
func (r *Registry) SetInterval(d time.Duration) {
	if d > 0 {
		r.interval = d
	}
}

// Sessions returns the last known listing.
func (r *Registry) Sessions() []Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Session(nil), r.cur...)
}

// Run polls until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) {
	r.poll(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.poll(ctx)
		}
	}
}

func (r *Registry) poll(ctx context.Context) {
	list, err := r.List(ctx)
	if err != nil && len(list) == 0 {
		r.log.Warn("session discovery failed", "err", err)
		return
	}
	r.mu.Lock()
	changed := !sameSessions(r.cur, list)
	if changed {
		r.cur = list
	}
	r.mu.Unlock()
	if changed && r.onChange != nil {
		r.onChange(append([]Session(nil), list...))
	}
}

func sameSessions(a, b []Session) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
