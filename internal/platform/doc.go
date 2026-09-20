// Package platform is the OS seam.
//
// Everything herdr-expose does that has no portable spelling lives here, in
// build-tagged files, ONE concept per pair: dialling the Herdr control socket,
// taking an advisory file lock, owning a spawned process tree, asking whether a
// pid is alive, and deciding where per-user state belongs.
//
// The rule is a small shared signature and two readable implementations, never
// a `runtime.GOOS` switch inside the caller. The Unix path is the one that has
// been in production; it is written here exactly as it was written inline, so
// porting to Windows cannot have changed its behaviour.
package platform
