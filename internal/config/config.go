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
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Defaults, mirrored by the scaffold in scaffold.go.
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

	// [share] — AMENDMENTS 9/10: every share is time-boxed, so these are
	// defaults for a TTL, never a way to switch time-boxing off.
	DefaultShareHours = 1.0
	// DefaultShareMode is `lan` (AMENDMENTS 16 L1): the least exposure that
	// still does the job a SHARE exists to do.
	//
	// Note the deliberate asymmetry with the daemon, whose [expose] default is
	// LOCAL and stays that way. Same principle, different job: the daemon
	// serves the person sitting at this machine, who just opens
	// localhost:21118, so loopback is right there. A share's entire reason to
	// exist is reach — nobody hands someone a link they cannot open — so a
	// loopback-only share is the one rung that makes the feature pointless.
	//
	// What this is NOT is the old "auto", which resolved domain -> quick ->
	// lan and could put a session on the public internet because an unrelated
	// `[expose] domain` key happened to be set. LAN covers loopback AND this
	// network in one bind and stops there; every rung above it is still an
	// explicit request.
	DefaultShareMode       = "lan"
	DefaultShareConcurrent = 10

	// [log] — a real FILE by default, rotated in-process. A detached daemon's
	// stderr goes nowhere useful, and an unbounded log on something designed
	// to run for weeks is a disk-filling bug.
	DefaultLogLevel     = "info"
	DefaultLogFormat    = "text"
	DefaultLogMaxSizeMB = 10
	DefaultLogKeep      = 3
)

// DefaultLogFileName is the log inside the state dir that `file = ""` means.
// The daemon writes it and `herdr-expose logs` reads it, both through the same
// resolver, so a log can never land where nothing reads it.
const DefaultLogFileName = "herdr-expose.log"

// Mode is the resolved exposure mode (AMENDMENT E1). It is decided at startup
// by internal/expose — never by a user-set bind address.
type Mode string

const (
	ModeCloudflare Mode = "cloudflare" // public static domain; binds loopback
	ModeQuick      Mode = "quick"      // ephemeral *.trycloudflare.com; SHARES ONLY (AMENDMENTS 15)
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

// Rung is the exposure ladder of AMENDMENTS 16 L1, expressed as an ORDER.
//
// The whole point of the amendment is that exposure never climbs above what
// the user asked for, and that is a property about ORDERING — "the resolved
// mode is <= the requested mode" — not about any particular pair of modes. So
// it is a comparison the code can actually make, and a test can assert, rather
// than an invariant a reviewer has to re-derive from a chain of if-statements.
//
// The blast radius of each rung differs by orders of magnitude: this machine,
// this room, the entire internet. This binary runs arbitrary commands inside
// the owner's agent sessions, so the distance between rung 0 and rung 2 must
// never be a config key somebody forgot they set. That is what makes the
// pairing, scope and TTL guarantees elsewhere in this spec mean anything.
type Rung int

const (
	RungLocal  Rung = iota // 127.0.0.1 — this machine only
	RungLAN                // http://<lan-ip>:<port> — anyone on this network
	RungQuick              // https://<random>.trycloudflare.com — the internet
	RungDomain             // https://<your host> — the internet, on a name you own
)

func (r Rung) String() string {
	switch r {
	case RungLocal:
		return "local"
	case RungLAN:
		return "lan"
	case RungQuick:
		return "quick"
	case RungDomain:
		return "domain"
	}
	return "unknown"
}

// Reach is the one-line, plain-language answer to "who can get at this?".
// It is printed at create time so the chosen rung is never something the
// person has to infer from a URL.
func (r Rung) Reach() string {
	switch r {
	case RungLocal:
		return "this machine only"
	case RungLAN:
		return "anyone on this network"
	case RungQuick:
		return "anyone on the internet"
	case RungDomain:
		return "anyone on the internet"
	}
	return "unknown"
}

// RungOf maps a resolved Mode onto the ladder. The tunnel modes that are not
// on the share ladder (ngrok, a JS adapter) are still the internet, so they
// sort at the top: an unknown mode must never look SAFER than it is.
func RungOf(m Mode) Rung {
	switch m {
	case ModeLocal:
		return RungLocal
	case ModeLAN:
		return RungLAN
	case ModeQuick:
		return RungQuick
	default:
		return RungDomain
	}
}

// ShareRung reads the [share] default_mode key as a rung.
//
// An unrecognised or empty value — including the withdrawn "auto" — means LAN,
// the shipped default. The failure mode of a misread config key is therefore
// a share that reaches this machine and this network and stops: never a tunnel,
// never the public internet. Nothing above RungLAN is reachable from this
// function without the key spelling it out.
func ShareRung(defaultMode string) Rung {
	switch strings.ToLower(strings.TrimSpace(defaultMode)) {
	case "local":
		return RungLocal
	case "quick":
		return RungQuick
	case "domain":
		return RungDomain
	default:
		return RungLAN
	}
}

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

// Share is the [share] table: defaults for `herdr-expose share`.
type Share struct {
	// DomainSuffix turns `share --name review` into review.<suffix>.
	DomainSuffix string `toml:"domain_suffix" json:"domain_suffix"`
	// DefaultHours is the TTL used when neither --hours nor --days is given.
	DefaultHours float64 `toml:"default_hours" json:"default_hours"`
	// DefaultMode is local | lan | quick | domain — the rung a `share` with no
	// transport flag climbs to. It ships as "lan" (AMENDMENTS 16 L1) and
	// exists only so somebody with a different personal default can set one;
	// it is never a way for a rung to be reached by accident, because the
	// person who edits this file is asking for it in exactly the same sense
	// that typing `--quick` asks for it.
	//
	// "auto" is the WITHDRAWN AMENDMENTS 15 ladder (domain -> quick -> lan).
	// It still loads, so an existing config file keeps working, but it now
	// means `lan`: the reading of the old key that cannot put anything on the
	// public internet that the user did not ask to put there. See ShareRung.
	DefaultMode string `toml:"default_mode" json:"default_mode"`
	// MaxConcurrent caps live shares.
	MaxConcurrent int `toml:"max_concurrent" json:"max_concurrent"`
}

// Log is the [log] table. `herdr-expose logs` reads whatever File points at,
// which is the only way to see what a detached daemon did.
type Log struct {
	Level  string `toml:"level" json:"level"`   // debug | info | warn | error
	Format string `toml:"format" json:"format"` // text | json
	// File is an explicit path. Empty means the default log inside the state
	// dir — NOT stderr: a detached daemon's stderr goes nowhere anybody can
	// find, which is the whole problem this exists to solve.
	File string `toml:"file" json:"file"`

	// MaxSizeMB is the size at which the log rotates. Rotation is in-process
	// and dependency-free; a daemon meant to run for weeks must not be able to
	// fill the disk.
	MaxSizeMB int `toml:"max_size_mb" json:"max_size_mb"`
	// Keep is how many rotated files are retained (herdr-expose.log.1 ...).
	Keep int `toml:"keep" json:"keep"`
}

// SlogLevel maps the configured level onto slog. An unknown value has already
// been refused by Validate; it degrades to info rather than panicking.
func (l Log) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(l.Level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
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

	// Quick selects an ephemeral TryCloudflare tunnel (*.trycloudflare.com):
	// no account, no zone, no DNS record, no API token, nothing to clean up.
	//
	// It is DELIBERATELY not a TOML key (`toml:"-"`). AMENDMENTS 15 brings the
	// quick tunnel back for SHARES ONLY; the permanent deployment still
	// requires a static domain (C1), and a key in the config file would be an
	// invitation to make the daily driver ephemeral again. `herdr-expose share
	// --quick` constructs this table in memory instead.
	Quick bool `toml:"-" json:"quick,omitempty"`

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
	Share  Share  `toml:"share" json:"share"`
	Log    Log    `toml:"log" json:"log"`

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
		Share: Share{
			DefaultHours:  DefaultShareHours,
			DefaultMode:   DefaultShareMode,
			MaxConcurrent: DefaultShareConcurrent,
		},
		Log: Log{
			Level:     DefaultLogLevel,
			Format:    DefaultLogFormat,
			MaxSizeMB: DefaultLogMaxSizeMB,
			Keep:      DefaultLogKeep,
		},
		raw: map[string]any{},
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
		// AMENDMENTS 11 §H1: a section this release added must become visible
		// in the user's file, and it must get there by APPENDING — their edits,
		// their key order and their comments above are untouched. Best-effort:
		// a read-only or root-owned config is not a reason to refuse to start.
		if added, aerr := EnsureSections(path); aerr == nil && len(added) > 0 {
			if d2, rerr := os.ReadFile(path); rerr == nil {
				data = d2
			}
		}
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
	if err := c.validateShare(); err != nil {
		return err
	}
	if err := c.validateLog(); err != nil {
		return err
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

func (c *Config) validateShare() error {
	switch c.Share.DefaultMode {
	// "auto" is the withdrawn AMENDMENTS 15 ladder. It stays LOADABLE so that
	// a config file written before AMENDMENTS 16 does not break, but it now
	// resolves to `lan` (see ShareRung) — a reading of the old key that cannot
	// publish a session the user never asked to publish.
	case "local", "auto", "lan", "quick", "domain":
	default:
		return fmt.Errorf("share.default_mode %q is not one of local|lan|quick|domain "+
			"(the default is \"lan\": a share reaches this machine and this network, and goes "+
			"no further unless you ask it to with --quick or --domain)",
			c.Share.DefaultMode)
	}
	if c.Share.DefaultHours <= 0 {
		return fmt.Errorf("share.default_hours must be positive, got %v — every share is time-boxed (AMENDMENTS 10); "+
			"a long share is a long TTL, e.g. --days 30, not an unlimited one", c.Share.DefaultHours)
	}
	if c.Share.DefaultHours > 365*24 {
		return fmt.Errorf("share.default_hours %v is over a year; a share is time-boxed by design", c.Share.DefaultHours)
	}
	if c.Share.MaxConcurrent <= 0 {
		return fmt.Errorf("share.max_concurrent must be positive, got %d", c.Share.MaxConcurrent)
	}
	d := strings.TrimSpace(c.Share.DomainSuffix)
	if d != "" && (strings.Contains(d, "/") || strings.Contains(d, ":") || !strings.Contains(d, ".")) {
		return fmt.Errorf("share.domain_suffix %q must be a bare domain such as \"share.example.com\"", d)
	}
	return nil
}

func (c *Config) validateLog() error {
	switch strings.ToLower(strings.TrimSpace(c.Log.Level)) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("log.level %q is not one of debug|info|warn|error", c.Log.Level)
	}
	switch strings.ToLower(strings.TrimSpace(c.Log.Format)) {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q is not one of text|json", c.Log.Format)
	}
	if c.Log.MaxSizeMB < 0 {
		return fmt.Errorf("log.max_size_mb must not be negative, got %d (0 disables rotation)", c.Log.MaxSizeMB)
	}
	if c.Log.Keep < 0 {
		return fmt.Errorf("log.keep must not be negative, got %d", c.Log.Keep)
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

	share := rawTable(doc, "share")
	share["domain_suffix"] = c.Share.DomainSuffix
	share["default_hours"] = c.Share.DefaultHours
	share["default_mode"] = c.Share.DefaultMode
	share["max_concurrent"] = int64(c.Share.MaxConcurrent)
	doc["share"] = share

	lg := rawTable(doc, "log")
	lg["level"] = c.Log.Level
	lg["format"] = c.Log.Format
	lg["file"] = c.Log.File
	lg["max_size_mb"] = int64(c.Log.MaxSizeMB)
	lg["keep"] = int64(c.Log.Keep)
	doc["log"] = lg

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

// create writes the first-run file: the COMPLETE scaffold (AMENDMENTS 11 §H1),
// every section and every key present with its default and a one-line comment,
// in a single 0600 write.
func create(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	return writeFile0600(path, []byte(DefaultFileContents()))
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
