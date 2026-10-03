package main

import (
	"os"
	"testing"
)

// A supervised unit's stderr is the journal. Printing the server token there
// writes a live credential into a log that outlives the process and is
// readable by anyone who can read the journal -- which docs/guide.md promises
// never happens. The banner is suppressed by the supervisor's own marker, so
// the check does not depend on how the manager happens to wire up fd 2.
func TestTokenIsNotPrintedWhenSupervised(t *testing.T) {
	t.Setenv("HERDR_EXPOSE_SUPERVISED", "1")
	if tokenPrintable() {
		t.Fatal("would print the server token into the journal")
	}
}

// Under a test harness stderr is a pipe, not a terminal, so this also pins the
// character-device half of the test: no human, no token.
func TestTokenIsNotPrintedWithoutATerminal(t *testing.T) {
	t.Setenv("HERDR_EXPOSE_SUPERVISED", "")
	st, err := os.Stderr.Stat()
	if err != nil {
		t.Skip("cannot stat stderr")
	}
	if st.Mode()&os.ModeCharDevice != 0 {
		t.Skip("stderr really is a terminal here")
	}
	if tokenPrintable() {
		t.Fatal("would print the server token to a non-terminal")
	}
}

// And rotate refuses rather than printing into a pipe a caller may be logging.
func TestTokenRotateRefusesWithoutATerminal(t *testing.T) {
	t.Setenv("HERDR_EXPOSE_SUPERVISED", "1")
	if err := cmdToken([]string{"rotate"}); err == nil {
		t.Fatal("rotate printed a token with no terminal")
	}
	if err := cmdToken(nil); err == nil {
		t.Fatal("rotate with no subcommand should be a usage error")
	}
}
