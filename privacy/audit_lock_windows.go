//go:build windows

package privacy

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func auditLock(f *os.File, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	handle := windows.Handle(f.Fd())
	overlap := new(windows.Overlapped)
	for {
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlap)
		if err == nil {
			return func() { _ = windows.UnlockFileEx(handle, 0, 1, 0, overlap) }, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errAudit
		}
		if !time.Now().Before(deadline) {
			return nil, errAudit
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}
