// Package config owns the on-disk TOML configuration for herdr-expose.
//
// Design rules (SPEC.md §5 as amended by B5 and B6):
//
//   - Path is ~/.config/herdr-expose/config.toml, UNCONDITIONALLY.
//     $HERDR_PLUGIN_CONFIG_DIR is deliberately NOT honoured (B6): Herdr only
//     sets it when Herdr itself launches the plugin, so honouring it would give
//     the tool two different configs depending on how it was started.
//     $HERDR_PLUGIN_STATE_DIR remains the right place for pidfile and state.
//   - The file is created with commented defaults on first run, 0600.
//   - NO SECRETS LIVE HERE (B5). The server token and the per-device tokens are
//     SHA-256 hashes in the state file owned by internal/serve. This package
//     never reads, writes, or holds a plaintext credential.
//   - FORWARD COMPATIBILITY IS THE POINT. Keys we do not understand are kept
//     verbatim on rewrite, keys that are missing take defaults, and an old
//     config must load on a newer binary without error.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Defaults, mirrored by defaultFileTemplate below.
const (
	DefaultPort = 21118
	DefaultBind = "127.0.0.1"
	// B5: 60s is hostile on a phone. Ten minutes.
	DefaultPairingTTLSeconds = 600
	DefaultMaxDevices        = 32
	DefaultDeviceTTLDays     = 30
	DefaultPairAttempts      = 10
	DefaultHandshakeAttempts = 60
	DefaultTheme             = "auto"
	DefaultView              = "grid"
)

// Mode is the resolved exposure mode (AMENDMENT E1). It is decided at startup
// by internal/expose — never by a user-set bind address.
type Mode string

const (
	ModeCloudflare Mode = "cloudflare" // public static domain; binds loopback
	ModeNgrok      Mode = "ngrok"      // public reserved domain; binds loopback
	ModeJS         Mode = "js"         // JS adapter escape hatch; binds loopback
	ModeLAN        Mode = "lan"        // binds 0.0.0.0; anyone on the wifi can reach the port
	ModeLocal      Mode = "local"      // binds loopback; the default
)

// The only two bind addresses this binary will ever use.
const (
	BindLoopback = "127.0.0.1"
	BindAll      = "0.0.0.0"
)

// Server is the [server] table.
type Server struct {
	Port int `toml:"port" json:"port"`

	// Bind is NOT user-settable (E1): the resolved exposure mode decides it.
	// A `bind` key in the file is rejected at load time. The field holds the
	// resolved address so that existing consumers keep working.
	Bind string `toml:"bind" json:"bind"`

	// AllowedOrigins is the browser Origin allowlist checked BEFORE any CORS
	// header is echoed (B5). Empty means "the tunnel hostname and loopback
	// only", which internal/serve derives at runtime.
	AllowedOrigins []string `toml:"allowed_origins" json:"allowed_origins"`
}

// Auth is the [auth] table: policy knobs only.
//
// There is deliberately no token key. Per B5 the server token and every device
// token exist only as SHA-256 hashes in the 0600 state file; a plaintext
// credential must never be written to, or read from, this file.
type Auth struct {
	// PairingTTLSeconds is how long a 6-char pairing code stays valid.
	PairingTTLSeconds int `toml:"pairing_ttl_seconds" json:"pairing_ttl_seconds"`
	// MaxDevices caps paired devices; the least recently seen is evicted.
	MaxDevices int `toml:"max_devices" json:"max_devices"`
	// DeviceTTLDays is the sliding expiry of a device token.
	DeviceTTLDays int `toml:"device_ttl_days" json:"device_ttl_days"`
	// PairAttemptsPerMinute rate-limits /v1/pair per remote address.
	PairAttemptsPerMinute int `toml:"pair_attempts_per_minute" json:"pair_attempts_per_minute"`
	// HandshakeAttemptsPerMinute rate-limits the websocket handshake per address.
	HandshakeAttemptsPerMinute int `toml:"handshake_attempts_per_minute" json:"handshake_attempts_per_minute"`
}

// UI is the [ui] table.
type UI struct {
	Theme       string `toml:"theme" json:"theme"`
	DefaultView string `toml:"default_view" json:"default_view"`
}

// Adapter is one [[expose.adapters]] entry.
type Adapter struct {
	ID     string            `toml:"id" json:"id"`
	Script string            `toml:"script" json:"script"`
	Env    map[string]string `toml:"env" json:"env"`
}

// Expose is the [expose] table.
//
// The happy path (SPEC AMENDMENT A2) is two lines:
//
//	[expose]
//	cloudflare = true
//	domain = "herdr.deemwar.com"   # REQUIRED
//
// `adapter` is the escape hatch: naming a JS adapter wins over the built-ins.
type Expose struct {
	Cloudflare bool `toml:"cloudflare" json:"cloudflare"`
	Ngrok      bool `toml:"ngrok" json:"ngrok"`

	// LAN allows binding 0.0.0.0 so phones and laptops on the same wifi can
	// reach the server directly. It is also the automatic fallback when a
	// tunnel was requested but its binary is not installed (E1).
	//
	// Safe only because auth is mandatory in every mode and the pairing code is
	// shown ONLY on this machine: no HTTP endpoint mints or displays one, so a
	// neighbour on the network can reach the port and get nowhere.
	LAN bool `toml:"lan" json:"lan"`

	// Domain is REQUIRED whenever a provider is enabled (C1). There is no
	// ephemeral/quick tunnel path: a hostname that changes on restart breaks
	// PWA installs, bookmarks and origin-bound device tokens.
	Domain string `toml:"domain" json:"domain"`
	// TunnelName names the Cloudflare named tunnel; `tunnel` is accepted as a
	// legacy alias.
	TunnelName string `toml:"tunnel_name" json:"tunnel_name"`
	Tunnel     string `toml:"tunnel" json:"-"`
	Autostart  bool   `toml:"autostart" json:"autostart"`

	// Enabled is the legacy master switch from the pre-amendment spec. It is
	// kept so old configs keep working: when it is present and false it
	// suppresses autostart. A nil value means "not specified".
	Enabled *bool `toml:"enabled" json:"enabled,omitempty"`

	// Adapter selects a JS adapter by id instead of a built-in provider.
	Adapter  string    `toml:"adapter" json:"adapter"`
	Adapters []Adapter `toml:"adapters" json:"adapters"`
}

// Provider identifies which exposure implementation the config selects.
type ProviderKind string

const (
	ProviderNone       ProviderKind = "none"
	ProviderCloudflare ProviderKind = "cloudflare"
	ProviderNgrok      ProviderKind = "ngrok"
	ProviderJS         ProviderKind = "js"
)

// Provider resolves the selection rules: an explicit JS adapter wins, then
// cloudflare, then ngrok.
func (e Expose) Provider() ProviderKind {
	switch {
	case strings.TrimSpace(e.Adapter) != "":
		return ProviderJS
	case e.Cloudflare:
		return ProviderCloudflare
	case e.Ngrok:
		return ProviderNgrok
	default:
		return ProviderNone
	}
}

// TunnelID is the named tunnel to create or reuse.
func (e Expose) TunnelID() string {
	if n := strings.TrimSpace(e.TunnelName); n != "" {
		return n
	}
	if n := strings.TrimSpace(e.Tunnel); n != "" {
		return n
	}
	return "herdr-expose"
}

// ShouldAutostart reports whether the server brings the tunnel up on boot.
func (e Expose) ShouldAutostart() bool {
	if e.Enabled != nil && !*e.Enabled {
		return false
	}
	return e.Autostart && e.Provider() != ProviderNone
}

// Config is the whole file. `raw` holds the decoded document as-is, including
// every key this binary has never heard of, so Save can put them back.
type Config struct {
	Server Server `toml:"server" json:"server"`
	Auth   Auth   `toml:"auth" json:"-"`
	UI     UI     `toml:"ui" json:"ui"`
	Expose Expose `toml:"expose" json:"expose"`

	path string
	raw  map[string]any
}

// Path returns the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// Port implements the Provider interface.
func (c *Config) Port() int { return c.Server.Port }

// Bind implements the Provider interface. On a bare Config this is the
// default; the live value comes from the Store, which internal/expose updates
// with the resolved mode.
func (c *Config) Bind() string { return c.Server.Bind }

// Mode implements the Provider interface. A Config on its own does not know
// whether cloudflared exists, so it reports the configured intent; the Store
// reports the resolved mode.
func (c *Config) Mode() string {
	switch c.Expose.Provider() {
	case ProviderCloudflare:
		return string(ModeCloudflare)
	case ProviderNgrok:
		return string(ModeNgrok)
	case ProviderJS:
		return string(ModeJS)
	}
	if c.Expose.LAN {
		return string(ModeLAN)
	}
	return string(ModeLocal)
}

// AdapterByID returns the configured adapter with the given id.
func (c *Config) AdapterByID(id string) (Adapter, bool) {
	for _, a := range c.Expose.Adapters {
		if a.ID == id {
			return a, true
		}
	}
	return Adapter{}, false
}

// Defaults returns a Config with every documented key at its default value.
func Defaults() *Config {
	return &Config{
		Server: Server{Port: DefaultPort, Bind: DefaultBind},
		Auth: Auth{
			PairingTTLSeconds:          DefaultPairingTTLSeconds,
			MaxDevices:                 DefaultMaxDevices,
			DeviceTTLDays:              DefaultDeviceTTLDays,
			PairAttemptsPerMinute:      DefaultPairAttempts,
			HandshakeAttemptsPerMinute: DefaultHandshakeAttempts,
		},
		UI:     UI{Theme: DefaultTheme, DefaultView: DefaultView},
		Expose: Expose{},
		raw:    map[string]any{},
	}
}

// Dir is ~/.config/herdr-expose, always. See B6: $HERDR_PLUGIN_CONFIG_DIR is
// intentionally ignored so that the same machine always has exactly one config,
// whether herdr-expose was started from a shell or from a Herdr pane.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "herdr-expose"), nil
}

// DefaultPath is Dir()/config.toml.
func DefaultPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

// Load reads the config at the default path, creating it (with commented
// defaults and a freshly minted token) when it does not exist.
func Load() (*Config, error) {
	p, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return LoadFrom(p)
}

// LoadFrom reads the config at path, creating it on first run.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		if err := create(path); err != nil {
			return nil, err
		}
		if data, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.path = path

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// parse decodes TOML twice: once into the typed struct (missing keys keep the
// defaults already in place) and once into a free-form map that retains every
// unknown key for Save.
func parse(data []byte) (*Config, error) {
	cfg := Defaults()
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	raw := map[string]any{}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	cfg.raw = raw
	// An explicit empty [[expose.adapters]] list must not be silently
	// repopulated from the defaults, but a file that never mentions adapters
	// keeps the default entry.
	if _, ok := rawTable(raw, "expose")["adapters"]; ok && cfg.Expose.Adapters == nil {
		cfg.Expose.Adapters = []Adapter{}
	}
	return cfg, nil
}

// Validate enforces the invariants the rest of the binary relies on. The
// loopback check is the important one: the HTTP surface is a remote code
// execution surface, and the tunnel adapter is the only sanctioned remote path.
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is out of range (1-65535)", c.Server.Port)
	}
	if err := ValidateBind(c.Server.Bind); err != nil {
		return err
	}
	// `bind` is no longer settable: the resolved mode decides it (E1). A legacy
	// loopback value is accepted and ignored so old configs keep loading, but
	// an attempt to widen the listen address is refused and pointed at `lan`.
	if raw, explicit := rawTable(c.raw, "server")["bind"]; explicit {
		val, _ := raw.(string)
		if ip := net.ParseIP(strings.Trim(strings.TrimSpace(val), "[]")); val != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("server.bind is no longer settable (you have %q): the exposure mode decides the "+
				"bind address. Remove the key and set `lan = true` under [expose] if you want devices on your "+
				"wifi to reach this machine directly — auth is still required in every mode", val)
		}
	}
	if c.Auth.PairingTTLSeconds <= 0 {
		return fmt.Errorf("auth.pairing_ttl_seconds must be positive, got %d", c.Auth.PairingTTLSeconds)
	}
	if c.Auth.MaxDevices <= 0 {
		return fmt.Errorf("auth.max_devices must be positive, got %d", c.Auth.MaxDevices)
	}
	if c.Auth.DeviceTTLDays <= 0 {
		return fmt.Errorf("auth.device_ttl_days must be positive, got %d", c.Auth.DeviceTTLDays)
	}
	switch c.UI.DefaultView {
	case "grid", "focus":
	default:
		return fmt.Errorf("ui.default_view %q is not one of grid|focus", c.UI.DefaultView)
	}
	seen := map[string]bool{}
	for i, a := range c.Expose.Adapters {
		if strings.TrimSpace(a.ID) == "" {
			return fmt.Errorf("expose.adapters[%d] has no id", i)
		}
		if seen[a.ID] {
			return fmt.Errorf("expose.adapters: duplicate id %q", a.ID)
		}
		seen[a.ID] = true
		if strings.TrimSpace(a.Script) == "" {
			return fmt.Errorf("expose.adapters[%q] has no script", a.ID)
		}
	}
	if a := strings.TrimSpace(c.Expose.Adapter); a != "" && !seen[a] {
		return fmt.Errorf("expose.adapter %q has no matching [[expose.adapters]] entry", a)
	}
	if c.Expose.Cloudflare && c.Expose.Ngrok {
		return fmt.Errorf("expose: set either cloudflare or ngrok, not both")
	}
	d := strings.TrimSpace(c.Expose.Domain)
	if d != "" {
		if strings.Contains(d, "/") || strings.Contains(d, ":") || !strings.Contains(d, ".") {
			return fmt.Errorf("expose.domain %q must be a bare hostname such as \"herdr.example.com\"", d)
		}
	}
	// C1: a static domain is mandatory. There is no random-hostname fallback.
	if d == "" {
		switch c.Expose.Provider() {
		case ProviderCloudflare:
			return fmt.Errorf("expose.cloudflare = true requires expose.domain, e.g. domain = \"herdr.example.com\" " +
				"(there is no quick/ephemeral tunnel: a hostname that changes on restart breaks PWA installs, " +
				"bookmarks and device tokens)")
		case ProviderNgrok:
			return fmt.Errorf("expose.ngrok = true requires expose.domain set to a RESERVED ngrok domain, " +
				"e.g. domain = \"herdr.ngrok.app\" (random ngrok URLs are not supported)")
		}
	}
	return nil
}

// ValidateBind refuses any address that is not loopback.
// It stays exported because internal/expose asserts the resolved address is
// still one of the two sanctioned values before the listener is created.
func ValidateBind(bind string) error {
	b := strings.TrimSpace(bind)
	if b == "" {
		return fmt.Errorf("server.bind is empty; it must be a loopback address such as %q", DefaultBind)
	}
	if b == "localhost" {
		return nil
	}
	ip := net.ParseIP(strings.Trim(b, "[]"))
	if ip == nil {
		return fmt.Errorf("server.bind %q is not an IP address; set `lan = true` under [expose] to listen on "+
			"your LAN, or leave it unset for loopback (%q)", bind, DefaultBind)
	}
	if b == BindAll {
		// 0.0.0.0 is legitimate, but only as the RESOLVED value of lan mode.
		return nil
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("server.bind %q is not a loopback address and not 0.0.0.0; herdr-expose binds either "+
			"loopback or, in lan mode, every interface. Set `lan = true` under [expose] instead of naming an "+
			"address", bind)
	}
	return nil
}

// Save writes the config back to disk with 0600 permissions. Unknown keys read
// at load time are written back untouched; only the keys this binary knows
// about are overwritten from the struct.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config has no path; use SaveTo")
	}
	return c.SaveTo(c.path)
}

// SaveTo writes the merged document to path.
func (c *Config) SaveTo(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	doc := c.merged()
	out, err := toml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return writeFile0600(path, out)
}

// merged overlays the known keys onto the raw document read from disk.
func (c *Config) merged() map[string]any {
	doc := cloneMap(c.raw)

	server := rawTable(doc, "server")
	server["port"] = int64(c.Server.Port)
	// bind is resolved, not configured: never write it back into the file.
	delete(server, "bind")
	if len(c.Server.AllowedOrigins) > 0 {
		origins := make([]any, 0, len(c.Server.AllowedOrigins))
		for _, o := range c.Server.AllowedOrigins {
			origins = append(origins, o)
		}
		server["allowed_origins"] = origins
	} else {
		delete(server, "allowed_origins")
	}
	doc["server"] = server

	auth := rawTable(doc, "auth")
	// Defensive: if an ancient config still carries a plaintext token, drop it
	// on rewrite rather than copying a secret forward.
	delete(auth, "token")
	auth["pairing_ttl_seconds"] = int64(c.Auth.PairingTTLSeconds)
	auth["max_devices"] = int64(c.Auth.MaxDevices)
	auth["device_ttl_days"] = int64(c.Auth.DeviceTTLDays)
	auth["pair_attempts_per_minute"] = int64(c.Auth.PairAttemptsPerMinute)
	auth["handshake_attempts_per_minute"] = int64(c.Auth.HandshakeAttemptsPerMinute)
	doc["auth"] = auth

	ui := rawTable(doc, "ui")
	ui["theme"] = c.UI.Theme
	ui["default_view"] = c.UI.DefaultView
	doc["ui"] = ui

	expose := rawTable(doc, "expose")
	expose["cloudflare"] = c.Expose.Cloudflare
	expose["ngrok"] = c.Expose.Ngrok
	expose["lan"] = c.Expose.LAN
	expose["autostart"] = c.Expose.Autostart
	setOrDelete(expose, "domain", c.Expose.Domain)
	setOrDelete(expose, "tunnel", c.Expose.Tunnel)
	setOrDelete(expose, "adapter", c.Expose.Adapter)
	if c.Expose.Enabled != nil {
		expose["enabled"] = *c.Expose.Enabled
	}
	if len(c.Expose.Adapters) > 0 {
		expose["adapters"] = mergeAdapters(expose["adapters"], c.Expose.Adapters)
	} else {
		delete(expose, "adapters")
	}
	doc["expose"] = expose

	return doc
}

// mergeAdapters keeps unknown per-adapter keys by matching on id.
func mergeAdapters(rawList any, adapters []Adapter) []any {
	prev := map[string]map[string]any{}
	if items, ok := rawList.([]any); ok {
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				if id, _ := m["id"].(string); id != "" {
					prev[id] = m
				}
			}
		}
	}
	out := make([]any, 0, len(adapters))
	for _, a := range adapters {
		m := cloneMap(prev[a.ID])
		m["id"] = a.ID
		m["script"] = a.Script
		env := rawTable(m, "env")
		for k, v := range a.Env {
			env[k] = v
		}
		// Drop env keys the struct no longer carries.
		for k := range env {
			if _, ok := a.Env[k]; !ok {
				delete(env, k)
			}
		}
		if len(env) > 0 {
			m["env"] = env
		} else {
			delete(m, "env")
		}
		out = append(out, m)
	}
	return out
}

func setOrDelete(m map[string]any, key, val string) {
	if strings.TrimSpace(val) == "" {
		delete(m, key)
		return
	}
	m[key] = val
}

func rawTable(m map[string]any, key string) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	if t, ok := m[key].(map[string]any); ok {
		return t
	}
	return map[string]any{}
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+4)
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			out[k] = cloneMap(sub)
			continue
		}
		out[k] = v
	}
	return out
}

// create writes the first-run file: commented defaults with the token already
// minted, so the file is correct after a single 0600 write.
func create(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	return writeFile0600(path, []byte(defaultFileTemplate))
}

// writeFile0600 writes atomically (temp file + rename) with 0600 permissions.
func writeFile0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Chmod(path, 0o600)
}

const defaultFileTemplate = `# herdr-expose configuration.
# Unknown keys are preserved when this file is rewritten, and missing keys fall
# back to the documented defaults, so it is safe to hand-edit and safe to keep
# across upgrades.

[server]
port = 21118
# There is no 'bind' key: the exposure mode below decides the bind address
# (loopback for a tunnel or for local mode, 0.0.0.0 for lan mode).
# allowed_origins = ["https://herdr.example.com"]   # checked before any CORS header

# Auth policy only. There are NO secrets in this file: the server token and the
# per-device tokens are stored as SHA-256 hashes in the 0600 state file.
[auth]
pairing_ttl_seconds = 600           # pairing code lifetime (10 minutes)
max_devices = 32                    # LRU-evicted
device_ttl_days = 30                # sliding expiry per device
pair_attempts_per_minute = 10       # per remote address
handshake_attempts_per_minute = 60  # per remote address

[ui]
theme = "auto"              # auto | light | dark
default_view = "grid"       # grid | focus

# Exposure. The happy path is two lines and zero JavaScript:
#
#   cloudflare = true
#   domain = "herdr.example.com"
#
# herdr-expose then creates or reuses a named Cloudflare tunnel, mints its
# credentials through the API (so 'cloudflared login' and its browser prompt are
# never needed), upserts the proxied CNAME, runs cloudflared, waits for
# https://<domain>/healthz to answer, and restarts it if it dies. The API token
# is read from the environment variable CLOUDFLARE_ALLPURPOSE_TOKEN at the
# moment it is used; it is never stored here, never written to state, never
# logged and never returned by 'status'.
[expose]
cloudflare = false
# domain is REQUIRED when cloudflare or ngrok is on. The tunnel is static, on a
# hostname you own; there is no ephemeral *.trycloudflare.com path at all.
# domain = "herdr.example.com"
# tunnel_name = "herdr-expose"
ngrok = false

# lan = true binds 0.0.0.0 so phones and laptops on the same wifi can reach this
# machine directly at http://<this-machine-ip>:<port>. It is also the AUTOMATIC
# fallback when cloudflare is on but cloudflared is not installed.
#
# It is safe because auth is never optional: a device token is required in every
# mode, and the pairing code is shown only on this machine's screen, so a
# neighbour on the network can reach the port and get nowhere. Note that plain
# HTTP on a LAN IP is not a secure context, so the PWA cannot be INSTALLED over
# LAN (the web app itself works fine).
lan = false

autostart = false

# Escape hatch for exotic setups (tailscale, a corporate proxy, a homelab box):
# point 'adapter' at one of the JS adapters below. A named adapter wins over the
# built-ins above.
# adapter = "my-tunnel"
#
# [[expose.adapters]]
# id = "my-tunnel"
# script = "adapters/template.js"
# [expose.adapters.env]
# hostname = "herdr.example.com"
`
