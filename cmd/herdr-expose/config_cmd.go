package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/serve"
)

// cmdConfig mirrors `herdr --default-config`: the tool can always show you the
// complete, commented configuration it understands, so the answer to "what can
// I set?" is a command rather than a trawl through the source.
func cmdConfig(args []string) error {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "print-default", "default", "print":
		// Exactly what a first run writes. Safe to redirect into a file.
		fmt.Print(config.DefaultFileContents())
		return nil
	case "path":
		p, err := config.DefaultPath()
		if err != nil {
			return err
		}
		fmt.Println(p)
		return nil
	case "show", "cat":
		p, err := config.DefaultPath()
		if err != nil {
			return err
		}
		// Load first: it creates the file on a first run and appends any
		// section a newer release added, so `config show` never shows a config
		// that is missing keys the binary understands.
		cfg, err := config.LoadFrom(p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(cfg.Path())
		if err != nil {
			return err
		}
		os.Stdout.Write(body)
		return nil
	case "edit":
		p, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if _, err := config.LoadFrom(p); err != nil {
			// A config too broken to load is exactly when you want to edit it.
			fmt.Fprintln(os.Stderr, "herdr-expose: config does not currently load:", err)
		}
		return editFile(p)
	case "set-password":
		return cmdSetPassword()
	case "generate-password":
		return cmdGeneratePassword(args[1:])
	case "clear-password":
		return cmdClearPassword()
	case "help", "--help", "-h":
		configUsage()
		return nil
	}
	configUsage()
	return fmt.Errorf("unknown config subcommand %q", sub)
}

func configUsage() {
	fmt.Fprint(os.Stderr, `herdr-expose config — the self-documenting configuration

  config print-default   print the COMPLETE commented config with every default
  config path            print the config file path
  config show            print the live config file (creating it if absent)
  config edit            open it in $VISUAL / $EDITOR
  config set-password    set a login password (read from a no-echo prompt, or
                         from stdin when piped — NEVER from an argument)
  config generate-password [--length N]
                         generate a strong random password, set it, and write
                         the PLAINTEXT to a 0600 file in the state dir to read
                         once and delete (default length 22, ~128 bits)
  config clear-password  remove the login password, disabling /v1/login

First run writes every section and every key, including the subsystems that are
nobody would guess at ([share], [log]) — a key that exists but is not written
is a key nobody will ever find. A section added by a later release is APPENDED
to your file; nothing you have already edited is rewritten or reordered.

The login password is the SECOND, weaker credential: the exposed web app can
exchange it for a device token instead of a locally-displayed pairing code. It
lives hashed in the 0600 state file, never in this config, so it is set with a
command rather than an editor.
`)
}

func editFile(path string) error {
	ed := strings.TrimSpace(os.Getenv("VISUAL"))
	if ed == "" {
		ed = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if ed == "" {
		switch runtime.GOOS {
		case "darwin":
			ed = "open -t"
		case "windows":
			// notepad is the only editor guaranteed to be present, and `vi`
			// is guaranteed NOT to be.
			ed = "notepad"
		default:
			ed = "vi"
		}
	}
	parts := strings.Fields(ed)
	cmd := exec.Command(parts[0], append(parts[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// cmdSetPassword stores the login password, the weaker second credential
// (ADR 0039). The password is read from a no-echo terminal prompt, or from
// stdin when it is piped, and NEVER from argv: an argument is visible in `ps`
// and in shell history, which is exactly where a credential must not be.
func cmdSetPassword() error {
	state, err := StateDir()
	if err != nil {
		return err
	}
	pw, err := readSecret("new login password: ")
	if err != nil {
		return err
	}
	auth, err := serve.NewAuth(state, newLogger(), nil)
	if err != nil {
		return err
	}
	if err := auth.SetPassword(pw); err != nil {
		return err
	}
	// No echo of the password, here or in the log — only that one is now set.
	fmt.Println("login password set. The exposed web app can log in with it now; " +
		"no restart is needed. Remove it with `herdr-expose config clear-password`.")
	return nil
}

// cmdClearPassword removes the login password, disabling /v1/login.
func cmdClearPassword() error {
	state, err := StateDir()
	if err != nil {
		return err
	}
	auth, err := serve.NewAuth(state, newLogger(), nil)
	if err != nil {
		return err
	}
	if err := auth.ClearPassword(); err != nil {
		return err
	}
	fmt.Println("login password cleared. /v1/login is now closed; pairing is unaffected.")
	return nil
}

// readSecret reads one secret with echo OFF when stdin is a terminal, or the
// first line of stdin when it is piped (a scripted set-password). It never takes
// the value from an argument.
func readSecret(prompt string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", errors.New("no password on stdin")
	}
	return strings.TrimSpace(sc.Text()), nil
}

const (
	// defaultGeneratedPasswordLen is 22 characters over passwordAlphabet, i.e.
	// ~128 bits (log2(58) ≈ 5.86 bits/char × 22 ≈ 129). A generated password is
	// read once off disk, never typed, so there is no reason to make it short.
	defaultGeneratedPasswordLen = 22
	// passwordAlphabet is base58: no 0/O, 1/l/I, so a human reading the value out
	// of the credential file cannot misread it.
	passwordAlphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	// credentialFileName is where generate-password leaves the plaintext, in the
	// state dir beside auth.json — outside the repo (ADR 0009), 0600.
	credentialFileName = "login-credential.txt"
)

// generatePassword returns n characters drawn uniformly from passwordAlphabet
// using crypto/rand. Bytes in the biased tail (there is one because 256 is not a
// multiple of the alphabet size) are rejected and redrawn, so the distribution
// is exactly uniform rather than merely close.
func generatePassword(n int) (string, error) {
	const max = 256 - (256 % len(passwordAlphabet)) // reject bytes >= max
	out := make([]byte, 0, n)
	buf := make([]byte, 1)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if int(buf[0]) >= max {
			continue
		}
		out = append(out, passwordAlphabet[int(buf[0])%len(passwordAlphabet)])
	}
	return string(out), nil
}

// cmdGeneratePassword mints a strong random password, sets it (so only the
// argon2id hash reaches auth.json, identical to set-password), and writes the
// PLAINTEXT to a 0600 credential file for the operator to read once and delete.
// The tradeoff — plaintext on disk — is why the warning is loud and why the
// value NEVER touches stdout or the log.
func cmdGeneratePassword(args []string) error {
	length := defaultGeneratedPasswordLen
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--length", "-n":
			if i+1 >= len(args) {
				return errors.New("--length needs a number")
			}
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil || v <= 0 {
				return fmt.Errorf("--length %q is not a positive number", args[i])
			}
			length = v
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}
	// The same floor SetPassword enforces, checked here so the error names the
	// flag rather than surfacing from inside auth.
	if length < serve.MinPasswordLen {
		return fmt.Errorf("--length must be at least %d", serve.MinPasswordLen)
	}
	state, err := StateDir()
	if err != nil {
		return err
	}
	pw, err := generatePassword(length)
	if err != nil {
		return err
	}
	auth, err := serve.NewAuth(state, newLogger(), nil)
	if err != nil {
		return err
	}
	if err := auth.SetPassword(pw); err != nil {
		return err
	}
	credPath, err := writeCredentialFile(state, pw)
	if err != nil {
		return err
	}
	// The password NEVER appears on stdout or in the log — only where to read it,
	// and that it must then be deleted.
	fmt.Println("login password generated and set (argon2id hash in auth.json).")
	fmt.Printf("plaintext written to %s (mode 0600)\n", credPath)
	fmt.Println("WARNING: read it, use it, then delete that file — the plaintext on disk is the tradeoff.")
	return nil
}

// writeCredentialFile writes the generated plaintext to the state dir, 0600,
// atomically (tmp + rename). It is the ONLY place a generated password is kept
// in the clear, and it exists so a human can read a password they never typed.
func writeCredentialFile(state, password string) (string, error) {
	path := filepath.Join(state, credentialFileName)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# herdr-expose login password — generated %s\n", time.Now().Format(time.RFC3339))
	if cfg, err := config.Load(); err == nil {
		fmt.Fprintf(&b, "# server port: %d\n", cfg.Port())
	}
	fmt.Fprintln(&b, "# read this, use it, then DELETE this file.")
	fmt.Fprintf(&b, "password=%s\n", password)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// cmdLogs reads the daemon's log, or a share's own log.
//
// This is the hole it fills: `herdr-expose daemon` forks and detaches, and
// until now the only way to find out what it did was to already know where
// `serve` redirected its output. A detached process whose output nobody can
// find is a process nobody can debug.
func cmdLogs(args []string) error {
	follow := false
	asJSON := false
	share := ""
	showPath := false
	n := 50
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--follow", "-f":
			follow = true
		case "--json":
			asJSON = true
		case "--share":
			if i+1 >= len(args) {
				return errors.New("--share needs a share id")
			}
			i++
			share = args[i]
		case "-n", "--lines":
			if i+1 >= len(args) {
				return errors.New("-n needs a number")
			}
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil || v < 0 {
				return fmt.Errorf("-n %q is not a line count", args[i])
			}
			n = v
		case "--path":
			showPath = true
		case "help", "--help", "-h":
			fmt.Fprint(os.Stderr, `herdr-expose logs [--follow|-f] [-n N] [--share ID] [--json] [--path]

  read the daemon's log; --share ID reads that share's own log instead.
  --json emits one JSON object per line (parsed when the log is text).
  --path prints the file it would read and exits.
  Where the file lives is the [log] file key; it rotates at [log] max_size_mb.
`)
			return nil
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}

	path, err := resolveLogPath(share)
	if err != nil {
		return err
	}
	if showPath {
		fmt.Println(path)
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			if share != "" {
				return fmt.Errorf("share %s has no log at %s (is the share still alive? `herdr-expose share list`)", share, path)
			}
			return fmt.Errorf("no log at %s yet — the daemon writes it on first start "+
				"(set `file` under [log] to put it somewhere else)", path)
		}
		return err
	}
	defer f.Close()

	emit := func(line string) { fmt.Println(line) }
	if asJSON {
		emit = func(line string) {
			if strings.HasPrefix(strings.TrimSpace(line), "{") {
				fmt.Println(line)
				return
			}
			b, _ := json.Marshal(map[string]string{"line": line})
			fmt.Println(string(b))
		}
	}

	if err := printLastLines(f, n, emit); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	// Tail. Deliberately dumb polling: the interesting failure is a daemon
	// that has STOPPED writing, and a poll shows that just as well as a watch.
	// It also survives the file being rotated out from under it, which is the
	// one thing a naive fd-based watch does not.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for {
		for sc.Scan() {
			emit(sc.Text())
		}
		if err := sc.Err(); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
		// Rotation: the path now points at a new, shorter file.
		if st, serr := os.Stat(path); serr == nil {
			if pos, perr := f.Seek(0, io.SeekCurrent); perr == nil && st.Size() < pos {
				nf, oerr := os.Open(path)
				if oerr == nil {
					f.Close()
					f = nf
					sc = bufio.NewScanner(f)
					sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
					continue
				}
			}
		}
		sc = bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	}
}

// resolveLogPath is the ONE place that decides which file to read, and it is
// the same function the daemon writes through.
func resolveLogPath(share string) (string, error) {
	if share != "" {
		dir, err := shareDirFor(share)
		if err != nil {
			return "", err
		}
		return shareLogPath(dir), nil
	}
	state, err := StateDir()
	if err != nil {
		return "", err
	}
	cfg, err := config.Load()
	if err != nil {
		// A broken config must not hide the log that would explain why.
		return filepath.Join(state, config.DefaultLogFileName), nil
	}
	return logFilePath(cfg, state), nil
}

// printLastLines writes the final n lines and leaves the file positioned at
// the end, ready for --follow.
func printLastLines(f *os.File, n int, emit func(string)) error {
	if n == 0 {
		_, err := f.Seek(0, io.SeekEnd)
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// Read at most the last megabyte: a rotated log can still be large and the
	// last 50 lines are never a megabyte away.
	const window = 1 << 20
	start := st.Size() - window
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ring := make([]string, 0, n)
	for sc.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for _, l := range ring {
		emit(l)
	}
	_, err = f.Seek(0, io.SeekEnd)
	return err
}
