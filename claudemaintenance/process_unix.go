//go:build !windows

package claudemaintenance

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func prepareProcess(cmd *exec.Cmd) (func(context.Context) error, func() error, func(), error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stop := func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cleanup := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	// POSIX exec may run before Start returns: cancellation here must never be
	// misreported as proof that a generation was not sent.
	return func(context.Context) error { return nil }, stop, cleanup, nil
}
