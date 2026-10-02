package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// AdaptersDir is Dir()/adapters: where adapters live on disk.
//
// It sits beside config.toml rather than in the state directory because an
// adapter is something a person WRITES and edits, like the config file, not
// something the daemon generates. State is ours; this directory is theirs.
func AdaptersDir() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "adapters"), nil
}

// ChatAdaptersDir is Dir()/adapters/chat.
//
// Chat adapters get their own subdirectory because they answer a different
// contract from the expose ones -- poll()/send() against a chat API, versus
// start()/stop() against a tunnel -- and a flat directory of both would make
// the wrong one loadable in the wrong place.
func ChatAdaptersDir() (string, error) {
	d, err := AdaptersDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "chat"), nil
}

// SeedResult says what a seed did, per file, so the CLI can show it.
type SeedResult struct {
	// Wrote are files that did not exist and now do.
	Wrote []string
	// Kept are files already present, which were NOT touched.
	Kept []string
}

// SeedChatAdapters writes the bundled adapters into the chat adapters
// directory, and NEVER overwrites a file that is already there.
//
// Never-overwrite is the whole contract. The bundled telegram.js is meant to
// be edited -- that is why it is written out rather than kept inside the
// binary -- and an upgrade that silently replaced an edited adapter would
// destroy the work and, worse, do it quietly: the adapter would keep running,
// just not the way its author left it. So a file that exists is kept, even if
// it is older than the bundled one, and even if it is broken. Deleting it is
// how you ask for a fresh copy.
//
// bundled is filename -> source, as BundledChatAdapters() returns. It is
// passed in rather than imported so this package does not depend on the module
// root, which depends on it.
func SeedChatAdapters(bundled map[string][]byte) (SeedResult, error) {
	var res SeedResult
	dir, err := ChatAdaptersDir()
	if err != nil {
		return res, err
	}
	// 0700: an adapter can carry a URL or a chat id, and this directory is
	// only ever read by this user's daemon.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, fmt.Errorf("create %s: %w", dir, err)
	}

	names := make([]string, 0, len(bundled))
	for n := range bundled {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			res.Kept = append(res.Kept, p)
			continue
		} else if !os.IsNotExist(err) {
			return res, fmt.Errorf("stat %s: %w", p, err)
		}
		// 0600, and written whole: a half-written adapter is a syntax error
		// at load time, which is a confusing way to learn about a full disk.
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, bundled[name], 0o600); err != nil {
			return res, fmt.Errorf("write %s: %w", p, err)
		}
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
			return res, fmt.Errorf("install %s: %w", p, err)
		}
		res.Wrote = append(res.Wrote, p)
	}
	return res, nil
}

// ResolveChatScript turns a [[chat.adapters]] script value into a path.
//
// A path -- anything with a separator, or any existing file -- always wins, so
// a local copy shadows a bundled adapter of the same name rather than fighting
// it. A bare name resolves inside the chat adapters directory. The returned
// path may not exist; the caller reports that, because "telegram.js is not
// there" is a better error than "could not resolve".
func ResolveChatScript(script string) (string, error) {
	if script == "" {
		return "", fmt.Errorf("adapter has no script")
	}
	if filepath.IsAbs(script) || filepath.Dir(script) != "." {
		return expandHome(script), nil
	}
	dir, err := ChatAdaptersDir()
	if err != nil {
		return "", err
	}
	name := script
	if filepath.Ext(name) == "" {
		name += ".js"
	}
	return filepath.Join(dir, name), nil
}

// expandHome turns a leading ~ into the home directory, because a config file
// is written by a person and a person writes ~.
func expandHome(p string) string {
	if p == "~" || (len(p) > 1 && p[0] == '~' && (p[1] == '/' || p[1] == filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}
