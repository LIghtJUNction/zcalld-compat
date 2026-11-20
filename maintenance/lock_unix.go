//go:build !windows

package maintenance

import (
	"errors"
	"os"
	"syscall"
)

func owned(_ string, info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}
func privateDirectory(path string, info os.FileInfo) bool {
	return owned(path, info) && info.Mode().Perm()&0077 == 0
}
func openLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
}
func tryLock(f *os.File) (func(), error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
