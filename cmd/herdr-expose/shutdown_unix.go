//go:build !windows

package main

import (
	"context"
	"os/signal"

	"github.com/muthuishere/herdr-expose/internal/platform"
)

// shutdownContext is cancelled when the operator, or whatever supervises us,
// asks the server to wind down. On Unix that is entirely signals.
func shutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, platform.ShutdownSignals()...)
}
