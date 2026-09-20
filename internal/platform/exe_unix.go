//go:build !windows

package platform

import "os"

// ExeName is the on-disk filename of a command. Unix has no extension.
func ExeName(base string) string { return base }

// IsExecutable reports whether path is a file this OS would run.
func IsExecutable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// BinCandidates are the directories to search when PATH is not to be trusted —
// a launchd/systemd unit starts with a minimal PATH that contains none of the
// places herdr or cloudflared are normally installed.
func BinCandidates() []string {
	return []string{
		"~/.local/bin", "~/.cargo/bin", "~/bin",
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin",
	}
}
