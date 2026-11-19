//go:build !windows

package community

import (
	"os"
	"syscall"
)

func platformLock(f *os.File) (func(), error) {
	if e := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		return nil, e
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
