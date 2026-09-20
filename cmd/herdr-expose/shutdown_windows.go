//go:build windows

package main

import (
	"context"
	"os/signal"
	"sync"

	"github.com/muthuishere/herdr-expose/internal/platform"
)

// On Windows there is a SECOND way to be told to stop, and it is not a signal:
// the Service Control Manager sends SERVICE_CONTROL_STOP over a control
// channel. A `serve` running as a Service would otherwise ignore it and be
// killed after the SCM's timeout, which turns every restart into a hard kill
// and every clean shutdown path (tunnel teardown, share revocation) into dead
// code. So the shutdown context listens to both.
var (
	scmStopOnce sync.Once
	scmStopCh   = make(chan struct{})
)

// signalSCMStop is called by the service control handler. Idempotent.
func signalSCMStop() { scmStopOnce.Do(func() { close(scmStopCh) }) }

// shutdownContext is cancelled by a console Ctrl-C OR by the SCM.
func shutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, platform.ShutdownSignals()...)
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-scmStopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(); stop() }
}
