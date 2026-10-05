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
	"strconv"
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
	ModeLAN   Mode = "lan"   // binds 0.0.0.0; anyone on the wifi can reach the port
	ModeLocal Mode = "local" // binds loopback; the default
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
// The ladder is now two rungs high, because this binary no longer raises
// tunnels: the top of it is the local network. The ORDER is kept rather than
// collapsed into a bool for the same reason it was introduced — the comparison
// at the resolver ("resolved <= requested") is the invariant, and it has to
// keep holding if a rung is ever added back. Reaching the internet is now
// somebody else's job: point a tunnel at the port.
type Rung int

const (
	RungLocal Rung = iota // 127.0.0.1 — this machine only
	RungLAN               // http://<lan-ip>:<port> — anyone on this network
)

func (r Rung) String() string {
	switch r {
	case RungLocal:
		return "local"
	case RungLAN:
		return "lan"
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
	}
	return "unknown"
}

// RungOf maps a resolved Mode onto the ladder. A mode this build has never
// heard of — "cloudflare" or "quick" in a share record written by an older
// version — sorts at the TOP, because an unknown mode must never look SAFER
// than it is. That is the only reason the default arm exists: nothing this
// binary resolves can land there any more.
func RungOf(m Mode) Rung {
	switch m {
	case ModeLocal:
		return RungLocal
	default:
		return RungLAN
	}
}

// ShareRung reads the [share] default_mode key as a rung.
//
// An unrecognised or empty value — including the withdrawn "auto", and the
// withdrawn "quick" and "domain" — means LAN, the shipped default and now the
// top of the ladder. The failure mode of a misread config key is therefore a
// share that reaches this machine and this network and stops.
//
// A file that still says "quick" or "domain" is not refused, because refusing
// would stop a daemon that was running yesterday. It is reported instead:
// DeadShareMode is what the startup warning asks, so the key is inert and
// SAID to be inert rather than quietly reinterpreted.
func ShareRung(defaultMode string) Rung {
	switch strings.ToLower(strings.TrimSpace(defaultMode)) {
	case "local":
		return RungLocal
	default:
		return RungLAN
	}
}

// DeadShareMode reports a [share] default_mode naming a rung this binary no
// longer has, so startup can say so.
func DeadShareMode(defaultMode string) bool {
	switch strings.ToLower(strings.TrimSpace(defaultMode)) {
	case "quick", "domain":
		return true
	}
	return false
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
	// DefaultHours is the TTL used when neither --hours nor --days is given.
	DefaultHours float64 `toml:"default_hours" json:"default_hours"`
	// DefaultMode is local | lan — the rung a `share` with no rung flag climbs
	// to. It ships as "lan" (AMENDMENTS 16 L1) and exists only so somebody
	// with a different personal default can set one.
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

// Chat is the [chat] table: reaching agents from a chat app.
//
// It ships DISABLED, and so does every adapter in it. A bot token sitting in
// an environment variable is not consent to start answering messages with it,
// and a config file that is one `true` away from putting an agent on a public
// chat network should make you type that `true`.
//
//	[chat]
//	enabled = true
//
//	[[chat.adapters]]
//	id = "telegram"
//	enabled = true
//	command = "node telegram.js"
//	  [chat.adapters.env]
//	  HERDR_EXPOSE_TELEGRAM_TOKEN = "$HERDR_EXPOSE_TELEGRAM_TOKEN"
//	  HERDR_CHAT_ID               = "123456789"
//
// Both switches must be on: the table's and the adapter's. One is the feature,
// the other is the channel, and turning the feature on must not silently light
// up every channel somebody once configured.
type Chat struct {
	// Enabled is the master switch. False, shipped, and false when absent.
	Enabled bool `toml:"enabled" json:"enabled"`
	// Adapters are the channels. Each carries its own Enabled.
	Adapters []ChatAdapter `toml:"adapters" json:"adapters"`
}

// ChatAdapter is one channel: a JS file implementing poll() and send().
//
// The script is JavaScript rather than Go on purpose. A channel is two
// functions and a REST call, every chat app spells those differently, and
// nobody should need this repository checked out — let alone a Go toolchain —
// to reach their own agents from Discord. The ones we ship are the same kind
// of file as the one you write.
type ChatAdapter struct {
	ID string `toml:"id" json:"id"`
	// Command is the program to run, as a shell-style argv string:
	// "node telegram.js", "python3 discord.py", "./my-adapter".
	//
	// It is a COMMAND rather than a script path because that costs nothing and
	// buys every language. The alternative -- a .js path run by an embedded
	// interpreter -- means an adapter can only do what the sandbox was taught
	// to do, and the first thing it needed was an HTTP client that the sandbox
	// deliberately did not have. A command has the whole machine: node's fetch,
	// python's requests, a static binary, whatever the author already knows.
	//
	// It runs with cwd set to the chat adapters directory, so the argv stays
	// short and a relative filename means what it looks like.
	//
	// It is NOT run through a shell: no pipes, no redirection, no globbing, no
	// ${} of its own. A chat adapter has no reason to need a shell, and
	// handing one a shell turns every config file into a place to hide a
	// command substitution.
	Command string `toml:"command" json:"command"`
	// Enabled is this channel's own switch, independent of [chat].enabled.
	Enabled bool `toml:"enabled" json:"enabled"`
	// Env is handed to the adapter as its ENVIRONMENT, with every $NAME and
	// ${NAME} expanded from our own environment first (see ExpandEnv).
	//
	// The environment, never argv: argv is world-readable through `ps`, so a
	// token on a command line is a token every process on the machine can
	// read.
	//
	// So a credential is written here as a REFERENCE -- `token =
	// "$HERDR_EXPOSE_TELEGRAM_TOKEN"` -- which is a name, not a secret: it can
	// be committed, read aloud, or pasted into a bug report. The value is
	// fetched at use and registered with the redactor, so it cannot reach a
	// log line. Writing the literal token here instead would work, and would
	// also put a plaintext credential in a file that gets backed up, synced
	// and screenshotted; the expansion exists so nobody has a reason to.
	Env map[string]string `toml:"env" json:"env"`
}

// ActiveChatAdapters returns the adapters that both switches allow to run.
//
// It is the ONLY way the rest of the binary should ask "what is running", so
// that "enabled" cannot come to mean two different things in two places.
func (c Chat) ActiveChatAdapters() []ChatAdapter {
	if !c.Enabled {
		return nil
	}
	var out []ChatAdapter
	for _, a := range c.Adapters {
		// An adapter with no id is unaddressable by the CLI, and one with no
		// command is nothing to run. Both are configuration mistakes that
		// must not become a silently inert "enabled" adapter.
		if a.Enabled && strings.TrimSpace(a.ID) != "" && strings.TrimSpace(a.Command) != "" {
			out = append(out, a)
		}
	}
	return out
}

// Expose is the [expose] table: how far the listener reaches, and nothing
// else. There are two answers — this machine, or this machine and the local
// network — so there is one key:
//
//	[expose]
//	lan = true
//
// This table used to select a transport (`cloudflare`, `domain`,
// `tunnel_name`, `adapter`). It no longer does: herdr-expose serves loopback
// and the LAN, and reaching it from outside the network is a tunnel you point
// at the port yourself. Those keys are still PARSED — see DeadKeys — because
// go-toml ignores what it does not recognise, and a config that says
// `cloudflare = true` must be told it is inert rather than left believing it.
type Expose struct {
	// LAN allows binding 0.0.0.0 so phones and laptops on the same wifi can
	// reach the server directly.
	//
	// Safe only because auth is mandatory in every mode and the pairing code is
	// shown ONLY on this machine: no HTTP endpoint mints or displays one, so a
	// neighbour on the network can reach the port and get nowhere.
	LAN bool `toml:"lan" json:"lan"`

	Autostart bool `toml:"autostart" json:"autostart"`

	// Enabled is the legacy master switch from the pre-amendment spec. It is
	// kept so old configs keep working: when it is present and false it
	// suppresses autostart. A nil value means "not specified".
	Enabled *bool `toml:"enabled" json:"enabled,omitempty"`

	// The keys below do NOTHING. They are decoded for one purpose: so startup
	// can name them and say they are inert. Deleting the fields would be worse
	// than keeping them, because an unknown TOML key is silently dropped — a
	// file still carrying `cloudflare = true` and a domain would load without
	// complaint and serve loopback, and the person would find out when the
	// link they sent somebody did not open. They carry no `json` tag: they are
	// not part of the config API any more. Save drops them on rewrite, the
	// same treatment the withdrawn `ngrok` key already gets.
	DeadCloudflare bool   `toml:"cloudflare" json:"-"`
	DeadDomain     string `toml:"domain" json:"-"`
	DeadTunnelName string `toml:"tunnel_name" json:"-"`
	DeadTunnel     string `toml:"tunnel" json:"-"`
	DeadAdapter    string `toml:"adapter" json:"-"`
	DeadAdapters   []any  `toml:"adapters" json:"-"`
}

// DeadKeys lists the [expose] keys present in this config that no longer do
// anything, spelled the way the file spells them. Empty is the good case.
func (e Expose) DeadKeys() []string {
	var out []string
	if e.DeadCloudflare {
		out = append(out, "cloudflare = true")
	}
	if strings.TrimSpace(e.DeadDomain) != "" {
		out = append(out, "domain = "+strconv.Quote(strings.TrimSpace(e.DeadDomain)))
	}
	if strings.TrimSpace(e.DeadTunnelName) != "" || strings.TrimSpace(e.DeadTunnel) != "" {
		out = append(out, "tunnel_name")
	}
	if strings.TrimSpace(e.DeadAdapter) != "" || len(e.DeadAdapters) > 0 {
		out = append(out, "adapter")
	}
	return out
}

// ShouldAutostart reports whether the daemon starts serving on boot.
func (e Expose) ShouldAutostart() bool {
	if e.Enabled != nil && !*e.Enabled {
		return false
	}
	return e.Autostart
}

// Config is the whole file. `raw` holds the decoded document as-is, including
// every key this binary has never heard of, so Save can put them back.
type Config struct {
	Server Server `toml:"server" json:"server"`
	Auth   Auth   `toml:"auth" json:"-"`
	UI     UI     `toml:"ui" json:"ui"`
	Expose Expose `toml:"expose" json:"expose"`
	Share  Share  `toml:"share" json:"share"`
	Chat   Chat   `toml:"chat" json:"chat"`
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

// Mode implements the Provider interface: the configured reach.
func (c *Config) Mode() string {
	if c.Expose.LAN {
		return string(ModeLAN)
	}
	return string(ModeLocal)
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
		Chat: Chat{},
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
	return cfg, nil
}

// Validate enforces the invariants the rest of the binary relies on. The
// loopback check is the important one: the HTTP surface is a remote code
// execution surface, and the only addresses it is ever allowed to answer on
// are loopback and the local network.
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
	if err := c.validateShare(); err != nil {
		return err
	}
	if err := c.validateLog(); err != nil {
		return err
	}
	// The dead [expose] keys are deliberately NOT validated and never an
	// error: a config written for the tunnel build has to keep loading, or an
	// upgrade turns into a daemon that will not start. Startup reports them
	// instead — see Expose.DeadKeys.
	return nil
}

func (c *Config) validateShare() error {
	switch c.Share.DefaultMode {
	// "auto" is the withdrawn AMENDMENTS 15 ladder. It stays LOADABLE so that
	// a config file written before AMENDMENTS 16 does not break, but it now
	// resolves to `lan` (see ShareRung) — a reading of the old key that cannot
	// publish a session the user never asked to publish.
	// "quick" and "domain" join "auto" as withdrawn spellings: still
	// LOADABLE, so a file written for the tunnel build does not stop the
	// daemon, but they now resolve to `lan`, and startup says so.
	case "local", "auto", "lan", "quick", "domain":
	default:
		return fmt.Errorf("share.default_mode %q is not one of local|lan "+
			"(the default is \"lan\": a share reaches this machine and this network, and there "+
			"is no rung above that — point a tunnel at the port if you need one)",
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
	// Keys this binary no longer has: read without complaint (an old config
	// must keep loading) and dropped on rewrite, exactly as `server.bind` and
	// `share.default_mode = "auto"` are. `ngrok` was a built-in provider until
	// AMENDMENTS 18 / ADR 0034 removed it; the rest went with the tunnel
	// itself. A file that still carries them is not an error — they are just
	// keys that no longer do anything, and a rewrite is the one moment they
	// can leave the file without surprising anybody, because the startup
	// warning has already named them.
	for _, dead := range []string{"ngrok", "cloudflare", "domain", "tunnel", "tunnel_name", "adapter", "adapters"} {
		delete(expose, dead)
	}
	expose["lan"] = c.Expose.LAN
	expose["autostart"] = c.Expose.Autostart
	if c.Expose.Enabled != nil {
		expose["enabled"] = *c.Expose.Enabled
	}
	doc["expose"] = expose

	share := rawTable(doc, "share")
	// Same as [expose]: a withdrawn key is read without complaint and dropped
	// on rewrite, never a reason to refuse to start. `domain_suffix` named a
	// hostname for the domain rung, which no longer exists.
	for _, dead := range []string{"ngrok", "domain_suffix"} {
		delete(share, dead)
	}
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
