package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muthuishere/herdr-expose/internal/core"
)

// Config is the small slice of configuration the serve layer needs.
//
// Workstream C owns internal/config and implements this; serve deliberately
// does not import it, and deliberately has no Token() accessor — auth secrets
// live hashed in state, never in config (SPEC amendment B5).
type Config interface {
	Port() int
	Bind() string
	AllowedOrigins() []string
	UI() map[string]any
}

// Exposure is the optional tunnel status the UI surfaces. Workstream C
// implements it; nil means "no exposure configured".
type Exposure interface {
	Status() (url string, healthy bool)
}

// Server is the HTTP + WebSocket surface.
type Server struct {
	Version string

	cfg      Config
	hub      *core.Hub
	auth     *Auth
	log      Logger
	slog     *slog.Logger
	static   fs.FS
	exposure Exposure
	upgrader websocket.Upgrader

	http  *http.Server
	addr  string
	local localMode
}

// Options configures a Server.
type Options struct {
	Version  string
	Config   Config
	Hub      *core.Hub
	Auth     *Auth
	Log      *slog.Logger
	Static   fs.FS // may be nil; workstream D injects the embedded web/dist
	Exposure Exposure
}

// New builds a server. It never binds a non-loopback address.
func New(o Options) (*Server, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	bind := o.Config.Bind()
	if !isLoopback(bind) {
		return nil, fmt.Errorf("serve: refusing to bind %q; loopback only, exposure is always a tunnel", bind)
	}
	s := &Server{
		Version:  o.Version,
		cfg:      o.Config,
		hub:      o.Hub,
		auth:     o.Auth,
		log:      o.Log,
		slog:     o.Log,
		static:   o.Static,
		exposure: o.Exposure,
		addr:     net.JoinHostPort(bind, fmt.Sprint(o.Config.Port())),
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		EnableCompression: true, // permessage-deflate, context takeover left ON
		CheckOrigin: func(r *http.Request) bool {
			// A browser ALWAYS sends Origin on a WS upgrade. A missing Origin
			// is therefore a non-browser client, which we allow only when it
			// also carries a token (checked in handleStream before the
			// upgrade). It is never defaulted-allow for a browser.
			o := r.Header.Get("Origin")
			if o == "" {
				return true
			}
			return s.originAllowed(r, o)
		},
	}
	s.http = &http.Server{
		Addr:              s.addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

func isLoopback(bind string) bool {
	if bind == "localhost" {
		return true
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

// Addr is the listen address.
func (s *Server) Addr() string { return s.addr }

// LocalURL is the address a tunnel should point at.
func (s *Server) LocalURL() string { return "http://" + s.addr }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/config", s.handleConfig)
	mux.HandleFunc("/v1/pair", s.handlePair)
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/stream", s.handleStream)
	mux.Handle("/", s.staticHandler())
	return s.securityHeaders(mux)
}

// securityHeaders applies the hardening from SPEC amendment B5 and checks the
// Origin allowlist BEFORE echoing anything into a CORS header.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")

		// Host pinning. A DNS-rebound name arrives here and will not match,
		// which is precisely what stops rebinding against a local server that
		// executes commands. /healthz is exempt so a tunnel health check works.
		if r.URL.Path != "/healthz" && !s.hostAllowed(r) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}

		origin := r.Header.Get("Origin")
		if origin != "" {
			if !s.originAllowed(r, origin) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			// Never reflect an arbitrary origin: only one we just allowlisted.
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// A state-changing request with no Origin is not a browser request
			// we can vouch for. /v1/pair is the one exception: it is reached by
			// a freshly-scanned phone before any origin is established, and it
			// is already rate-limited and single-use.
			if r.URL.Path != "/v1/pair" {
				http.Error(w, "origin required", http.StatusForbidden)
				return
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed pins the Host header. In local mode only the loopback forms are
// accepted; when a tunnel hostname is configured that is accepted too.
func (s *Server) hostAllowed(r *http.Request) bool {
	if s.local.hostPinned(r) {
		return true
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, o := range s.cfg.AllowedOrigins() {
		o = strings.ToLower(strings.TrimSpace(o))
		o = strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
		if h, _, err := net.SplitHostPort(o); err == nil {
			o = h
		}
		if o != "" && o == host {
			return true
		}
	}
	return false
}

// originAllowed applies the allowlist. In local mode the loopback origins are
// the allowlist; otherwise the configured origins are.
func (s *Server) originAllowed(r *http.Request, origin string) bool {
	if s.local.originPinned(origin) {
		return true
	}
	return s.auth.OriginAllowed(origin)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	t := s.hub.Store().Tree()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"version":        s.Version,
		"api":            APIVersion,
		"upstream":       t.Connected,
		"herdr_version":  t.Version,
		"herdr_protocol": t.Protocol,
		"tree_rev":       t.Rev,
		"web_ui":         s.static != nil,
	})
}

// handleConfig is the client bootstrap. It contains NO secrets, by construction:
// the only secrets in this system are hashes in state.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"api":     APIVersion,
		"version": s.Version,
		"stream":  "/v1/stream",
		"ui":      s.cfg.UI(),
		"limits": map[string]any{
			"min_cols": core.MinCols, "min_rows": core.MinRows,
		},
	}
	if s.exposure != nil {
		if url, healthy := s.exposure.Status(); url != "" {
			out["exposure"] = map[string]any{"url": url, "healthy": healthy}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMetrics reports the measured latency of the two budgeted paths.
// Authenticated: it tells an attacker how busy the machine is.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.local.bypassAuth(r) {
		if _, err := s.auth.Authenticate(BearerFrom(r), RemoteIP(r), r.UserAgent()); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.hub.Metrics().Snapshot())
}

// handlePair exchanges a one-time pairing code for a long-lived device token.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token, dev, err := s.auth.RedeemPairing(body.Code, body.Name, RemoteIP(r), r.UserAgent())
	if err != nil {
		// Never distinguish expired / wrong / already-used.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"device":     dev,
		"expires_in": int(DeviceTTL.Seconds()),
	})
}

// staticHandler serves the SPA from an injected fs.FS.
//
// This layer does NOT go:embed anything: workstream D owns the build and wires
// the embedded FS in. A nil FS is a normal, working state (an API-only server).
func (s *Server) staticHandler() http.Handler {
	if s.static == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(noWebUIPage))
		})
	}
	files := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "/" {
			clean = "index.html"
		}
		f, err := s.static.Open(clean)
		if err != nil {
			// SPA fallback: unknown routes render the app shell.
			s.serveIndex(w, r)
			return
		}
		_ = f.Close()

		switch {
		case clean == "index.html" || clean == "sw.js":
			w.Header().Set("Cache-Control", "no-cache")
		case looksHashed(clean):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// looksHashed recognises Vite's content-hashed filenames (name-<hash>.ext).
func looksHashed(name string) bool {
	base := path.Base(name)
	i := strings.LastIndexByte(base, '-')
	if i < 0 {
		return false
	}
	rest := base[i+1:]
	dot := strings.IndexByte(rest, '.')
	if dot < 8 {
		return false
	}
	for _, c := range rest[:dot] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_') {
			return false
		}
	}
	return true
}

const noWebUIPage = `<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>herdr-expose</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;
min-height:100dvh;background:#101014;color:#e6e6ea;padding:24px}
main{max-width:34rem}code{background:#1e1e26;padding:.15em .4em;border-radius:4px}</style>
<main><h1>web UI not built</h1>
<p>The server is running and the API is live, but no web bundle was embedded in
this binary.</p>
<p>Build it with <code>npm --prefix web ci &amp;&amp; npm --prefix web run build</code>,
then rebuild the binary.</p>
<p><code>GET /healthz</code> and <code>GET /v1/stream</code> work regardless.</p>
</main>`

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve listens and serves until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	// Derived from the LISTENER, never from a header (SPEC F1).
	s.local = newLocalMode(ln)
	if s.local.enabled {
		s.log.Info("local mode: loopback listener, device token not required",
			"origin_pinning", true, "host_pinning", true)
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(sctx)
	}()
	s.log.Info("herdr-expose listening", "addr", s.LocalURL())
	if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
