//go:build windows

package platform

import (
	"os"
	"path/filepath"
)

// Windows has no ~/.config and no ~/.local/state, and transplanting them gives
// you a literal C:\Users\me\.config that no backup, roaming profile or admin
// tool knows about. So the two kinds of data go where Windows puts them:
//
//	config  -> %APPDATA%          (os.UserConfigDir) — roams with the profile
//	state   -> %LOCALAPPDATA%     (os.UserCacheDir)  — machine-local, correct
//	                                                   for a pidfile and a
//	                                                   socket path that cannot
//	                                                   follow the user
//
// The Unix paths are untouched; this file is the whole difference.

// StateDir is %LOCALAPPDATA%\<app>\state.
func StateDir(app string) (string, error) {
	base, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return "", err
	}
	return filepath.Join(base, app, "state"), nil
}

// ConfigDir is %APPDATA%\<app>.
func ConfigDir(app string) (string, error) {
	base, err := os.UserConfigDir() // %APPDATA%
	if err != nil {
		return "", err
	}
	return filepath.Join(base, app), nil
}

// HerdrConfigDir is where HERDR keeps its own config and session sockets.
// Mirrors herdr's src/config/io.rs: $XDG_CONFIG_HOME wins if set, else
// %APPDATA%\herdr.
func HerdrConfigDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "herdr"), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "herdr"), nil
}
