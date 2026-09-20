package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstRunCreatesFileWithDefaultsAnd0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Server.Port != DefaultPort || cfg.Server.Bind != DefaultBind {
		t.Fatalf("unexpected defaults: %+v", cfg.Server)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600", st.Mode().Perm())
	}
	if cfg.Auth.PairingTTLSeconds != 600 {
		t.Fatalf("pairing ttl = %d, want 600", cfg.Auth.PairingTTLSeconds)
	}
	// Comments survive the first-run write.
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "the exposure mode below decides the bind address") {
		t.Fatalf("first-run file lost its comments:\n%s", body)
	}
}

// Forward compatibility, the whole point:
//  1. unknown keys survive a rewrite,
//  2. missing keys take defaults,
//  3. an old config loads on a newer binary without error.
func TestForwardCompatibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// An "old" config: sparse, plus keys from a hypothetical newer release.
	src := `[server]
port = 8123
bind = "127.0.0.1"
quic = true                 # key from a future version

[future_table]
alpha = "keep me"
nested = { deep = 1 }

[expose]
enabled = true
cloudflare = true
domain = "herdr.example.com"
experimental_retry = 7
adapter = "cloudflare-named"

[[expose.adapters]]
id = "cloudflare-named"
script = "adapters/cloudflare-named.js"
weight = 42
[expose.adapters.env]
hostname = "herdr.example.com"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	// (3) loads without error ...
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("old config failed to load: %v", err)
	}
	// (2) ... and missing keys take defaults.
	if cfg.Auth.PairingTTLSeconds != DefaultPairingTTLSeconds {
		t.Fatalf("pairing_ttl_seconds = %d, want default %d", cfg.Auth.PairingTTLSeconds, DefaultPairingTTLSeconds)
	}
	if cfg.UI.Theme != DefaultTheme || cfg.UI.DefaultView != DefaultView {
		t.Fatalf("ui defaults not applied: %+v", cfg.UI)
	}
	if cfg.Server.Port != 8123 {
		t.Fatalf("port = %d, want 8123", cfg.Server.Port)
	}
	a, ok := cfg.AdapterByID("cloudflare-named")
	if !ok || a.Env["hostname"] != "herdr.example.com" {
		t.Fatalf("adapter env not parsed: %+v", cfg.Expose.Adapters)
	}
	if cfg.Expose.Provider() != ProviderJS {
		t.Fatalf("named adapter must win, got %s", cfg.Expose.Provider())
	}
	if cfg.Expose.Enabled == nil || !*cfg.Expose.Enabled {
		t.Fatal("legacy enabled flag lost")
	}

	// (1) rewrite and confirm nothing unknown was clobbered.
	cfg.UI.Theme = "dark"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, _ := os.ReadFile(path)
	text := string(out)
	for _, want := range []string{"quic = true", "future_table", "keep me", "deep = 1", "experimental_retry = 7", "weight = 42"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Save clobbered unknown key %q:\n%s", want, text)
		}
	}

	round, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload after save: %v", err)
	}
	if round.UI.Theme != "dark" || round.Server.Port != 8123 || round.Expose.Adapter != "cloudflare-named" {
		t.Fatalf("round trip lost known keys: %+v %+v", round.Server, round.UI)
	}
}

func TestBindMustBeLoopback(t *testing.T) {
	good := []string{"127.0.0.1", "127.0.0.2", "::1", "localhost"}
	for _, b := range good {
		if err := ValidateBind(b); err != nil {
			t.Fatalf("ValidateBind(%q) = %v, want nil", b, err)
		}
	}
	// 0.0.0.0 is legal as the RESOLVED value of lan mode (E1), so ValidateBind
	// accepts it; what is refused is a user naming an address in the file.
	if err := ValidateBind(BindAll); err != nil {
		t.Fatalf("ValidateBind(0.0.0.0) = %v, want nil (lan mode resolves to it)", err)
	}
	bad := []string{"192.168.1.10", "::", "example.com", "", "78.46.65.254"}
	for _, b := range bad {
		if err := ValidateBind(b); err == nil {
			t.Fatalf("ValidateBind(%q) = nil, want an error", b)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[server]\nbind = \"0.0.0.0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("loading a config that sets bind = 0.0.0.0 must fail")
	}
	if !strings.Contains(err.Error(), "lan = true") {
		t.Fatalf("error should point at lan mode, got: %v", err)
	}
}

// B5/B6: config carries no secrets, and a legacy plaintext token is dropped
// rather than carried forward on rewrite.
func TestNoSecretsInConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	src := "[auth]\ntoken = \"legacy-plaintext-secret\"\npairing_ttl_seconds = 60\nkeep_me = 1\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("legacy config with a token must still load: %v", err)
	}
	if cfg.Auth.PairingTTLSeconds != 60 {
		t.Fatalf("explicit ttl lost: %d", cfg.Auth.PairingTTLSeconds)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "legacy-plaintext-secret") {
		t.Fatalf("legacy secret carried forward:\n%s", body)
	}
	if !strings.Contains(string(body), "keep_me") {
		t.Fatalf("unknown key dropped while removing the secret:\n%s", body)
	}

	b, err := json.Marshal(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "token") {
		t.Fatalf("config JSON mentions a token: %s", b)
	}
}

// B6: the config path ignores $HERDR_PLUGIN_CONFIG_DIR on purpose.
func TestConfigPathIgnoresPluginConfigDir(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "/tmp/herdr-plugin-config-dir")
	p, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, "herdr-plugin-config-dir") {
		t.Fatalf("config path honoured HERDR_PLUGIN_CONFIG_DIR: %s", p)
	}
	if !strings.HasSuffix(p, filepath.Join(".config", "herdr-expose", "config.toml")) {
		t.Fatalf("unexpected config path: %s", p)
	}
}

func TestStoreReloadKeepsOldConfigOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	st, err := OpenFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Port() != DefaultPort || st.Bind() != DefaultBind {
		t.Fatal("provider interface returned junk")
	}

	changed := make(chan int, 1)
	st.OnChange(func(c *Config) { changed <- c.Server.Port })

	body, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(body), "port = 21118", "port = 7777", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if st.Port() != 7777 {
		t.Fatalf("port after reload = %d, want 7777", st.Port())
	}
	if p := <-changed; p != 7777 {
		t.Fatalf("listener saw port %d", p)
	}

	if err := os.WriteFile(path, []byte("this is not ==== toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.Reload(); err == nil {
		t.Fatal("reload of a broken file should error")
	}
	if st.Port() != 7777 {
		t.Fatalf("broken reload clobbered live config: port = %d", st.Port())
	}
}

var _ Provider = (*Config)(nil)

func TestExposeProviderSelection(t *testing.T) {
	no := false
	cases := []struct {
		name string
		e    Expose
		want ProviderKind
		auto bool
	}{
		{"empty", Expose{}, ProviderNone, false},
		{"two-liner", Expose{Cloudflare: true, Domain: "herdr.deemwar.com", Autostart: true}, ProviderCloudflare, true},
		{"quick", Expose{Cloudflare: true}, ProviderCloudflare, false},
		{"js wins", Expose{Cloudflare: true, Adapter: "x"}, ProviderJS, false},
		{"legacy disabled", Expose{Cloudflare: true, Autostart: true, Enabled: &no}, ProviderCloudflare, false},
	}
	for _, c := range cases {
		if got := c.e.Provider(); got != c.want {
			t.Errorf("%s: provider = %s, want %s", c.name, got, c.want)
		}
		if got := c.e.ShouldAutostart(); got != c.auto {
			t.Errorf("%s: autostart = %v, want %v", c.name, got, c.auto)
		}
	}
}

func TestTwoLineExposeConfigRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	src := "[expose]\ncloudflare = true\ndomain = \"herdr.deemwar.com\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("two-line config failed to load: %v", err)
	}
	if !cfg.Expose.Cloudflare || cfg.Expose.Domain != "herdr.deemwar.com" {
		t.Fatalf("expose not parsed: %+v", cfg.Expose)
	}
	if cfg.Server.Port != DefaultPort || cfg.Auth.PairingTTLSeconds != DefaultPairingTTLSeconds {
		t.Fatalf("defaults missing: %+v %+v", cfg.Server, cfg.Auth)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Expose.Domain != "herdr.deemwar.com" || !again.Expose.Cloudflare {
		t.Fatalf("round trip lost expose keys: %+v", again.Expose)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "adapters") {
		t.Fatalf("empty adapter list should not be written:\n%s", body)
	}
}

// A config written against an older build still carries `ngrok = true` (and
// the reserved domain it needed). The built-in provider is gone (AMENDMENTS 18
// / ADR 0034), and the key must behave exactly as the other withdrawn keys do:
// LOADED without complaint, IGNORED, and DROPPED on the next rewrite. Refusing
// to start over a key that no longer does anything would be the worst of both
// worlds — the user cannot act on it and their daemon is down.
func TestLegacyNgrokKeysLoadAndAreDroppedOnRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	src := "[expose]\nngrok = true\ndomain = \"herdr.example.ngrok.app\"\nautostart = true\n" +
		"[share]\nngrok = true\ndefault_mode = \"lan\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("a config carrying the withdrawn ngrok keys must still load: %v", err)
	}
	// Ignored: nothing is selected, so nothing autostarts.
	if got := cfg.Expose.Provider(); got != ProviderNone {
		t.Fatalf("the withdrawn ngrok key still selects a provider: %s", got)
	}
	if cfg.Expose.ShouldAutostart() {
		t.Fatal("a config whose only provider was ngrok must not autostart anything")
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(strings.ToLower(string(body)), "ngrok = ") {
		t.Fatalf("the withdrawn ngrok key survived a rewrite:\n%s", body)
	}
	if _, err := LoadFrom(path); err != nil {
		t.Fatalf("the rewritten config must still load: %v", err)
	}
}

// A legacy config that still carries the old loopback `bind` key must keep
// loading: the key is ignored, not fatal.
func TestLegacyLoopbackBindIsIgnoredNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[server]\nport = 21118\nbind = \"127.0.0.1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("legacy bind key should load: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "bind") {
		t.Fatalf("bind should not be written back:\n%s", body)
	}
}

// E1: lan mode and the mode reported for each configuration.
func TestLANModeConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[expose]\nlan = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Expose.LAN {
		t.Fatal("lan not parsed")
	}
	if cfg.Mode() != string(ModeLAN) {
		t.Fatalf("mode = %s, want lan", cfg.Mode())
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := LoadFrom(path)
	if err != nil || !again.Expose.LAN {
		t.Fatalf("lan lost on round trip: %v %+v", err, again.Expose)
	}

	st := NewStore(again)
	if st.Mode() != string(ModeLocal) || st.Bind() != BindLoopback {
		t.Fatalf("store should start conservative: mode=%s bind=%s", st.Mode(), st.Bind())
	}
	if err := st.SetBinding(ModeLAN, BindAll); err != nil {
		t.Fatal(err)
	}
	if st.Mode() != string(ModeLAN) || st.Bind() != BindAll {
		t.Fatalf("resolved binding not applied: mode=%s bind=%s", st.Mode(), st.Bind())
	}
	if err := st.SetBinding(ModeLAN, "192.168.1.5"); err == nil {
		t.Fatal("SetBinding must refuse an address that is neither loopback nor 0.0.0.0")
	}
	// A reload must not clobber the resolved bind with the file's idea of it.
	if err := st.Reload(); err != nil {
		t.Fatal(err)
	}
	if st.Bind() != BindAll {
		t.Fatalf("reload clobbered the resolved bind: %s", st.Bind())
	}
}
