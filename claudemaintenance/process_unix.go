//go:build !windows

package claudemaintenance

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func prepareProcess(cmd *exec.Cmd) (func(context.Context) error, func() error, func(time.Time) error, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stop := func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cleanup := func(deadline time.Time) error {
		if cmd.Process == nil {
			return nil
		}
		pid := -cmd.Process.Pid
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return errProcessDrain
		}
		// Signal delivery is not an exit acknowledgement. Confirm only this
		// owned group; the helper separately checks its own PTY session group.
		for {
			err := syscall.Kill(pid, 0)
			if err == syscall.ESRCH {
				return nil
			}
			if err != nil || !time.Now().Before(deadline) {
				return errProcessDrain
			}
			time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
		}
	}
	// POSIX exec may run before Start returns: cancellation here must never be
	// misreported as proof that a generation was not sent.
	var once sync.Once
	var finishErr error
	finish := func(deadline time.Time) error { once.Do(func() { finishErr = cleanup(deadline) }); return finishErr }
	return func(context.Context) error { return nil }, stop, finish, nil
}
