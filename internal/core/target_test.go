package core

import "testing"

func TestSplitJoinTarget(t *testing.T) {
	cases := []struct{ target, session, id string }{
		{"herdr-plugins/w2:p1", "herdr-plugins", "w2:p1"},
		{"crypto-desk/w1:p1", "crypto-desk", "w1:p1"},
		{"default/w1:t1", "default", "w1:t1"},
		{"w1:p1", "", "w1:p1"}, // unqualified: caller resolves the default
	}
	for _, c := range cases {
		s, id := SplitTarget(c.target)
		if s != c.session || id != c.id {
			t.Fatalf("SplitTarget(%q) = (%q,%q) want (%q,%q)", c.target, s, id, c.session, c.id)
		}
		if c.session != "" {
			if got := JoinTarget(c.session, c.id); got != c.target {
				t.Fatalf("JoinTarget(%q,%q) = %q want %q", c.session, c.id, got, c.target)
			}
		}
	}
}

// The whole point of namespacing: two sessions both mint w1:p1 and the two
// targets must stay distinct all the way down the wire.
func TestIdenticalPaneIDsInTwoSessionsDoNotCollide(t *testing.T) {
	a := JoinTarget("herdr-plugins", "w1:p1")
	b := JoinTarget("crypto-desk", "w1:p1")
	if a == b {
		t.Fatal("targets collided across sessions")
	}
	if SessionOf(a) != "herdr-plugins" || SessionOf(b) != "crypto-desk" {
		t.Fatalf("session routing wrong: %q %q", SessionOf(a), SessionOf(b))
	}
	if IDOf(a) != IDOf(b) || IDOf(a) != "w1:p1" {
		t.Fatalf("herdr-side ids wrong: %q %q", IDOf(a), IDOf(b))
	}
}

func TestStripSession(t *testing.T) {
	if got := StripSession("crypto-desk", "crypto-desk/w1:p1"); got != "w1:p1" {
		t.Fatalf("got %q", got)
	}
	// A param that is not qualified, or qualified by another session, is left
	// alone: we never mangle values we did not namespace.
	if got := StripSession("crypto-desk", "w1:p1"); got != "w1:p1" {
		t.Fatalf("got %q", got)
	}
	if got := StripSession("crypto-desk", "other/w1:p1"); got != "other/w1:p1" {
		t.Fatalf("got %q", got)
	}
}
