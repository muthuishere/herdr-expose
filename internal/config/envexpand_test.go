package config

import (
	"slices"
	"testing"
)

func TestExpandEnvForms(t *testing.T) {
	t.Setenv("HX_TOKEN", "s3cret-value-long-enough")
	t.Setenv("HX_ID", "12345")

	for _, tc := range []struct {
		name, in, want string
	}{
		{"bare", "$HX_TOKEN", "s3cret-value-long-enough"},
		{"braced", "${HX_TOKEN}", "s3cret-value-long-enough"},
		{"embedded", "bot$HX_TOKEN/send", "bots3cret-value-long-enough/send"},
		{"braced adjacent to text", "id=${HX_ID}x", "id=12345x"},
		{"two refs", "$HX_ID:$HX_ID", "12345:12345"},
		// $$ is the escape, so a literal dollar survives.
		{"escaped dollar", "costs $$5", "costs $5"},
		// A bare $ that is not a reference must not be eaten: a template or a
		// price in a config value has to come out the other side unharmed.
		{"lone dollar", "costs $ 5", "costs $ 5"},
		{"dollar then digit", "$5", "$5"},
		{"no refs", "plain", "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := ExpandEnv(tc.in)
			if got != tc.want {
				t.Fatalf("ExpandEnv(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A missing variable expands to empty AND is reported. Expanding to the
// literal "$TOKEN" would send that string to an API as a credential and
// produce a baffling 401; expanding to empty in silence produces an adapter
// that does nothing for no visible reason. Neither is acceptable alone.
func TestExpandEnvReportsMissingAndDoesNotLeakTheName(t *testing.T) {
	got, found, missing := ExpandEnv("prefix-$HX_DEFINITELY_NOT_SET-suffix")
	if got != "prefix--suffix" {
		t.Errorf("expanded to %q; a missing var must become empty, not its own name", got)
	}
	if len(found) != 0 {
		t.Errorf("found = %v, want none", found)
	}
	if !slices.Contains(missing, "HX_DEFINITELY_NOT_SET") {
		t.Errorf("missing = %v, want it to name the unset variable", missing)
	}
}

// Only a value that CAME FROM the environment is a secret. A literal written
// in the config file is not: redacting it would scrub ordinary words -- a chat
// id, a channel name -- out of every log line that happened to contain them.
func TestResolveEnvTreatsOnlyEnvValuesAsSecret(t *testing.T) {
	t.Setenv("HX_TOKEN", "s3cret-value-long-enough")
	a := ChatAdapter{ID: "example", Env: map[string]string{
		"token":   "$HX_TOKEN",
		"chat_id": "12345",
		"absent":  "$HX_NOT_SET",
	}}
	r := a.ResolveEnv()

	if r.Values["token"] != "s3cret-value-long-enough" {
		t.Errorf("token = %q, want the expanded value", r.Values["token"])
	}
	if r.Values["chat_id"] != "12345" {
		t.Errorf("chat_id = %q, want the literal", r.Values["chat_id"])
	}
	if !slices.Contains(r.Secrets, "s3cret-value-long-enough") {
		t.Errorf("secrets = %v, want the env-sourced value", r.Secrets)
	}
	if slices.Contains(r.Secrets, "12345") {
		t.Error("a literal from the config file was marked secret")
	}
	// An unset reference expands to empty, and empty must never be registered
	// as a secret: redacting "" would scrub every character of every log line.
	if slices.Contains(r.Secrets, "") {
		t.Error("empty string registered as a secret")
	}
	if got := r.Missing["absent"]; len(got) != 1 || got[0] != "HX_NOT_SET" {
		t.Errorf("Missing[absent] = %v, want [HX_NOT_SET]", got)
	}
	if len(r.MissingReport()) != 1 {
		t.Errorf("MissingReport() = %v, want one line", r.MissingReport())
	}
}
