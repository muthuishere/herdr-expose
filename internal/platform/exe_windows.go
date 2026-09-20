//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// ExeName is the on-disk filename of a command. Windows will not run a file
// without an extension, so a bare name gets .exe.
func ExeName(base string) string {
	if filepath.Ext(base) != "" {
		return base
	}
	return base + ".exe"
}

// IsExecutable reports whether path is a file this OS would run.
//
// The Unix spelling of this — Mode().Perm()&0o111 — is ALWAYS FALSE on Windows:
// os.Stat there synthesises 0666/0444 from the read-only attribute and there is
// no execute bit to find. A direct port of that check silently disables the
// whole binary-search fallback, which is precisely the path a Windows Service
// depends on, because a service inherits the machine PATH and not the user's.
// Windows decides executability by EXTENSION, so that is what this asks.
func IsExecutable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	for _, e := range strings.Split(strings.ToLower(os.Getenv("PATHEXT")), ";") {
		if e != "" && ext == strings.TrimSpace(e) {
			return true
		}
	}
	switch ext {
	case ".exe", ".com", ".bat", ".cmd":
		return true
	}
	return false
}

// BinCandidates are the directories to search when PATH is not to be trusted.
// A Windows Service runs with the MACHINE PATH, so anything a user-scoped
// installer put on the user PATH is invisible to it — the same failure the Unix
// list exists for, arrived at by a different route.
func BinCandidates() []string {
	dirs := []string{
		`~\.local\bin`,
		`~\bin`,
		`~\AppData\Local\Programs`,
		`~\AppData\Local\Microsoft\WindowsApps`,
		`~\.cargo\bin`,
	}
	if p := os.Getenv("LOCALAPPDATA"); p != "" {
		dirs = append(dirs, filepath.Join(p, "Programs"), filepath.Join(p, "herdr", "bin"))
	}
	if p := os.Getenv("ProgramFiles"); p != "" {
		dirs = append(dirs, p)
	}
	return dirs
}
