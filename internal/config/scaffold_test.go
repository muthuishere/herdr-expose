package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H1: first run writes the COMPLETE config — every section and every key, with
// a one-line comment each, including the subsystems that are OFF by default.
// A key that exists but is not written is a key nobody will ever find.
func TestFirstRunWritesEverySectionAndKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if _, err := LoadFrom(path); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)

	for _, want := range []string{"[server]", "[auth]", "[ui]", "[expose]", "[share]", "[log]"} {
		if !strings.Contains(text, want) {
			t.Errorf("scaffold is missing section %s:\n%s", want, text)
		}
	}
	// Every key AMENDMENTS 11 §H2 lists, at its documented default.
	for _, want := range []string{
		"port = 21118",
		"pairing_ttl_seconds = 600", "max_devices = 32", "device_ttl_days = 30",
		"pair_attempts_per_minute = 10", "handshake_attempts_per_minute = 60",
		`theme = "auto"`, `default_view = "grid"`,
		"cloudflare = false", `domain = ""`, `tunnel_name = "herdr-expose"`,
		"ngrok = false", "lan = false", "autostart = false",
		`domain_suffix = ""`, "default_hours = 1", `default_mode = "auto"`, "max_concurrent = 10",
		`level = "info"`, `format = "text"`, `file = ""`, "max_size_mb = 10", "keep = 3",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("scaffold is missing key %q", want)
		}
	}
	// The stale `bind` key is gone, and its stale claim with it.
	if strings.Contains(text, "\nbind = ") {
		t.Errorf("scaffold still writes a bind key:\n%s", text)
	}
	if strings.Contains(text, "anything else is refused at startup") {
		t.Error("scaffold still carries the stale loopback-only claim; LAN mode binds 0.0.0.0")
	}
	// Every non-comment key line carries a one-line comment (H1).
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "[") {
			continue
		}
		if !strings.Contains(l, "#") {
			t.Errorf("scaffold key without an explanatory comment: %q", l)
		}
	}
	// What it writes is exactly what `config print-default` prints.
	if text != DefaultFileContents() {
		t.Error("first-run file differs from DefaultFileContents()")
	}
	// And what it writes loads back with the documented defaults.
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" || cfg.Log.File != "" ||
		cfg.Log.MaxSizeMB != DefaultLogMaxSizeMB || cfg.Log.Keep != DefaultLogKeep {
		t.Errorf("log defaults: %+v", cfg.Log)
	}
	if cfg.Share.DefaultHours != 1 || cfg.Share.DefaultMode != "auto" || cfg.Share.MaxConcurrent != 10 {
		t.Errorf("share defaults: %+v", cfg.Share)
	}
}

// H1's hard part: ADDING A SECTION MUST NEVER REWRITE OR REORDER WHAT THE USER
// HAS ALREADY EDITED. Everything above the append point must survive byte for
// byte — comments, key order, spacing and keys this binary never heard of.
func TestAddingASectionIsAppendOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// A file as a careful user left it: reordered, commented, with a key from a
	// future release and a table this binary does not know.
	before := `# my notes, do not touch
[ui]
default_view = "focus"   # I like focus mode
theme = "dark"

[server]
port = 9999
quic = true              # from a newer build

[expose]
lan = true
domain = "herdr.example.com"

[mystery]
alpha = "keep me"
`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	added, err := EnsureSections(path)
	if err != nil {
		t.Fatalf("EnsureSections: %v", err)
	}
	want := map[string]bool{"auth": true, "share": true, "log": true}
	if len(added) != len(want) {
		t.Fatalf("added = %v, want exactly %v", added, want)
	}
	for _, a := range added {
		if !want[a] {
			t.Fatalf("unexpected section appended: %s", a)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// APPEND ONLY: the original bytes are still a prefix of the file.
	if !strings.HasPrefix(string(after), before) {
		t.Fatalf("EnsureSections rewrote existing content.\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, s := range []string{"[auth]", "[share]", "[log]"} {
		if !strings.Contains(string(after), s) {
			t.Fatalf("section %s not appended:\n%s", s, after)
		}
	}

	// The user's values still win, and the appended sections supply defaults.
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Server.Port != 9999 || cfg.UI.DefaultView != "focus" || cfg.UI.Theme != "dark" || !cfg.Expose.LAN {
		t.Fatalf("user edits lost: %+v %+v %+v", cfg.Server, cfg.UI, cfg.Expose)
	}
	if cfg.Auth.PairingTTLSeconds != DefaultPairingTTLSeconds || cfg.Log.Level != "info" {
		t.Fatalf("appended defaults not applied: %+v %+v", cfg.Auth, cfg.Log)
	}

	// Idempotent: a second pass adds nothing and touches nothing.
	again, err := EnsureSections(path)
	if err != nil || len(again) != 0 {
		t.Fatalf("second EnsureSections = %v, %v; want no-op", again, err)
	}
	after2, _ := os.ReadFile(path)
	if string(after2) != string(after) {
		t.Fatal("a second EnsureSections changed the file")
	}
}

// A legacy config carrying the now-stale `bind` key keeps loading, the key is
// ignored, and it is never written back.
func TestStaleBindKeyLoadsAndIsNeverWrittenBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := "[server]\nport = 21118\nbind = \"127.0.0.1\"  # loopback only; anything else is refused at startup\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the installed config's stale bind key must load harmlessly: %v", err)
	}
	// Loading appended the missing sections; the stale line is still there
	// verbatim because appending never edits what is above it.
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), `bind = "127.0.0.1"`) {
		t.Fatal("append-only migration edited an existing line")
	}
	// But a REWRITE drops it, and never re-adds it.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if strings.Contains(string(body), "bind") {
		t.Fatalf("bind written back on save:\n%s", body)
	}
}

func TestLogAndShareValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		ok   bool
	}{
		{"default", func(*Config) {}, true},
		{"bad level", func(c *Config) { c.Log.Level = "loud" }, false},
		{"bad format", func(c *Config) { c.Log.Format = "xml" }, false},
		{"json format", func(c *Config) { c.Log.Format = "json" }, true},
		{"rotation off", func(c *Config) { c.Log.MaxSizeMB = 0 }, true},
		{"negative size", func(c *Config) { c.Log.MaxSizeMB = -1 }, false},
		{"negative keep", func(c *Config) { c.Log.Keep = -1 }, false},
		{"share hours zero", func(c *Config) { c.Share.DefaultHours = 0 }, false},
		{"share bad mode", func(c *Config) { c.Share.DefaultMode = "forever" }, false},
	}
	for _, tc := range cases {
		cfg := Defaults()
		tc.mut(cfg)
		err := cfg.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}
