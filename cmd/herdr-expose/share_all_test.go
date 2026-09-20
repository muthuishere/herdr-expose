package main

import (
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/core"
)

// `--all` is a SCOPE, not a rung. These tests pin the two properties that
// make it safe to have at all: it is the ABSENCE of a scope (never a weaker
// scope), and it cannot be combined with a scope pin.

func TestAllAndSessionPinAreMutuallyExclusive(t *testing.T) {
	cases := [][]string{
		{"--all", "--session", "crypto-desk"},
		{"--session", "crypto-desk", "--all"},
		{"--all", "--pane", "w1:p1"},
		{"--all", "--only-target", "crypto-desk/w1:p1"},
	}
	for _, args := range cases {
		_, err := parseShareFlags(args)
		if err == nil {
			t.Fatalf("%v: accepted --all together with a scope pin", args)
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("%v: unhelpful error: %v", args, err)
		}
	}
}

// An all-sessions share must resolve to a NIL *Scope — the ordinary
// unrestricted server (internal/core/scope.go). If this ever produced an
// Active() scope with some wildcard session name, every AllowsSession check
// would start comparing against a literal "all" and the share would serve
// nothing.
func TestAllScopeIsTheAbsenceOfAScope(t *testing.T) {
	f, err := parseShareFlags([]string{"--all"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.all {
		t.Fatal("--all did not set the flag")
	}
	scope, err := core.ParseScope("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Active() {
		t.Fatal("an unpinned scope reports itself as active")
	}
	if got := shareScopeLabel(f, scope); got != scopeAll {
		t.Fatalf("scope label = %q, want %q", got, scopeAll)
	}
	// And the enforcement mechanism is untouched for a SCOPED share.
	scoped, err := core.ParseScope("crypto-desk", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !scoped.Active() || scoped.AllowsSession("other") {
		t.Fatal("scope enforcement weakened for a scoped share")
	}
	if got := shareScopeLabel(shareFlags{}, scoped); got != "crypto-desk" {
		t.Fatalf("scoped label = %q", got)
	}
}

// `--all` composes with every rung, and never moves the rung itself.
func TestAllComposesWithEveryRung(t *testing.T) {
	cases := []struct {
		args []string
		want config.Rung
	}{
		{[]string{"--all"}, config.RungLAN},
		{[]string{"--all", "--lan"}, config.RungLAN},
		{[]string{"--all", "--local"}, config.RungLocal},
		{[]string{"--all", "--quick"}, config.RungQuick},
		{[]string{"--all", "--domain", "x.example.com"}, config.RungDomain},
	}
	for _, c := range cases {
		f, err := parseShareFlags(c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !f.all {
			t.Fatalf("%v: --all lost", c.args)
		}
		if got := f.rung(); got != c.want {
			t.Fatalf("%v: rung = %v, want %v", c.args, got, c.want)
		}
	}
}

// The record on disk is what `share list`, the sweeper, the restorer and
// teardown all read. An all-sessions share must be recognisable from it alone.
func TestAllShareRecordRoundTrips(t *testing.T) {
	withStateDir(t)
	dir := seedShare(t, "all1", &shareRecord{
		Mode: string(config.ModeLAN), Scope: scopeAll, Port: 21999,
		State: "ready", ExpiresAt: time.Now().Add(time.Hour),
	})
	rec, err := readShareFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.isAll() {
		t.Fatal("an all-sessions record did not read back as one")
	}
	if got := rec.view().Scope; got != scopeAll {
		t.Fatalf("view scope = %q, want %q", got, scopeAll)
	}

	scoped := seedShare(t, "one1", &shareRecord{
		Mode: string(config.ModeLAN), Session: "crypto-desk", Scope: "crypto-desk",
		Port: 21998, State: "ready", ExpiresAt: time.Now().Add(time.Hour),
	})
	srec, err := readShareFile(scoped)
	if err != nil {
		t.Fatal(err)
	}
	if srec.isAll() {
		t.Fatal("a scoped record claimed to be an all-sessions share")
	}

	// A record with no session that never asked for `all` must NOT be served
	// unscoped — the run path refuses it rather than guessing.
	broken := &shareRecord{ID: "bad1", Mode: string(config.ModeLAN), Scope: ""}
	if broken.isAll() {
		t.Fatal("a malformed record was treated as an all-sessions share")
	}
}

// `extend` never takes --all: silently ignoring it is how somebody believes
// they extended every share.
func TestExtendRejectsAll(t *testing.T) {
	withStateDir(t)
	err := cmdShareExtend([]string{"abc12345", "--all", "--hours", "1"})
	if err == nil || !strings.Contains(err.Error(), "--all") {
		t.Fatalf("extend --all: %v", err)
	}
}
