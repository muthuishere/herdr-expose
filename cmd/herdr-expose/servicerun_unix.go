//go:build !windows

package main

// runUnderServiceManager exists because the Windows SCM starts its services
// with a control channel instead of a command line. launchd and systemd just
// exec the binary, so there is nothing to adapt here.
func runUnderServiceManager() (bool, error) { return false, nil }
