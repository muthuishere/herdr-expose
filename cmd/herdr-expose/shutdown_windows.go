//go:build windows

package main

import (
	"context"
	"os/signal"

	"github.com/muthuishere/herdr-expose/internal/platform"
)

// shutdownContext is cancelled when the operator asks the server to wind down.
//
// There was a second path here — a Service Control Manager stop, delivered over
// a control channel rather than as a signal — back when Windows installed an
// SCM Service. That whole mechanism is gone (see service_windows.go), so this
// is now exactly the Unix shape: console Ctrl-C, which Go delivers as
// os.Interrupt.
func shutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, platform.ShutdownSignals()...)
}
