//go:build !windows

package platform

import (
	"os"
	"path/filepath"
)

// StateDir is where this plugin keeps mutable per-user state: the pidfile, the
// managed-pid ledger, share records, generated tunnel configs, logs.
//
// ~/.local/state/<app> — the XDG spelling, unchanged from before the port.
func StateDir(app string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", app), nil
}

// ConfigDir is where this plugin's config.toml lives: ~/.config/<app>.
func ConfigDir(app string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", app), nil
}

// HerdrConfigDir is where HERDR keeps its own config and session sockets.
// Mirrors herdr's src/config/io.rs: $XDG_CONFIG_HOME/herdr, else ~/.config/herdr.
func HerdrConfigDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "herdr"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "herdr"), nil
}
