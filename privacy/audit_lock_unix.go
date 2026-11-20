//go:build !windows

package privacy

import (
	"errors"
	"os"
	"syscall"
	"time"
)

func auditLock(f *os.File, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, errAudit
		}
		if !time.Now().Before(deadline) {
			return nil, errAudit
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}
