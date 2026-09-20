//go:build !windows

package main

// notifyServerDown is a no-op on macOS and Linux: a LaunchAgent or a systemd
// --user unit has already restarted the server by the time a human could read
// a notification about it. Telling somebody to run a command that a supervisor
// is running for them is noise.
func notifyServerDown(string) {}
