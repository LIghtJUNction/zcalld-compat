//go:build windows

package community

import (
	"golang.org/x/sys/windows"
	"os"
)

func platformLock(f *os.File) (func(), error) {
	handle := windows.Handle(f.Fd())
	overlap := new(windows.Overlapped)
	if e := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlap); e != nil {
		return nil, e
	}
	return func() { _ = windows.UnlockFileEx(handle, 0, 1, 0, overlap) }, nil
}
