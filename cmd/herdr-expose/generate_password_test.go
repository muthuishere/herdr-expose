package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/herdr-expose/internal/serve"
)

// TestGeneratePasswordSetsLoginAndWritesCredential pins the whole flow:
// generate-password stores only an argon2id HASH in auth.json, writes the
// plaintext to a 0600 credential file, and the generated password then logs in.
func TestGeneratePasswordSetsLoginAndWritesCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	// Isolate config.Load()'s default path so the test never touches the real
	// per-user config dir.
	t.Setenv("HOME", t.TempDir())

	if err := cmdGeneratePassword(nil); err != nil {
		t.Fatalf("cmdGeneratePassword: %v", err)
	}

	// The credential file exists, is 0600, and carries a non-empty password line.
	credPath := filepath.Join(dir, credentialFileName)
	fi, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("stat credential file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file mode = %v, want 0600", perm)
	}
	body, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	pw := ""
	for _, line := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(line, "password="); ok {
			pw = v
		}
	}
	if pw == "" {
		t.Fatal("credential file has no non-empty password line")
	}

	// auth.json stores a HASH, never the generated plaintext.
	authBody, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if strings.Contains(string(authBody), pw) {
		t.Fatal("the generated plaintext appears in auth.json")
	}

	// The generated password actually logs in and mints a usable token.
	auth, err := serve.NewAuth(dir, newLogger(), nil)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	tok, _, err := auth.LoginWithPassword(pw, "phone", "10.0.0.2", "ua")
	if err != nil {
		t.Fatalf("LoginWithPassword with the generated password: %v", err)
	}
	if tok == "" {
		t.Fatal("login returned an empty token")
	}
}

// TestGeneratePasswordLengthAndFloor: --length widens the password, and a length
// under the SetPassword floor is refused before anything is written.
func TestGeneratePasswordLengthAndFloor(t *testing.T) {
	long, err := generatePassword(40)
	if err != nil {
		t.Fatalf("generatePassword: %v", err)
	}
	if len(long) != 40 {
		t.Fatalf("generated %d characters, want 40", len(long))
	}
	for _, c := range long {
		if !strings.ContainsRune(passwordAlphabet, c) {
			t.Fatalf("generated password contains %q, outside the alphabet", c)
		}
	}

	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	if err := cmdGeneratePassword([]string{"--length", "4"}); err == nil {
		t.Fatal("cmdGeneratePassword accepted a length below the floor")
	}
	if _, err := os.Stat(filepath.Join(dir, credentialFileName)); !os.IsNotExist(err) {
		t.Fatal("a refused generate-password still wrote a credential file")
	}
}
