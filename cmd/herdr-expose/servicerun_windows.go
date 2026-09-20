//go:build windows

package main

// runUnderServiceManager hands control to the SCM protocol when the SCM is who
// started us, and returns false when a human did.
func runUnderServiceManager() (bool, error) { return runWindowsServiceIfManaged() }
