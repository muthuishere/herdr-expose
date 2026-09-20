package main

import (
	"errors"
	"strings"
	"testing"
)

// The bug this guards: doctor used to ask only the host stub resolver, which on
// macOS caches the NXDOMAIN it got seconds before the record existed. It then
// printed `dns: FAIL` beside `exposure: PASS` for a tunnel that was serving
// traffic. Public authority decides whether the record exists; the local stub
// only decides whether this machine can see it yet, and that difference must be
// NAMED, not collapsed into a pass or a fail.
func TestClassifyDNS(t *testing.T) {
	nx := errors.New("no such host")

	cases := []struct {
		name     string
		pub      []string
		pubErr   error
		local    []string
		localErr error
		want     checkStatus
		mustSay  string
	}{
		{"both agree", []string{"104.21.1.1"}, nil, []string{"104.21.1.1"}, nil, statusPass, "agree"},
		{"order differs is still agreement", []string{"a", "b"}, nil, []string{"b", "a"}, nil, statusPass, "agree"},
		{"stale negative cache", []string{"104.21.1.1"}, nil, nil, nx, statusWarn, "resolves publicly"},
		{"answers differ", []string{"104.21.1.1"}, nil, []string{"10.0.0.9"}, nil, statusWarn, "instead"},
		{"local only", nil, nx, []string{"10.0.0.9"}, nil, statusWarn, "only locally"},
		{"nowhere", nil, nx, nil, nx, statusFail, "does not resolve"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDNS("x.example.com", tc.pub, tc.pubErr, tc.local, tc.localErr)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q (detail: %s)", got.Status, tc.want, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.mustSay) {
				t.Fatalf("detail %q does not say %q", got.Detail, tc.mustSay)
			}
			if got.Status != statusPass && got.Hint == "" {
				t.Fatalf("a non-pass verdict must carry a hint that names the fix")
			}
		})
	}
}

// A working tunnel must never be reported as broken just because this machine's
// resolver is behind.
func TestStaleLocalCacheIsNotAFailure(t *testing.T) {
	got := classifyDNS("herdr.example.com", []string{"104.21.1.1"}, nil, nil, errors.New("NXDOMAIN"))
	if got.Status == statusFail {
		t.Fatal("a hostname that resolves publicly must not FAIL doctor")
	}
	if !strings.Contains(got.Hint, "clears on its own") {
		t.Fatalf("hint should tell the user this is self-healing, got %q", got.Hint)
	}
}
