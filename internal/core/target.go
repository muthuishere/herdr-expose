package core

import "strings"

// TargetSep separates the session name from the Herdr id in a wire target.
//
// Every id a client sees is SESSION-QUALIFIED: `herdr-plugins/w2:p1`, not
// `w2:p1`. Bare pane ids are ambiguous the moment a second session exists,
// because every Herdr session starts its own id space at w1:p1 — two sessions
// WILL both have `w1:p1` and a bare target would route a keystroke to whichever
// socket happened to be looked up first.
//
// `/` is safe as the separator: a session name is a directory name under
// ~/.config/herdr/sessions/ and cannot contain one, while Herdr ids are
// `w<N>[:t<N>|:p<N>]` and never do either.
const TargetSep = "/"

// SplitTarget splits a wire target into its session and its Herdr id.
// An unqualified target returns an empty session, which callers resolve to the
// default session — that keeps a v1 client (and a hand-typed CLI probe) working.
func SplitTarget(target string) (session, id string) {
	if i := strings.Index(target, TargetSep); i >= 0 {
		return target[:i], target[i+len(TargetSep):]
	}
	return "", target
}

// JoinTarget builds a wire target from a session and a Herdr id.
func JoinTarget(session, id string) string {
	if session == "" || id == "" {
		return id
	}
	return session + TargetSep + id
}

// SessionOf is the session part of a target ("" when unqualified).
func SessionOf(target string) string {
	s, _ := SplitTarget(target)
	return s
}

// IDOf is the Herdr-side id of a target, with any session prefix removed.
func IDOf(target string) string {
	_, id := SplitTarget(target)
	return id
}

// StripSession removes a leading "<session>/" from a value, if present. Used
// when passing client-supplied params through to a session's socket: Herdr
// knows nothing about our namespacing.
func StripSession(session, value string) string {
	if session != "" && strings.HasPrefix(value, session+TargetSep) {
		return value[len(session)+len(TargetSep):]
	}
	return value
}
