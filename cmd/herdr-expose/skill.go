package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// `herdr-expose skill install|uninstall|status` — the agent skill half of the
// one install.
//
// The skill in skill/ is how the owner actually drives this binary: they say
// "share this session" to Claude Code and the skill runs the CLI. It is
// therefore part of the product, not a sample, and it has to arrive with the
// same single command that puts the binary in place.
//
// Three rules shape everything below, and each one is a bug that was easy to
// write:
//
//  1. It is a SYMLINK, never a copy. A copy is a fork the moment you
//     `git pull` — the agent would then be reading a skill that no longer
//     matches the binary it drives, which is the worst possible way to be
//     wrong about a tool that exposes terminals to the internet.
//
//  2. The source is RESOLVED from this executable, never guessed. The same
//     binary runs from a dev checkout and from Herdr's managed plugin checkout
//     under ~/.config/herdr/plugins/github/, and a hard-coded path would link
//     the wrong one — silently, because both exist on this machine.
//
//  3. A real directory in the way is REFUSED, never clobbered. A skill
//     directory somebody wrote by hand is their work; replacing it with a
//     symlink would delete it with no way back.

const skillLinkName = "herdr-share"

// skillTarget is one place a skill directory is expected to live.
type skillTarget struct {
	Dir      string // the skills directory, e.g. ~/.claude/skills
	Required bool   // created if missing; optional dirs are skipped when absent
	Label    string // for output, e.g. "~/.claude/skills/herdr-share"
}

// Link is the full path of the link this tool manages inside Dir.
func (t skillTarget) Link() string { return filepath.Join(t.Dir, skillLinkName) }

// skillTargets lists where the skill is linked on this machine.
//
// ~/.claude/skills is created if it does not exist: Claude Code is the client
// this skill is written for, so "install" there means "make it work". The
// shared ~/.agents/skills tree is only touched when it ALREADY exists — an
// agent runtime that is not set up is not a thing to set up from here.
func skillTargets() ([]skillTarget, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	out := []skillTarget{{
		Dir:      filepath.Join(home, ".claude", "skills"),
		Required: true,
		Label:    filepath.Join("~", ".claude", "skills", skillLinkName),
	}}
	agents := filepath.Join(home, ".agents", "skills")
	if st, err := os.Stat(agents); err == nil && st.IsDir() {
		out = append(out, skillTarget{
			Dir:      agents,
			Required: false,
			Label:    filepath.Join("~", ".agents", "skills", skillLinkName),
		})
	}
	return out, nil
}

// skillSourceDir resolves the skill/ directory belonging to THIS binary.
//
// It walks up from the resolved executable looking for a directory that holds
// both herdr-plugin.toml and skill/SKILL.md — the pair that identifies a
// herdr-expose checkout, whether that is the dev clone or the managed plugin
// checkout Herdr cloned. Symlinks are evaluated first, so running through
// ~/.local/bin/herdr-expose still finds the checkout the binary really lives
// in rather than ~/.local.
//
// The working directory is the fallback, for `go run` and test binaries, whose
// executables live in a temp dir with no checkout above them.
func skillSourceDir() (string, error) {
	var tried []string
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if root, ok := findCheckoutRoot(filepath.Dir(exe)); ok {
			return filepath.Join(root, "skill"), nil
		}
		tried = append(tried, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		if root, ok := findCheckoutRoot(wd); ok {
			return filepath.Join(root, "skill"), nil
		}
		tried = append(tried, wd)
	}
	return "", fmt.Errorf("cannot find this binary's herdr-expose checkout (looked upward from %s). "+
		"Run `herdr-expose skill install` from inside the checkout, or reinstall the plugin",
		strings.Join(tried, " and "))
}

// findCheckoutRoot walks up from dir to the filesystem root looking for a
// herdr-expose checkout.
func findCheckoutRoot(dir string) (string, bool) {
	for {
		if isCheckoutRoot(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func isCheckoutRoot(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "herdr-plugin.toml")); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "skill", "SKILL.md"))
	return err == nil
}

// skillDirName reads the `name:` out of a skill directory's SKILL.md
// frontmatter. It is how uninstall tells "a symlink this tool created" from
// "a symlink to somebody else's skill that happens to share a name", and it is
// read from the file rather than assumed from the path because the path is the
// thing that changes between a dev checkout and a managed one.
func skillDirName(dir string) string {
	f, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inFront := false
	for i := 0; sc.Scan() && i < 40; i++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if i == 0 && line == "---" {
			inFront = true
			continue
		}
		if !inFront {
			return ""
		}
		if line == "---" {
			return ""
		}
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// skillLinkState is what is at a target path right now.
type skillLinkState int

const (
	linkAbsent  skillLinkState = iota // nothing there
	linkCorrect                       // a symlink already pointing at src
	linkStale                         // a symlink pointing somewhere else, or at nothing
	linkReal                          // a real file or directory — never touched
)

// inspectSkillLink classifies the target path against the wanted source.
func inspectSkillLink(link, src string) (state skillLinkState, points string) {
	fi, err := os.Lstat(link)
	if err != nil {
		return linkAbsent, ""
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return linkReal, ""
	}
	points, err = os.Readlink(link)
	if err != nil {
		return linkStale, ""
	}
	abs := points
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(filepath.Dir(link), abs)
	}
	if samePath(abs, src) {
		return linkCorrect, points
	}
	return linkStale, points
}

// samePath compares two paths with symlinks evaluated, so
// /var/... and /private/var/... (macOS) are not reported as different
// installs of the same directory.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, erra := filepath.EvalSymlinks(a)
	rb, errb := filepath.EvalSymlinks(b)
	if erra != nil || errb != nil {
		return false
	}
	return ra == rb
}

func cmdSkill(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: herdr-expose skill install|uninstall|status")
	}
	switch args[0] {
	case "install":
		return cmdSkillInstall()
	case "uninstall", "remove":
		return cmdSkillUninstall()
	case "status":
		return cmdSkillStatus()
	}
	return fmt.Errorf("unknown skill subcommand %q (want install, uninstall or status)", args[0])
}

// cmdSkillInstall converges on "the skill is linked here", and says what it
// did in every branch — including the branch where it did nothing, which on a
// machine that already has the skill is the common one.
func cmdSkillInstall() error {
	src, err := skillSourceDir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
		return fmt.Errorf("%s has no SKILL.md; this checkout is incomplete", src)
	}
	targets, err := skillTargets()
	if err != nil {
		return err
	}

	fmt.Printf("skill source: %s\n", src)
	var refused []string
	for _, t := range targets {
		link := t.Link()
		state, points := inspectSkillLink(link, src)
		switch state {
		case linkCorrect:
			fmt.Printf("  %s  already linked here — nothing to do\n", t.Label)
			continue
		case linkReal:
			fmt.Printf("  %s  REFUSED: a real directory is already there\n", t.Label)
			fmt.Printf("      That is somebody's own skill, not a link this tool made, so it is not\n")
			fmt.Printf("      touched. Move or delete %s yourself, then re-run.\n", link)
			refused = append(refused, t.Label)
			continue
		case linkStale:
			// A stale link is the normal state after the checkout moves — a
			// plugin reinstall, a renamed workspace. Repointing it is the
			// whole reason a wrong link is not an error.
			if points != "" {
				fmt.Printf("  %s  repointing (was %s)\n", t.Label, points)
			} else {
				fmt.Printf("  %s  repointing (was a broken symlink)\n", t.Label)
			}
			if err := os.Remove(link); err != nil {
				return fmt.Errorf("removing the stale link %s: %w", link, err)
			}
		}
		if err := os.MkdirAll(t.Dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", t.Dir, err)
		}
		if err := os.Symlink(src, link); err != nil {
			return fmt.Errorf("linking %s: %w", link, err)
		}
		fmt.Printf("  %s -> %s\n", t.Label, src)
	}
	if len(refused) > 0 {
		return fmt.Errorf("could not link %s: a real directory is in the way", strings.Join(refused, ", "))
	}
	fmt.Println("\nThe skill is `herdr-share`. Start a new agent session to pick it up, then say")
	fmt.Println("\"share this session\". Remove it with `herdr-expose skill uninstall`.")
	return nil
}

func cmdSkillStatus() error {
	src, srcErr := skillSourceDir()
	if srcErr != nil {
		fmt.Println("skill source: UNRESOLVED —", srcErr)
	} else {
		fmt.Printf("skill source: %s\n", src)
	}
	targets, err := skillTargets()
	if err != nil {
		return err
	}
	for _, t := range targets {
		link := t.Link()
		state, points := inspectSkillLink(link, src)
		switch state {
		case linkAbsent:
			fmt.Printf("  %s  not linked\n", t.Label)
		case linkCorrect:
			if _, err := os.Stat(link); err != nil {
				// A correct link whose source vanished: the checkout was
				// deleted out from under it. That reads as "installed" to
				// anything that only looks at the link.
				fmt.Printf("  %s  linked to %s but the TARGET IS MISSING\n", t.Label, points)
				continue
			}
			fmt.Printf("  %s -> %s  (ok)\n", t.Label, points)
		case linkStale:
			resolves := "target missing"
			if _, err := os.Stat(link); err == nil {
				resolves = "resolves, but to another checkout"
			}
			fmt.Printf("  %s -> %s  STALE (%s) — `skill install` repoints it\n", t.Label, points, resolves)
		case linkReal:
			fmt.Printf("  %s  a real directory, not a link from this tool\n", t.Label)
		}
	}
	return nil
}

// cmdSkillUninstall removes only what install created.
//
// The check is deliberately stricter than "is it a symlink": a symlink named
// herdr-share could point at somebody's own copy of the skill. It is removed
// only when it resolves to a directory whose SKILL.md declares
// `name: herdr-share`, i.e. it really is this skill. Anything else is reported
// and left alone, and a second run is success, because the desired state — no
// link — is already reached.
func cmdSkillUninstall() error {
	targets, err := skillTargets()
	if err != nil {
		return err
	}
	removed := 0
	for _, t := range targets {
		link := t.Link()
		fi, err := os.Lstat(link)
		if err != nil {
			fmt.Printf("  %s  not linked — nothing to do\n", t.Label)
			continue
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			fmt.Printf("  %s  a REAL directory — not removing it\n", t.Label)
			continue
		}
		points, _ := os.Readlink(link)
		abs := points
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(filepath.Dir(link), abs)
		}
		if name := skillDirName(abs); name != "" && name != skillLinkName {
			fmt.Printf("  %s  points at the %q skill, not %q — not removing it\n",
				t.Label, name, skillLinkName)
			continue
		}
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("removing %s: %w", link, err)
		}
		fmt.Printf("  %s  removed (the checkout at %s is untouched)\n", t.Label, points)
		removed++
	}
	if removed == 0 {
		fmt.Println("nothing to remove.")
	}
	return nil
}
