//go:build !windows

package claudeprovider

import (
	"context"
	"os/signal"
	"syscall"
)

func workerContext() (context.Context, context.CancelFunc) {
	// A closed parent stdout must become a write error so deferred native
	// cleanup runs, rather than SIGPIPE terminating the worker immediately.
	signal.Ignore(syscall.SIGPIPE)
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
}
