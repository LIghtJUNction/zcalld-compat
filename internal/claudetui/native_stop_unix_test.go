//go:build darwin || linux

package claudetui

import (
	"os/signal"
	"syscall"
)

// Used only by the synthetic CLI to force the helper's kill-and-reap path.
func nativeIgnoreStop() { signal.Ignore(syscall.SIGTERM, syscall.SIGHUP) }
